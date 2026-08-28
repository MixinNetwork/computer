package solana

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	mc "github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
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
	mmix, err := bot.NewMixAddressFromString(mix)
	logger.Printf("bot.NewMixAddressFromString(%s) => %v", mix, err)
	if err != nil {
		return node.failRequest(ctx, req, "")
	}
	if !checkUser(req, mmix) {
		return node.failRequest(ctx, req, "")
	}

	old, err := node.store.ReadUserByMixAddress(ctx, mix)
	logger.Printf("store.ReadUserByMixAddress(%s) => %v %v", mix, old, err)
	if err != nil {
		panic(fmt.Errorf("store.ReadUserByMixAddress(%s) => %v", mix, err))
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
	mix, err := bot.NewMixAddressFromString(user.MixAddress)
	if err != nil {
		panic(err)
	}
	if !checkUser(req, mix) {
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
//     if memo includes the fee id and mtg receives extra amount of XIN (> 0.0001), same value of SOL would be tranfered
//     to user account in prepare system call.
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
	if !checkUser(req, mix) {
		return node.failRequest(ctx, req, "")
	}

	os, storage, err := node.GetSystemCallReferenceOutputs(ctx, user.UserId, req.MixinHash.String(), common.RequestStateInitial)
	logger.Printf("node.GetSystemCallReferenceTxs(%s) => %v %v %v", req.MixinHash.String(), os, storage, err)
	if err != nil || storage == nil {
		return node.failRequest(ctx, req, "")
	}
	// External-asset deployments are MTG consensus state and can be checked
	// deterministically before the call is persisted.
	err = node.validateSystemCallReferencedAssets(ctx, os)
	if err != nil {
		logger.Printf("node.validateSystemCallReferencedAssets(%s) => %v", req.Id, err)
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

	old, err := node.store.ReadSystemCallByRequestId(ctx, cid, 0)
	if err != nil {
		panic(err)
	}
	if old != nil {
		logger.Printf("store.ReadSystemCallByRequestId(%s) => %v", cid, old)
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

	old, err = node.store.ReadSystemCallByMessage(ctx, call.MessageHash)
	if err != nil {
		panic(err)
	}
	if old != nil {
		logger.Printf("store.ReadSystemCallByMessage(%s) => %s", call.MessageHash, old.RequestId)
		return node.failRequest(ctx, req, "")
	}
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
	if len(extra) < 1+uuid.Size {
		logger.Printf("invalid extra length for confirm nonce: %d", len(extra))
		return node.failRequest(ctx, req, "")
	}
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
	switch flag {
	case ConfirmFlagNonceAvailable:
		as := node.GetSystemCallRelatedAsset(ctx, os)
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

			// A fee-only prepare transfers SOL from the payer, so it does not
			// necessarily require the MTG authority to sign.
			err = node.VerifySubSystemCallEnvelope(tx, node.getMTGAddress(ctx), false)
			logger.Printf("node.VerifySubSystemCallEnvelope(%s) => %v", prepare.RequestId, err)
			if err != nil {
				return node.failRequest(ctx, req, "")
			}
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
	if len(extra) < 1 {
		logger.Printf("invalid extra length for deploy external assets: %d", len(extra))
		return node.failRequest(ctx, req, "")
	}
	assetSize := uuid.Size + solana.PublicKeyLength
	if len(extra) != 1+int(extra[0])*assetSize {
		logger.Printf("invalid extra length for deploy external assets: %d", len(extra))
		return node.failRequest(ctx, req, "")
	}
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
			// Deployment validation must use current on-chain state. The general
			// asset lookup is cached and could otherwise preserve a stale supply.
			mint, err := node.solana.RPCGetAsset(ctx, address)
			if err != nil {
				panic(fmt.Errorf("solana.RPCGetAsset(%s) => %v", address, err))
			}
			err = validateExternalAssetMint(address, node.getMTGAddress(ctx).String(), mint)
			if err != nil {
				logger.Printf("validateExternalAssetMint(%s) => %v", address, err)
				return node.failRequest(ctx, req, "")
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

func validateExternalAssetMint(address, mtg string, mint *solanaApp.Asset) error {
	if mint == nil {
		return fmt.Errorf("mint not found")
	}
	if mint.Address != address {
		return fmt.Errorf("invalid address: %s", mint.Address)
	}
	if mint.ProgramId != solana.TokenProgramID.String() {
		return fmt.Errorf("invalid program: %s", mint.ProgramId)
	}
	if !mint.IsInitialized {
		return fmt.Errorf("mint is not initialized")
	}
	if mint.Decimals != uint32(solanaApp.AssetDecimal) {
		return fmt.Errorf("invalid decimals: %d", mint.Decimals)
	}
	if mint.MintAuthority != mtg {
		return fmt.Errorf("invalid mint authority: %s", mint.MintAuthority)
	}
	if mint.FreezeAuthority != "" {
		return fmt.Errorf("invalid freeze authority: %s", mint.FreezeAuthority)
	}

	supply, ok := new(big.Int).SetString(mint.Supply, 10)
	if !ok || supply.Sign() != 0 {
		return fmt.Errorf("invalid initial supply: %s", mint.Supply)
	}
	return nil
}

// processConfirmCall accepts the following record combinations:
//
//	1 [success:Main]: confirm a Main call that has no pending Prepare; optional
//	  storage contains its normal post-process call.
//	2 [fail:Main]: fail a Main call that has no pending Prepare; optional
//	  storage contains its cleanup call.
//	3 [fail:Prepare]: fail Prepare and its Main call; storage contains the
//	  Solana-asset refund call exactly when such a refund exists.
//	4 [success:Prepare, success:Main]: confirm both calls; optional storage
//	  contains Main's normal post-process call.
//	5 [success:Prepare, fail:Main]: confirm Prepare and fail Main; optional
//	  storage contains Main's cleanup call.
//	6 [success:Deposit/PostProcess]: finish one terminal call;
//		  storage must be empty.
//	7 [fail:Deposit/PostProcess]: fail one terminal call;
//		  storage must be empty.
//
// No other ordering is valid: there are at most two records, a failed record
// must be last, and a two-record sequence must be Prepare followed by its Main.
// Every record must also match a Solana transaction whose Meta.Err agrees with
// the reported status.
func (node *Node) processConfirmCall(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeConfirmCall {
		panic(req.Action)
	}

	records, storage, err := decodeConfirmCallRecords(req.ExtraBytes())
	if err != nil {
		logger.Printf("decodeConfirmCallRecords(%s) => %v", req.Id, err)
		return node.failRequest(ctx, req, "")
	}
	err = validateConfirmCallStorage(storage)
	if err != nil {
		panic(fmt.Errorf("validateConfirmCallStorage(%s) => %w", req.Id, err))
	}

	calls := make([]*store.SystemCall, 0, len(records))
	transactions := make([]*rpc.GetTransactionResult, 0, len(records))
	// Verify every reported result independently against the transaction that
	// Solana confirmed. validateConfirmCall also marks the in-memory call with
	// the target state/hash; database writes still happen only after the whole
	// record combination is accepted.
	for _, record := range records {
		call, transaction, err := node.validateConfirmCall(ctx, req, record.Status, record.CallId, record.Signature.String())
		logger.Printf("node.validateConfirmCall(%d %s %s) => %v", record.Status, record.CallId, record.Signature.String(), err)
		if err != nil {
			return node.failRequest(ctx, req, "")
		}
		calls = append(calls, call)
		transactions = append(transactions, transaction)
	}

	// A two-call confirmation is only valid for the execution sequence created
	// by the observer: a successful Prepare followed by its Main call.
	if len(calls) == 2 {
		if records[0].Status != FlagConfirmCallSuccess || calls[0].Type != store.CallTypePrepare ||
			calls[1].Type != store.CallTypeMain || calls[0].Superior != calls[1].RequestId {
			logger.Printf("invalid confirm call sequence: %v %v", calls[0], calls[1])
			return node.failRequest(ctx, req, "")
		}
	}
	if len(calls) == 1 {
		switch calls[0].Type {
		case store.CallTypeMain:
			// If Main still has a pending Prepare, its result cannot be confirmed alone.
			needPrepare, err := node.store.CheckUnfinishedSubCalls(ctx, calls[0])
			if err != nil {
				panic(err)
			}
			if needPrepare {
				logger.Printf("main confirm call is missing prepare evidence: %s", calls[0].RequestId)
				return node.failRequest(ctx, req, "")
			}
		case store.CallTypePrepare:
			// The observer never executes Prepare independently: ListSignedCalls always
			// pairs it with Main. Accepting a standalone success would unnecessarily
			// widen the confirmation protocol and allow Main evidence to be omitted.
			if records[0].Status == FlagConfirmCallSuccess {
				return node.failRequest(ctx, req, "")
			}
		}
	}
	// Execution stops at the first failed transaction, so a failure can only be
	// the final record in the reported sequence.
	for i, record := range records {
		if record.Status == FlagConfirmCallFail && i != len(records)-1 {
			logger.Printf("failed confirm call record must be last: %d", i)
			return node.failRequest(ctx, req, "")
		}
	}

	call := calls[len(calls)-1]
	flag := records[len(records)-1].Status
	signature := records[len(records)-1].Signature.String()

	// Deposit and PostProcess calls are terminal calls. They cannot be combined
	// with another confirmation or create another post-process transaction.
	if call.Type == store.CallTypeDeposit || call.Type == store.CallTypePostProcess {
		if len(calls) != 1 || len(storage) != 0 {
			return node.failRequest(ctx, req, "")
		}
		// For situation 7
		if flag == FlagConfirmCallFail {
			return node.failSystemCall(ctx, req, call, nil, nil)
		}
		// For situation 6
		return node.confirmBurnRelatedSystemCall(ctx, req, call, transactions[0], signature)
	}

	// Failed Main/Prepare calls may carry one cleanup transaction, which is
	// validated by failSystemCall before a new signing session is created.
	// For situation 2, 3, 5
	if flag == FlagConfirmCallFail {
		return node.failSystemCall(ctx, req, call, storage, calls)
	}

	// For situation 1, 4
	return node.confirmMainOrPrepareSystemCalls(ctx, req, calls, storage)
}

func (node *Node) confirmMainOrPrepareSystemCalls(ctx context.Context, req *store.Request, calls []*store.SystemCall, storage []byte) ([]*mtg.Transaction, string) {
	call := calls[len(calls)-1]

	var session *store.Session
	var outputs []*store.UserOutput
	var post *store.SystemCall
	if call.Type != store.CallTypeMain {
		return node.failRequest(ctx, req, "")
	}
	os, _, err := node.GetSystemCallReferenceOutputs(ctx, call.UserIdFromPublicPath(), call.RequestHash, common.RequestStatePending)
	if err != nil {
		panic(err)
	}
	outputs = os

	post, err = node.getPostProcessCall(ctx, req, FlagConfirmCallSuccess, call, storage)
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
	err = node.store.ConfirmSystemCallsWithRequest(ctx, req, calls, post, session, outputs)
	if err != nil {
		panic(err)
	}
	return nil, ""
}

func (node *Node) processObserverRequestSign(ctx context.Context, req *store.Request) ([]*mtg.Transaction, string) {
	if req.Role != RequestRoleObserver {
		panic(req.Role)
	}
	if req.Action != OperationTypeSignInput {
		panic(req.Action)
	}

	extra := req.ExtraBytes()
	if len(extra) != uuid.Size {
		logger.Printf("invalid extra length for sign request: %d", len(extra))
		return node.failRequest(ctx, req, "")
	}
	callId := uuid.Must(uuid.FromBytes(extra[:16])).String()
	call, err := node.store.ReadSystemCallByRequestId(ctx, callId, common.RequestStatePending)
	logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", callId, call, err)
	if err != nil {
		panic(err)
	}
	if call == nil || call.Signature.Valid || call.State == common.RequestStateFailed {
		return node.failRequest(ctx, req, "")
	}
	if call.RequestSignerAt.Valid && call.RequestSignerAt.Time.Add(mpcRetryInterval).After(req.CreatedAt) {
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
	if len(extra) < solana.PublicKeyLength+solana.SignatureLength {
		logger.Printf("invalid extra length for deposit call: %d", len(extra))
		return node.failRequest(ctx, req, "")
	}
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
	if err != nil || call == nil {
		logger.Printf("node.getSubSystemCallFromExtra(%v) => %v %v", req, call, err)
		return node.failRequest(ctx, req, "")
	}
	err = node.VerifySubSystemCallEnvelope(tx, userAddress, true)
	logger.Printf("node.VerifySubSystemCallEnvelope(%s) => %v", call.RequestId, err)
	if err != nil {
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
func (node *Node) processDeposit(ctx context.Context, out *mtg.Action, restored bool) ([]*mtg.Transaction, string) {
	logger.Printf("node.processDeposit(%v)", out)
	ar, handled, err := node.store.ReadActionResult(ctx, out.OutputId, out.OutputId)
	logger.Printf("store.ReadActionResult(%s %s) => %v %t %v", out.OutputId, out.OutputId, ar, handled, err)
	if err != nil {
		panic(err)
	}
	if ar != nil {
		if restored {
			err = node.store.ResetRequest(ctx, out.OutputId, out.Sequence)
			if err != nil {
				panic(err)
			}
			handled = false
		} else {
			return ar.Transactions, ar.Compaction
		}
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

	var t *solanaApp.Transfer
	var tx *solana.Transaction
	var meta *rpc.TransactionMeta
	if common.CheckTestEnvironment(ctx) {
		t = &solanaApp.Transfer{
			AssetId:  out.AssetId,
			Receiver: node.SolanaDepositEntry().String(),
			Sender:   "GTQaVWXJyTyqauC4XgrDKUeVhSFkbS94YnbTnVCbFRiF",
			Value:    new(big.Int).SetInt64(90432841),
		}
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
		t = solanaApp.ExtractTransferFromTransactionByIndex(ctx, tx, rpcTx.Meta, out.DepositIndex.Int64)
		logger.Printf("solana.ExtractTransferFromTransactionByIndex(%s %s %d) => %v", out.OutputId, out.DepositHash.String, out.DepositIndex.Int64, t)
	}
	if t == nil || t.AssetId != out.AssetId || t.Receiver != node.SolanaDepositEntry().String() {
		return node.failDepositRequest(ctx, out, "", false)
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
		logger.Printf("invalid deposit amount: %s %s", actual.String(), out.Amount.String())
		return node.failDepositRequest(ctx, out, "", false)
	}

	// user == nil: transfer solana withdrawn assets from mtg to mtg deposit entry by post call for failed prepare call
	// user != nil: transfer or burn assets from user account to mtg deposit entry by post call or deposit call
	user, err := node.store.ReadUserByChainAddress(ctx, t.Sender)
	logger.Printf("store.ReadUserByAddress(%s) => %v %v", t.Sender, user, err)
	if err != nil {
		panic(err)
	}
	var call *store.SystemCall
	if user == nil {
		memo := solanaApp.ExtractMemoFromTransaction(ctx, tx, meta, node.SolanaPayer())
		logger.Printf("solana.ExtractMemoFromTransaction(%s) => %s", tx.Signatures[0].String(), memo)
		if memo == "" {
			return node.failDepositRequest(ctx, out, "", false)
		}
		call, err = node.store.ReadSystemCallByRequestId(ctx, memo, common.RequestStateFailed)
		logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", memo, call, err)
		if err != nil {
			panic(err)
		}
		if call == nil || call.Type != store.CallTypePrepare {
			return node.failDepositRequest(ctx, out, "", false)
		}
		superior, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, common.RequestStateFailed)
		logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", call.Superior, superior, err)
		if err != nil {
			panic(err)
		}
		user, err = node.store.ReadUser(ctx, superior.UserIdFromPublicPath())
		if err != nil {
			panic(err)
		}
		call = superior
	} else {
		call, err = node.store.ReadSystemCallByHash(ctx, out.DepositHash.String)
		logger.Printf("store.ReadSystemCallByHash(%s) => %v %v", out.DepositHash.String, call, err)
		if err != nil {
			panic(err)
		}
		if call == nil || call.State != common.RequestStateDone {
			return node.failDepositRequest(ctx, out, "", true)
		}
		switch call.Type {
		case store.CallTypeDeposit:
		case store.CallTypePostProcess:
			superior, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
			if err != nil {
				panic(err)
			}
			call = superior
		default:
			return node.failDepositRequest(ctx, out, "", false)
		}
	}
	mix, err := bot.NewMixAddressFromString(user.MixAddress)
	if err != nil {
		panic(err)
	}
	id := common.UniqueId(out.DepositHash.String, fmt.Sprint(out.DepositIndex.Int64))
	id = common.UniqueId(id, t.Receiver)
	mtx := node.buildTransaction(ctx, out, node.conf.AppId, t.AssetId, mix.Members(), int(mix.Threshold), out.Amount.String(), []byte(out.DepositHash.String), id)
	if mtx == nil {
		return node.failDepositRequest(ctx, out, t.AssetId, false)
	}
	txs := []*mtg.Transaction{mtx}
	old := call.GetRefundIds()
	old = append(old, mtx.TraceId)
	call.RefundTraces = sql.NullString{Valid: true, String: strings.Join(old, ",")}

	err = node.store.WriteDepositRequestIfNotExist(ctx, out, common.RequestStateDone, call, []*mtg.Transaction{mtx}, "")
	logger.Printf("store.WriteDepositRequestIfNotExist(%v %s) => %v", out, mtx.TraceId, err)
	if err != nil {
		panic(err)
	}

	return txs, ""
}

func (node *Node) failDepositRequest(ctx context.Context, out *mtg.Action, compaction string, save bool) ([]*mtg.Transaction, string) {
	logger.Printf("node.failDepositRequest(%v %s)", out, compaction)
	err := node.store.FailDepositRequestIfNotExist(ctx, out, compaction, save)
	if err != nil {
		panic(err)
	}
	return nil, compaction
}

func (node *Node) refundAndFailRequest(ctx context.Context, req *store.Request, members []string, threshod int, call *store.SystemCall, os []*store.UserOutput) ([]*mtg.Transaction, string) {
	as := aggregateSystemCallReferenceAssets(os)
	txs, compaction := node.buildRefundTxs(ctx, req, call.RequestId, as, members, threshod)
	err := node.store.RefundOutputsWithRequest(ctx, req, call, os, txs, compaction)
	if err != nil {
		panic(err)
	}
	return txs, compaction
}

// Handle the following situations for a failed system call:
//
//   - [fail:Main]: fail a Main call that has no pending Prepare; optional
//     storage contains its cleanup call.
//   - [fail:Prepare]: fail Prepare and its Main call; storage contains the
//     Solana-asset refund call exactly when such a refund exists.
//   - [success:Prepare, fail:Main]: confirm Prepare and fail Main; optional
//     storage contains Main's cleanup call.
//   - [fail:Deposit/PostProcess]: fail one terminal call;
//     storage must be empty.
func (node *Node) failSystemCall(ctx context.Context, req *store.Request, call *store.SystemCall, storage []byte, calls []*store.SystemCall) ([]*mtg.Transaction, string) {
	if call == nil {
		return node.failRequest(ctx, req, "")
	}
	// validateConfirmCall mutates call to the target state before any database
	// write happens. Re-read the persisted row here when rebuilding cleanup
	// expectations, so a first failure still sees Pending references and a
	// compaction replay sees the already-failed references.
	referenceCall, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
	logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", call.RequestId, referenceCall, err)
	if err != nil || referenceCall == nil {
		panic(fmt.Errorf("store.ReadSystemCallByRequestId(%s) => %v %v", call.RequestId, referenceCall, err))
	}

	var outputs []*store.UserOutput
	var mix *bot.MixAddress
	switch call.Type {
	case store.CallTypeMain, store.CallTypePrepare:
		main := referenceCall
		if call.Type == store.CallTypePrepare {
			c, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
			logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", call.Superior, c, err)
			if err != nil || c == nil {
				panic(err)
			}
			main = c

			// A failed Prepare needs a Solana cleanup whenever its associated withdrawals
			// produced refundable Solana transfers. The observer may omit storage only
			// when the deterministic refund builder produces no transaction.
			if len(storage) == 0 && len(node.buildRefundWithdrawalTransfers(ctx, referenceCall, main)) > 0 {
				logger.Printf("missing refund storage for failed Prepare: %s", call.RequestId)
				return node.failRequest(ctx, req, "")
			}

			user, err := node.store.ReadUser(ctx, main.UserIdFromPublicPath())
			if err != nil {
				panic(err)
			}
			mix, err = bot.NewMixAddressFromString(user.MixAddress)
			if err != nil {
				panic(err)
			}
		}

		os, _, err := node.GetSystemCallReferenceOutputs(ctx, main.UserIdFromPublicPath(), main.RequestHash, systemCallReferenceOutputStateValue(referenceCall.State))
		if err != nil {
			panic(err)
		}
		outputs = os
	}

	var session *store.Session
	post, err := node.getPostProcessCall(ctx, req, FlagConfirmCallFail, referenceCall, storage)
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
			txs, compaction = node.buildRefundTxs(ctx, req, call.RequestId, assets, mix.Members(), int(mix.Threshold))
		}
	}

	var confirmed []*store.SystemCall
	if len(calls) > 1 {
		confirmed = calls[:len(calls)-1]
	}
	err = node.store.FailSystemCallWithRequest(ctx, req, call, confirmed, post, session, outputs, txs, compaction)
	if err != nil {
		panic(err)
	}
	return txs, compaction
}

// validateConfirmCall verifies the observer's status against the stored call and
// the exact Solana transaction. Once the proof is accepted, it updates the
// returned call to the state that should be persisted.
func (node *Node) validateConfirmCall(ctx context.Context, req *store.Request, status byte, callId, signature string) (*store.SystemCall, *rpc.GetTransactionResult, error) {
	call, err := node.store.ReadSystemCallByRequestId(ctx, callId, 0)
	if err != nil || call == nil {
		panic(fmt.Errorf("store.ReadSystemCallByRequestId(%s) => %v %v", callId, call, err))
	}
	if !validConfirmCallState(req, status, call) {
		return nil, nil, fmt.Errorf("invalid confirm call state: %s %d", callId, call.State)
	}
	if !validFailedConfirmCallHash(call, signature) {
		return nil, nil, fmt.Errorf("invalid failed confirm call hash: %s %s", callId, signature)
	}

	transaction, err := node.RPCGetTransaction(ctx, signature)
	if err != nil || transaction == nil || transaction.Meta == nil {
		panic(fmt.Errorf("RPCGetTransaction(%s) => %v %v", signature, transaction, err))
	}
	tx, err := transaction.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	if len(tx.Signatures) == 0 || tx.Signatures[0].String() != signature {
		panic(fmt.Errorf("confirm call transaction signature mismatch: %s", signature))
	}
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		panic(err)
	}
	hash := crypto.Sha256Hash(msg).String()

	if common.CheckTestEnvironment(ctx) {
		cm, err := node.store.ListSignedCalls(ctx)
		if err != nil {
			panic(err)
		}
		fmt.Println("===")
		fmt.Println(signature)
		fmt.Println(hex.EncodeToString(msg))
		for _, cs := range cm {
			for _, c := range cs {
				fmt.Println(c.Type, c.MessageHash)
			}
		}
		test := getTestSystemConfirmCallMessage(signature)
		if test != "" {
			hash = test
		}
	}
	if hash != call.MessageHash {
		panic(fmt.Errorf("confirm call message mismatch: %s %s %s", callId, call.MessageHash, hash))
	}

	failed := transaction.Meta.Err != nil
	if common.CheckTestEnvironment(ctx) && isTestFailedSystemConfirmCall(signature) {
		failed = true
	}
	if status == FlagConfirmCallSuccess && failed {
		return nil, nil, fmt.Errorf("expected successful solana tx: %s", signature)
	}
	if status == FlagConfirmCallFail && !failed {
		return nil, nil, fmt.Errorf("expected failed solana tx: %s", signature)
	}
	switch status {
	case FlagConfirmCallSuccess:
		call.State = common.RequestStateDone
	case FlagConfirmCallFail:
		call.State = common.RequestStateFailed
	default:
		panic(status)
	}
	call.Hash = sql.NullString{Valid: true, String: signature}
	return call, transaction, nil
}

func (node *Node) confirmBurnRelatedSystemCall(ctx context.Context, req *store.Request, call *store.SystemCall, rpcTx *rpc.GetTransactionResult, signature string) ([]*mtg.Transaction, string) {
	main := call
	if call.Superior != call.RequestId {
		c, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
		if err != nil {
			panic(err)
		}
		main = c
	}
	user, err := node.store.ReadUser(ctx, main.UserIdFromPublicPath())
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
	var ids []string
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
		ids = append(ids, tx.TraceId)
	}

	fd, err := node.store.ReadFailedDepositByHash(ctx, signature)
	if err != nil {
		panic(err)
	}
	if fd != nil {
		id := common.UniqueId(fd.Hash, fmt.Sprint(fd.Index))
		id = common.UniqueId(id, node.SolanaDepositEntry().String())
		memo := []byte(fd.Hash)
		tx := node.buildTransaction(ctx, req.Output, node.conf.AppId, fd.AssetId, mix.Members(), int(mix.Threshold), fd.Amount, memo, id)
		if tx == nil {
			return node.failRequest(ctx, req, fd.AssetId)
		}
		txs = append(txs, tx)
		ids = append(ids, tx.TraceId)
	}

	old := call.GetRefundIds()
	old = append(old, ids...)
	call.RefundTraces = sql.NullString{Valid: true, String: strings.Join(old, ",")}

	err = node.store.ConfirmBurnRelatedSystemCallWithRequest(ctx, req, call, fd, txs)
	if err != nil {
		panic(err)
	}
	return txs, ""
}

func checkUser(req *store.Request, mix *bot.MixAddress) bool {
	senders := append([]string(nil), req.Output.Senders...)
	return mix.Threshold == byte(req.Output.SendersThreshold) &&
		bot.HashMembers(mix.Members()) == bot.HashMembers(senders)
}

func validConfirmCallState(req *store.Request, status byte, call *store.SystemCall) bool {
	if call.State == common.RequestStatePending {
		return true
	}
	// A fresh confirmation can only consume a pending call. Failed calls are
	// accepted only while replaying a compacted action; processAction admits that
	// path after it has found the same action result with a non-empty compaction.
	return status == FlagConfirmCallFail && call.State == common.RequestStateFailed && req != nil && req.Restored
}

func validFailedConfirmCallHash(call *store.SystemCall, signature string) bool {
	return call.State != common.RequestStateFailed || !call.Hash.Valid || call.Hash.String == signature
}
