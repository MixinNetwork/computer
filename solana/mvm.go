package solana

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	mc "github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/mixin/util/base58"
	"github.com/MixinNetwork/safe/apps/mixin"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
)

const (
	ConfirmFlagNonceAvailable = 0
	ConfirmFlagNonceExpired   = 1

	FlagWithPostProcess = 0
	FlagSkipPostProcess = 1
)

func (node *Node) processAddUser(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleUser {
		panic(req.Role)
	}
	if req.Action != OperationTypeAddUser {
		panic(req.Action)
	}

	plan, err := node.store.ReadLatestOperationParams(ctx, req.CreatedAt)
	if err != nil {
		panic(err)
	}
	if plan == nil ||
		!plan.OperationPriceAmount.IsPositive() ||
		req.AssetId != plan.OperationPriceAsset ||
		req.Amount.Cmp(plan.OperationPriceAmount) < 0 {
		return node.failRequest(ctx, req, "")
	}

	mix := string(req.ExtraBytes())
	_, err = bot.NewMixAddressFromString(mix)
	logger.Printf("common.NewAddressFromString(%s) => %v", mix, err)
	if err != nil {
		return node.failRequest(ctx, req, "")
	}

	old, err := node.store.ReadUserByMixAddress(ctx, mix)
	logger.Printf("store.ReadUserByAddress(%s) => %v %v", mix, old, err)
	if err != nil {
		panic(fmt.Errorf("store.ReadUserByAddress(%s) => %v", mix, err))
	} else if old != nil {
		return node.failRequest(ctx, req, "")
	}

	id, err := node.store.GetNextUserId(ctx)
	logger.Printf("store.GetNextUserId() => %s %v", id.String(), err)
	if err != nil {
		panic(err)
	}
	master, err := node.store.ReadLatestPublicKey(ctx)
	logger.Printf("store.ReadLatestPublicKey() => %s %v", master, err)
	if err != nil || master == "" {
		panic(fmt.Errorf("store.ReadLatestPublicKey() => %s %v", master, err))
	}
	public := mixin.DeriveEd25519Child(master, id.FillBytes(make([]byte, 8)))
	chainAddress := solana.PublicKeyFromBytes(public[:]).String()

	err = node.store.WriteUserWithRequest(ctx, req, id.String(), mix, chainAddress, master)
	if err != nil {
		panic(fmt.Errorf("store.WriteUserWithRequest(%v %s) => %v", req, mix, err))
	}
	return nil, ""
}

func (node *Node) processUserDeposit(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleUser {
		panic(req.Role)
	}
	if req.Action != OperationTypeUserDeposit {
		panic(req.Action)
	}

	data := req.ExtraBytes()
	if len(data) != 8 {
		logger.Printf("invalid extra length of request for user deposit: %d", len(data))
		return node.failRequest(ctx, req, "")
	}
	id := new(big.Int).SetBytes(data[:8])
	user, err := node.store.ReadUser(ctx, id.String())
	logger.Printf("store.ReadUser(%d) => %v %v", id, user, err)
	if err != nil {
		panic(fmt.Errorf("store.ReadUser() => %v", err))
	} else if user == nil {
		return node.failRequest(ctx, req, "")
	}

	asset, err := common.SafeReadAssetUntilSufficient(ctx, req.AssetId)
	if err != nil || asset == nil {
		panic(err)
	}

	output := &store.UserOutput{
		OutputId:        req.Output.OutputId,
		UserId:          user.UserId,
		TransactionHash: req.Output.TransactionHash,
		OutputIndex:     req.Output.OutputIndex,
		AssetId:         req.AssetId,
		ChainId:         asset.ChainID,
		Amount:          req.Amount.String(),
		State:           common.RequestStateInitial,
		CreatedAt:       req.CreatedAt,
		UpdatedAt:       req.CreatedAt,
	}
	err = node.store.WriteUserDepositWithRequest(ctx, req, output)
	if err != nil {
		panic(err)
	}

	return nil, ""
}

// System call operation full lifecycle:
//
//  1. user creates system call with locked nonce
//     memo: user id (8 bytes) | call id (16 bytes) | skip post-process flag (1 byte) | fee id (16 bytes if needed)
//     if memo includes the fee id and mtg receives extra amount of XIN (> 0.001), same value of SOL would be tranfered to user account in prepare system call.
//     processSystemCall
//     (state: initial, withdrawal_traces: NULL, signature: NULL)
//
//  2. observer confirms nonce available and creates prepare system call to transfer assets to user account in advance
//     mvm creates withdrawal txs and makes sign requests for user system call and prepare system call
//     processConfirmNonce
//     (user    system call, state: pending, withdrawal_traces: NOT NULL, signature: NULL)
//     (prepare system call, state: pending, withdrawal_traces: "",       signature: NULL)
//
//     1). observer requests to regenerate signatures for system calls if timeout
//     processObserverRequestSign
//
//     2). mtg generate signatures for system calls
//     processSignerSignatureResponse
//     (user    system call, signature: NOT NULL)
//     (prepare system call, signature: NOT NULL)
//
//  3. observer pays the withdrawal fees
//
//  4. observer runs prepare system call and user system call in a row if withdrawals of user system call are all confirmed,
//     builds post-process system call to transfer solana assets to mtg deposit entry and burn external assets if needed,
//     then confirms the two calls successful in one request to mtg with the post-process call.
//     mtg would mark the prepare and user system call as done, and makes sign requests for post-process system call
//     processConfirmCall
//     (prepare      system call, state: done,    hash: NOT NULL)
//     (user         system call, state: done,    hash: NOT NULL)
//     (post-process system call, state: pending, signature: NULL)
//
//     1). mtg generate signatures for post-process system call
//     processSignerSignatureResponse
//     (post-process system call, signature: NOT NULL)
//
//  5. observer runs, confirms post-process call successfully
//     (post-process system call, state: done)
func (node *Node) processSystemCall(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleUser {
		panic(req.Role)
	}
	if req.Action != OperationTypeSystemCall {
		panic(req.Action)
	}

	data := req.ExtraBytes()
	if len(data) != 25 && len(data) != 41 { // because a fee id for observer usage
		logger.Printf("invalid extra length of request to create system call: %d", len(data))
		return node.failRequest(ctx, req, "")
	}
	id := new(big.Int).SetBytes(data[:8])
	user, err := node.store.ReadUser(ctx, id.String())
	logger.Printf("store.ReadUser(%d) => %v %v", id, user, err)
	if err != nil {
		panic(fmt.Errorf("store.ReadUser() => %v", err))
	} else if user == nil {
		return node.failRequest(ctx, req, "")
	}
	mix, err := bot.NewMixAddressFromString(user.MixAddress)
	if err != nil {
		panic(err)
	}
	if !slices.ContainsFunc(mix.Members(), func(m string) bool {
		return slices.Contains(req.Output.Senders, m)
	}) && !common.CheckTestEnvironment(ctx) {
		// TODO use better and general authentication without MM api
		return node.failRequest(ctx, req, "")
	}

	os, storage, err := node.GetSystemCallReferenceOutputs(ctx, user.UserId, req.MixinHash.String(), common.RequestStateInitial)
	logger.Printf("node.GetSystemCallReferenceTxs(%s) => %v %v %v", req.MixinHash.String(), os, storage, err)
	if err != nil || storage == nil {
		return node.failRequest(ctx, req, "")
	}

	cid := uuid.Must(uuid.FromBytes(data[8:24])).String()
	skipPostProcess := false
	switch data[24] {
	case FlagSkipPostProcess:
		skipPostProcess = true
	case FlagWithPostProcess:
	default:
		logger.Printf("invalid skip post process flag: %d", data[24])
		return node.failRequest(ctx, req, "")
	}

	plan, err := node.store.ReadLatestOperationParams(ctx, req.CreatedAt)
	if err != nil {
		panic(err)
	}
	if plan == nil ||
		!plan.OperationPriceAmount.IsPositive() ||
		req.AssetId != plan.OperationPriceAsset ||
		req.Amount.Cmp(plan.OperationPriceAmount) < 0 {
		return node.failRequest(ctx, req, "")
	}

	rb := node.readStorageExtraFromObserver(ctx, *storage)
	call, tx, err := node.buildSystemCallFromBytes(ctx, req, cid, rb, false)
	if err != nil {
		return node.failRequest(ctx, req, "")
	}
	call.Superior = call.RequestId
	call.Type = store.CallTypeMain
	call.Public = hex.EncodeToString(user.FingerprintWithPath())
	call.SkipPostProcess = skipPostProcess

	err = node.checkUserSystemCall(ctx, tx)
	if err != nil {
		logger.Printf("node.checkUserSystemCall(%v) => %v", tx, err)
		return node.failRequest(ctx, req, "")
	}

	err = node.store.WriteInitialSystemCallWithRequest(ctx, req, call, os)
	logger.Printf("solana.WriteInitialSystemCallWithRequest(%v %d) => %v", call, len(os), err)
	if err != nil {
		panic(err)
	}

	return nil, ""
}

func (node *Node) processConfirmNonce(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeConfirmNonce {
		panic(req.Action)
	}

	extra := req.ExtraBytes()
	flag, extra := extra[0], extra[1:]
	callId := uuid.Must(uuid.FromBytes(extra[0:16])).String()

	call, err := node.store.ReadSystemCallByRequestId(ctx, callId, 0)
	logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", callId, call, err)
	if err != nil {
		panic(err)
	}
	if call == nil || call.WithdrawalTraces.Valid {
		return node.failRequest(ctx, req, "")
	}
	// call maybe be failed when re-processing output after compaction
	outputState := common.RequestStatePending
	switch call.State {
	case common.RequestStateInitial:
	case common.RequestStateFailed:
		outputState = common.RequestStateDone
	default:
		return node.failRequest(ctx, req, "")
	}

	user, err := node.store.ReadUser(ctx, call.UserIdFromPublicPath())
	if err != nil || user == nil {
		panic(fmt.Errorf("store.ReadUser(%s) => %v %v", call.UserIdFromPublicPath(), user, err))
	}
	os, _, err := node.GetSystemCallReferenceOutputs(ctx, call.UserIdFromPublicPath(), call.RequestHash, byte(outputState))
	logger.Printf("node.GetSystemCallReferenceTxs(%s) => %v %v", req.MixinHash.String(), os, err)
	if err != nil {
		panic(err)
	}
	as := node.GetSystemCallRelatedAsset(ctx, os)

	switch flag {
	case ConfirmFlagNonceAvailable:
		var sessions []*store.Session
		prepare, tx, err := node.getSubSystemCallFromExtra(ctx, req, extra[16:])
		if err != nil {
			return node.failRequest(ctx, req, "")
		}
		if prepare != nil {
			prepare.Superior = call.RequestId
			prepare.Type = store.CallTypePrepare
			prepare.Public = hex.EncodeToString(user.FingerprintWithEmptyPath())
			prepare.State = common.RequestStatePending

			err = node.VerifySubSystemCall(ctx, tx, solana.MustPublicKeyFromBase58(node.conf.SolanaDepositEntry), solana.MustPublicKeyFromBase58(user.ChainAddress))
			logger.Printf("node.VerifySubSystemCall(%s) => %v", user.ChainAddress, err)
			if err != nil {
				return node.failRequest(ctx, req, "")
			}
			err = node.comparePrepareCallWithSolanaTx(tx, as)
			logger.Printf("node.comparePrepareCallWithSolanaTx(%s) => %v", call.RequestId, err)
			if err != nil {
				return node.failRequest(ctx, req, "")
			}

			sessions = append(sessions, &store.Session{
				Id:         prepare.RequestId,
				RequestId:  prepare.RequestId,
				MixinHash:  req.MixinHash.String(),
				MixinIndex: req.Output.OutputIndex,
				Index:      0,
				Operation:  OperationTypeSignInput,
				Public:     prepare.Public,
				Extra:      prepare.MessageHex(),
				CreatedAt:  req.CreatedAt,
			})

			index, err := solanaApp.GetSignatureIndexOfAccount(*tx, node.getMTGAddress(ctx))
			if err != nil {
				panic(err)
			}
			if index == -1 {
				prepare.Signature = sql.NullString{Valid: true, String: ""}
			}
		}

		var txs []*mtg.Transaction
		var ids []string
		destination := node.getMTGAddress(ctx).String()
		for _, asset := range as {
			if !asset.Solana {
				continue
			}
			id := common.UniqueId(req.Id, asset.AssetId)
			id = common.UniqueId(id, "withdrawal")
			memo := []byte(call.RequestId)
			tx := node.buildWithdrawalTransaction(ctx, req.Output, asset.AssetId, asset.Amount.String(), memo, destination, "", id)
			if tx == nil {
				return node.failRequest(ctx, req, asset.AssetId)
			}
			txs = append(txs, tx)
			ids = append(ids, tx.TraceId)
		}
		call.RequestSignerAt = sql.NullTime{Valid: true, Time: req.CreatedAt}
		call.WithdrawalTraces = sql.NullString{Valid: true, String: strings.Join(ids, ",")}
		call.State = common.RequestStatePending

		sessions = append(sessions, &store.Session{
			Id:         call.RequestId,
			RequestId:  call.RequestId,
			MixinHash:  req.MixinHash.String(),
			MixinIndex: req.Output.OutputIndex,
			Index:      1,
			Operation:  OperationTypeSignInput,
			Public:     call.Public,
			Extra:      call.MessageHex(),
			CreatedAt:  req.CreatedAt,
		})

		err = node.store.ConfirmNonceAvailableWithRequest(ctx, req, call, prepare, sessions, txs, "")
		if err != nil {
			panic(err)
		}
		return txs, ""
	case ConfirmFlagNonceExpired:
		mix, err := bot.NewMixAddressFromString(user.MixAddress)
		if err != nil {
			panic(err)
		}
		return node.refundAndFailRequest(ctx, req, mix.Members(), int(mix.Threshold), call, os)
	default:
		logger.Printf("invalid nonce confirm flag: %d", flag)
		return node.failRequest(ctx, req, "")
	}
}

func (node *Node) processDeployExternalAssetsCall(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeDeployExternalAssets {
		panic(req.Action)
	}

	var as []*solanaApp.DeployedAsset
	extra := req.ExtraBytes()
	n, extra := extra[0], extra[1:]
	offset := 0
	for len(as) < int(n) {
		assetId := uuid.Must(uuid.FromBytes(extra[offset : offset+16])).String()
		offset += 16
		address := solana.PublicKeyFromBytes(extra[offset : offset+32]).String()
		offset += 32

		asset, err := common.SafeReadAssetUntilSufficient(ctx, assetId)
		if err != nil {
			panic(err)
		}
		if asset == nil || asset.ChainID == solanaApp.SolanaChainBase {
			logger.Printf("processDeployExternalAssets(%s) => invalid asset", assetId)
			return node.failRequest(ctx, req, "")
		}
		old, err := node.store.ReadDeployedAsset(ctx, assetId)
		if err != nil {
			panic(err)
		}
		if old != nil {
			logger.Printf("processDeployExternalAssets(%s) => asset already existed", assetId)
			return node.failRequest(ctx, req, "")
		}
		if !common.CheckTestEnvironment(ctx) { // TODO should not skip the test
			mint, err := node.RPCGetAsset(ctx, address)
			if err != nil || mint == nil ||
				mint.Decimals != uint32(solanaApp.AssetDecimal) ||
				mint.MintAuthority != node.getMTGAddress(ctx).String() ||
				mint.FreezeAuthority != "" {
				// TODO check symbol and name
				panic(fmt.Errorf("solana.RPCGetAsset(%s) => %v", address, mint))
			}
		}
		as = append(as, &solanaApp.DeployedAsset{
			AssetId:  assetId,
			ChainId:  asset.ChainID,
			Address:  address,
			Decimals: int64(solanaApp.AssetDecimal),
			Asset:    asset,
		})
		logger.Verbosef("processDeployExternalAssets() => %s %s", assetId, address)
	}

	err := node.store.WriteDeployedAssetsWithRequest(ctx, req, as)
	logger.Printf("store.WriteDeployedAssetsWithRequest() => %v", err)
	if err != nil {
		panic(err)
	}
	return nil, ""
}

func (node *Node) processConfirmCall(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeConfirmCall {
		panic(req.Action)
	}

	extra := req.ExtraBytes()
	flag, extra := extra[0], extra[1:]

	switch flag {
	case FlagConfirmCallSuccess:
		n, extra := int(extra[0]), extra[1:]
		if n == 0 || n > 2 {
			logger.Printf("invalid length of signature: %d", n)
			return node.failRequest(ctx, req, "")
		}

		var calls []*store.SystemCall

		signature := base58.Encode(extra[:64])
		call, tx, err := node.checkConfirmCallSignature(ctx, signature)
		logger.Printf("node.checkConfirmCallSignature(%s) => %v", signature, err)
		if err != nil {
			if strings.Contains(err.Error(), "failed solana tx") {
				return node.failSystemCall(ctx, req, call)
			}
			return node.failRequest(ctx, req, "")
		}

		switch call.Type {
		case store.CallTypeDeposit, store.CallTypePostProcess:
			return node.confirmBurnRelatedSystemCall(ctx, req, call, tx, signature)
		case store.CallTypePrepare:
			calls = append(calls, call)
			if n == 2 {
				signature := base58.Encode(extra[64:128])
				call, _, err = node.checkConfirmCallSignature(ctx, signature)
				logger.Printf("node.checkConfirmCallSignature(%s) => %v", signature, err)
				if err != nil {
					if strings.Contains(err.Error(), "failed solana tx") {
						return node.failSystemCall(ctx, req, call)
					}
					return node.failRequest(ctx, req, "")
				}
				calls = append(calls, call)
			}
		case store.CallTypeMain:
			if n == 2 {
				panic(call.Type)
			}
			calls = append(calls, call)
		default:
			panic(call.Type)
		}

		var session *store.Session
		var outputs []*store.UserOutput
		var post *store.SystemCall
		if call.Type == store.CallTypeMain {
			os, _, err := node.GetSystemCallReferenceOutputs(ctx, call.UserIdFromPublicPath(), call.RequestHash, common.RequestStatePending)
			if err != nil {
				panic(err)
			}
			outputs = os

			post, err = node.getPostProcessCall(ctx, req, flag, call, extra[n*64:])
			logger.Printf("node.getPostProcessCall(%v %v) => %v %v", req, call, post, err)
			if err != nil {
				return node.failRequest(ctx, req, "")
			}
			if post != nil {
				session = &store.Session{
					Id:         post.RequestId,
					RequestId:  post.RequestId,
					MixinHash:  req.MixinHash.String(),
					MixinIndex: req.Output.OutputIndex,
					Index:      0,
					Operation:  OperationTypeSignInput,
					Public:     post.Public,
					Extra:      post.MessageHex(),
					CreatedAt:  req.CreatedAt,
				}
			}
		}
		err = node.store.ConfirmSystemCallsWithRequest(ctx, req, calls, post, session, outputs)
		if err != nil {
			panic(err)
		}
		return nil, ""
	case FlagConfirmCallFail:
		callId := uuid.Must(uuid.FromBytes(extra[:16])).String()
		call, err := node.store.ReadSystemCallByRequestId(ctx, callId, 0)
		logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", callId, call, err)
		if err != nil {
			panic(err)
		}
		return node.failSystemCall(ctx, req, call)
	default:
		logger.Printf("invalid confirm flag: %d", flag)
		return node.failRequest(ctx, req, "")
	}
}

func (node *Node) processObserverRequestSign(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeSignInput {
		panic(req.Action)
	}

	extra := req.ExtraBytes()
	callId := uuid.Must(uuid.FromBytes(extra[:16])).String()
	call, err := node.store.ReadSystemCallByRequestId(ctx, callId, common.RequestStatePending)
	logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", callId, call, err)
	if err != nil {
		panic(err)
	}
	if call == nil || call.Signature.Valid || call.State == common.RequestStateFailed {
		return node.failRequest(ctx, req, "")
	}
	if call.RequestSignerAt.Valid && call.RequestSignerAt.Time.Add(20*time.Minute).After(req.CreatedAt) {
		return node.failRequest(ctx, req, "")
	}

	old, err := node.store.ReadSession(ctx, req.Id)
	logger.Printf("store.ReadSession(%s) => %v %v", req.Id, old, err)
	if err != nil {
		panic(err)
	}
	if old != nil {
		return node.failRequest(ctx, req, "")
	}

	session := &store.Session{
		Id:         req.Id,
		RequestId:  call.RequestId,
		MixinHash:  req.MixinHash.String(),
		MixinIndex: req.Output.OutputIndex,
		Index:      0,
		Operation:  OperationTypeSignInput,
		Public:     call.Public,
		Extra:      call.MessageHex(),
		CreatedAt:  req.CreatedAt,
	}
	err = node.store.WriteSignSessionWithRequest(ctx, req, call, []*store.Session{session})
	if err != nil {
		panic(err)
	}
	return nil, ""
}

// create system call to transfer assets to mtg deposit entry or burn assets from user account on Solana
func (node *Node) processObserverCreateDepositCall(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	logger.Printf("node.processObserverCreateDepositCall(%s)", string(node.id))
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeDeposit {
		panic(req.Action)
	}

	extra := req.ExtraBytes()
	userAddress := solana.PublicKeyFromBytes(extra[:32])
	signature := solana.SignatureFromBytes(extra[32:96])

	user, err := node.store.ReadUserByChainAddress(ctx, userAddress.String())
	logger.Printf("store.ReadUserByChainAddress(%s) => %v %v", userAddress.String(), user, err)
	if err != nil {
		panic(err)
	}
	if user == nil {
		return node.failRequest(ctx, req, "")
	}

	call, tx, err := node.getSubSystemCallFromExtra(ctx, req, extra[96:])
	if err != nil {
		logger.Printf("node.getSubSystemCallFromExtra(%v) => %v", req, err)
		return node.failRequest(ctx, req, "")
	}
	err = node.VerifySubSystemCall(ctx, tx, solana.MustPublicKeyFromBase58(node.conf.SolanaDepositEntry), userAddress)
	logger.Printf("node.VerifySubSystemCall(%s %s) => %v", node.conf.SolanaDepositEntry, userAddress, err)
	if err != nil {
		return node.failRequest(ctx, req, "")
	}
	call.Superior = call.RequestId
	call.Type = store.CallTypeDeposit
	call.Public = hex.EncodeToString(user.FingerprintWithPath())
	call.State = common.RequestStatePending

	err = node.compareDepositCallWithSolanaTx(ctx, tx, signature.String(), user.ChainAddress)
	if err != nil {
		logger.Printf("node.compareDepositCallWithSolanaTx(%s %s) => %v", signature.String(), user.ChainAddress, err)
		return node.failRequest(ctx, req, "")
	}

	session := &store.Session{
		Id:         call.RequestId,
		RequestId:  call.RequestId,
		MixinHash:  req.MixinHash.String(),
		MixinIndex: req.Output.OutputIndex,
		Index:      0,
		Operation:  OperationTypeSignInput,
		Public:     call.Public,
		Extra:      call.MessageHex(),
		CreatedAt:  req.CreatedAt,
	}
	err = node.store.WriteDepositCallWithRequest(ctx, req, call, session)
	if err != nil {
		panic(err)
	}

	return nil, ""
}

// deposit from Solana to mtg deposit entry
func (node *Node) processDeposit(ctx context.Context, out *mtg.Action) ([]*mtg.Transaction, string) {
	logger.Printf("node.processDeposit(%v)", out)
	ar, handled, err := node.store.ReadActionResult(ctx, out.OutputId, out.OutputId)
	logger.Printf("store.ReadActionResult(%s %s) => %v %t %v", out.OutputId, out.OutputId, ar, handled, err)
	if err != nil {
		panic(err)
	}
	if ar != nil {
		return ar.Transactions, ar.Compaction
	}
	if handled {
		err = node.store.FailAction(ctx, &store.Request{
			Id:     out.OutputId,
			Output: out,
		})
		if err != nil {
			panic(err)
		}
		return nil, ""
	}

	var ts []*solanaApp.Transfer
	var tx *solana.Transaction
	var meta *rpc.TransactionMeta
	if common.CheckTestEnvironment(ctx) {
		ts = append(ts, &solanaApp.Transfer{
			AssetId:  out.AssetId,
			Receiver: node.SolanaDepositEntry().String(),
			Sender:   "GTQaVWXJyTyqauC4XgrDKUeVhSFkbS94YnbTnVCbFRiF",
			Value:    new(big.Int).SetInt64(90432841),
		})
	} else {
		if len(out.DepositHash.String) < 16 {
			panic(out.TransactionHash)
		}
		rpcTx, err := node.RPCGetTransaction(ctx, out.DepositHash.String)
		if err != nil {
			panic(err)
		}
		tx, err = rpcTx.Transaction.GetTransaction()
		if err != nil {
			panic(err)
		}
		meta = rpcTx.Meta
		err = node.processTransactionWithAddressLookups(ctx, tx)
		if err != nil {
			panic(err)
		}
		ts, err = solanaApp.ExtractTransfersFromTransaction(ctx, tx, rpcTx.Meta, nil)
		if err != nil {
			panic(err)
		}
	}

	var txs []*mtg.Transaction
	var compaction string
	for i, t := range ts {
		logger.Printf("%d-th transfer: %v", i, t)
		if t.AssetId != out.AssetId {
			continue
		}
		if t.Receiver != node.SolanaDepositEntry().String() {
			continue
		}
		user, err := node.store.ReadUserByChainAddress(ctx, t.Sender)
		logger.Printf("store.ReadUserByAddress(%s) => %v %v", t.Sender, user, err)
		if err != nil {
			panic(err)
		} else if user == nil {
			memo := solanaApp.ExtractMemoFromTransaction(ctx, tx, meta, node.SolanaPayer())
			logger.Printf("solana.ExtractMemoFromTransaction(%s) => %s", tx.Signatures[0].String(), memo)
			if memo == "" {
				continue
			}
			call, err := node.store.ReadSystemCallByRequestId(ctx, memo, common.RequestStateFailed)
			logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", memo, call, err)
			if err != nil {
				panic(err)
			}
			if call == nil || call.Type != store.CallTypePrepare {
				continue
			}
			superir, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, common.RequestStateFailed)
			if err != nil {
				panic(err)
			}
			user, err = node.store.ReadUser(ctx, superir.UserIdFromPublicPath())
			if err != nil {
				panic(err)
			}
		}
		mix, err := bot.NewMixAddressFromString(user.MixAddress)
		if err != nil {
			panic(err)
		}
		asset, err := common.SafeReadAssetUntilSufficient(ctx, t.AssetId)
		if err != nil {
			panic(err)
		}
		var expected mc.Integer
		if asset.ChainID == common.SafeSolanaChainId {
			expected = mc.NewIntegerFromString(decimal.NewFromBigInt(t.Value, -int32(asset.Precision)).String())
		} else {
			expected = mc.NewIntegerFromString(decimal.NewFromBigInt(t.Value, -int32(solanaApp.AssetDecimal)).String())
		}
		actual := mc.NewIntegerFromString(out.Amount.String())
		if expected.Cmp(actual) != 0 {
			continue
		}
		id := common.UniqueId(out.DepositHash.String, fmt.Sprintf("deposit-%d", i))
		id = common.UniqueId(id, t.Receiver)
		hash := out.DepositHash.String
		tx := node.buildTransaction(ctx, out, node.conf.AppId, t.AssetId, mix.Members(), int(mix.Threshold), out.Amount.String(), []byte(hash), id)
		if tx == nil {
			return nil, t.AssetId
		}
		txs = append(txs, tx)
	}

	err = node.store.WriteDepositRequestIfNotExist(ctx, out, common.RequestStateDone, txs, compaction)
	logger.Printf("store.WriteDepositRequestIfNotExist(%v %d %s) => %v", out, len(txs), compaction, err)
	if err != nil {
		panic(err)
	}

	return txs, compaction
}

func (node *Node) refundAndFailRequest(ctx context.Context, req *store.Request, members []string, threshod int, call *store.SystemCall, os []*store.UserOutput) ([]*mtg.Transaction, string) {
	as := node.GetSystemCallRelatedAsset(ctx, os)
	txs, compaction := node.buildRefundTxs(ctx, req, call.RequestId, as, members, threshod)
	err := node.store.RefundOutputsWithRequest(ctx, req, call, os, txs, compaction)
	if err != nil {
		panic(err)
	}
	return txs, compaction
}

func (node *Node) failSystemCall(ctx context.Context, req *store.Request, call *store.SystemCall) ([]*mtg.Transaction, string) {
	switch call.State {
	case common.RequestStatePending, common.RequestStateFailed:
	default:
		return node.failRequest(ctx, req, "")
	}

	extra := req.ExtraBytes()
	flag, extra := extra[0], extra[1:]

	storage := extra[16:]
	if call == nil {
		panic(req)
	}
	if flag == FlagConfirmCallSuccess {
		storage = nil
	}

	var outputs []*store.UserOutput
	var mix *bot.MixAddress
	switch call.Type {
	case store.CallTypeMain, store.CallTypePrepare:
		main := call
		if call.Type == store.CallTypePrepare {
			c, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
			logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", call.Superior, c, err)
			if err != nil || c == nil {
				panic(err)
			}
			main = c

			user, err := node.store.ReadUser(ctx, main.UserIdFromPublicPath())
			if err != nil {
				panic(err)
			}
			mix, err = bot.NewMixAddressFromString(user.MixAddress)
			if err != nil {
				panic(err)
			}
		}

		os, _, err := node.GetSystemCallReferenceOutputs(ctx, main.UserIdFromPublicPath(), main.RequestHash, 0)
		if err != nil {
			panic(err)
		}
		outputs = os
	}

	var session *store.Session
	post, err := node.getPostProcessCall(ctx, req, FlagConfirmCallFail, call, storage)
	logger.Printf("node.getPostProcessCall(%v %v) => %v %v", req, call, post, err)
	if err != nil {
		return node.failRequest(ctx, req, "")
	}
	if post != nil {
		session = &store.Session{
			Id:         post.RequestId,
			RequestId:  post.RequestId,
			MixinHash:  req.MixinHash.String(),
			MixinIndex: req.Output.OutputIndex,
			Index:      0,
			Operation:  OperationTypeSignInput,
			Public:     post.Public,
			Extra:      post.MessageHex(),
			CreatedAt:  req.CreatedAt,
		}
	}

	// refund external assets when prepare call failed
	// solana assets would be transfered to user when mtg receives deposit
	var txs []*mtg.Transaction
	var compaction string
	if call.Type == store.CallTypePrepare && mix != nil {
		as := node.GetSystemCallRelatedAsset(ctx, outputs)
		var assets []*ReferencedTxAsset
		for _, a := range as {
			if a.Solana {
				continue
			}
			assets = append(assets, a)
		}
		if len(assets) > 0 {
			txs, compaction = node.buildRefundTxs(ctx, req, call.RequestId, as, mix.Members(), int(mix.Threshold))
		}
	}

	err = node.store.FailSystemCallWithRequest(ctx, req, call, post, session, outputs, txs, compaction)
	if err != nil {
		panic(err)
	}
	return nil, ""
}

func (node *Node) checkConfirmCallSignature(ctx context.Context, signature string) (*store.SystemCall, *rpc.GetTransactionResult, error) {
	transaction, err := node.RPCGetTransaction(ctx, signature)
	if err != nil || transaction == nil {
		panic(fmt.Errorf("checkConfirmCallSignature(%s) => %v", signature, err))
	}
	tx, err := transaction.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		panic(err)
	}
	hash := crypto.Sha256Hash(msg).String()

	if common.CheckTestEnvironment(ctx) {
		cs, err := node.store.ListSignedCalls(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Println("===")
		fmt.Println(signature)
		fmt.Println(hex.EncodeToString(msg))
		for _, c := range cs {
			fmt.Println(c.Type, c.MessageHash)
		}
		test := getTestSystemConfirmCallMessage(signature)
		if test != "" {
			hash = test
		}
	}

	call, err := node.store.ReadSystemCallByMessage(ctx, hash)
	if err != nil {
		panic(fmt.Errorf("store.ReadSystemCallByMessage(%x) => %v", msg, err))
	}
	if call == nil || call.State != common.RequestStatePending {
		return nil, nil, fmt.Errorf("checkConfirmCallSignature(%s) => invalid call %v", signature, call)
	}
	if transaction.Meta.Err != nil {
		return call, nil, fmt.Errorf("failed solana tx: %s %v", signature, transaction.Meta.Err)
	}
	call.State = common.RequestStateDone
	call.Hash = sql.NullString{Valid: true, String: signature}
	return call, transaction, nil
}

func (node *Node) confirmBurnRelatedSystemCall(ctx context.Context, req *store.Request, call *store.SystemCall, rpcTx *rpc.GetTransactionResult, signature string) ([]*mtg.Transaction, string) {
	user, err := node.store.ReadUser(ctx, call.UserIdFromPublicPath())
	if err != nil {
		panic(err)
	}
	mix, err := bot.NewMixAddressFromString(user.MixAddress)
	if err != nil {
		panic(err)
	}

	tx, err := rpcTx.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	if common.CheckTestEnvironment(ctx) {
		if tx.Signatures[0].String() == "5s3UBMymdgDHwYvuaRdq9SLq94wj5xAgYEsDDB7TQwwuLy1TTYcSf6rF4f2fDfF7PnA9U75run6r1pKm9K1nusCR" {
			user.ChainAddress = "5YLSixqjK2m8ECirGaco8tHSn2Uc4aY7cLPoMSMptsgG"
		}
	}
	changes := node.buildUserBalanceChangesFromMeta(ctx, tx, rpcTx.Meta, solana.MPK(user.ChainAddress))

	var txs []*mtg.Transaction
	bs := solanaApp.ExtractBurnsFromTransaction(ctx, tx)
	for _, burn := range bs {
		address := burn.GetMintAccount().PublicKey.String()
		da, err := node.store.ReadDeployedAssetByAddress(ctx, address)
		if err != nil || da == nil {
			panic(err)
		}

		amount := decimal.New(int64(*burn.Amount), -int32(da.Decimals))
		amt := mc.NewIntegerFromString(amount.String())
		if common.CheckTestEnvironment(ctx) && req.Id == "329346e1-34c2-4de0-8e35-729518eda8bd" {
			amt = mc.NewIntegerFromString("0.02")
		}
		if amt.Sign() == 0 {
			continue
		}

		change := changes[address]
		if change == nil || !change.Amount.Abs().Equal(amount) {
			continue
		}

		id := common.UniqueId(signature, fmt.Sprintf("BURN:%s", da.AssetId))
		id = common.UniqueId(id, user.MixAddress)
		memo := []byte(call.RequestId)
		if call.Type == store.CallTypeDeposit {
			req, err := node.store.ReadRequestByHash(ctx, call.RequestHash)
			if err != nil {
				panic(err)
			}
			sig := solana.SignatureFromBytes(req.ExtraBytes()[32:96]).String()
			memo = []byte(sig)
		}
		tx := node.buildTransaction(ctx, req.Output, node.conf.AppId, da.AssetId, mix.Members(), int(mix.Threshold), amt.String(), memo, id)
		if tx == nil {
			return node.failRequest(ctx, req, da.AssetId)
		}
		txs = append(txs, tx)
	}

	err = node.store.ConfirmBurnRelatedSystemCallWithRequest(ctx, req, call, txs)
	if err != nil {
		panic(err)
	}
	return txs, ""
}
