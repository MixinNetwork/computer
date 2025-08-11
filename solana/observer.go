package solana

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
)

const (
	loopInterval = time.Second * 5
	BalanceLimit = 500000000
)

func (node *Node) bootObserver(ctx context.Context, version string) {
	if string(node.id) != node.conf.ObserverId {
		return
	}
	logger.Printf("bootObserver(%s)", node.id)
	go node.StartHTTP(version)

	err := node.sendPriceInfo(ctx)
	if err != nil {
		panic(err)
	}
	err = node.initMPCKeys(ctx)
	if err != nil {
		panic(err)
	}

	err = node.createALTForAccounts(ctx)
	if err != nil {
		panic(err)
	}

	err = node.checkNonceAccounts(ctx)
	if err != nil {
		panic(err)
	}

	go node.initializeUsersLoop(ctx)
	go node.deployOrConfirmAssetsLoop(ctx)

	go node.createNonceAccountLoop(ctx)
	go node.releaseNonceAccountLoop(ctx)

	go node.feeInfoLoop(ctx)
	go node.withdrawalFeeLoop(ctx)
	go node.unconfirmedWithdrawalLoop(ctx)

	go node.unconfirmedCallLoop(ctx)
	go node.unsignedCallLoop(ctx)
	go node.signedCallLoop(ctx)
	go node.pendingBurnLoop(ctx)

	go node.solanaRPCBlocksLoop(ctx)
	go node.addressLookupTableLoop(ctx)

	go node.refreshAssetsLoop(ctx)
}

func (node *Node) initMPCKeys(ctx context.Context) error {
	for {
		count, err := node.store.CountKeys(ctx)
		if err != nil {
			return err
		}
		if count >= node.conf.MPCKeyNumber {
			val, err := node.store.ReadProperty(ctx, store.UserInitializeTimeKey)
			if err != nil || val != "" {
				return err
			}
			err = node.InitializeAccount(ctx, node.getMTGAddress(ctx).String())
			if err != nil {
				return err
			}
			return node.writeRequestTime(ctx, store.UserInitializeTimeKey, time.Now())
		}

		requestAt := node.ReadPropertyAsTime(ctx, store.KeygenRequestTimeKey)
		if time.Since(requestAt) < time.Hour {
			time.Sleep(1 * time.Minute)
			continue
		}

		now := time.Now().UTC()
		for i := count; i < node.conf.MPCKeyNumber; i++ {
			id := common.UniqueId(node.group.GenesisId(), fmt.Sprintf("MPC:BASE:%d", i))
			id = common.UniqueId(id, now.String())
			extra := []byte{byte(i)}
			err = node.sendObserverTransactionToGroup(ctx, &common.Operation{
				Id:    id,
				Type:  OperationTypeKeygenInput,
				Extra: extra,
			}, nil)
			if err != nil {
				return err
			}
		}

		err = node.writeRequestTime(ctx, store.KeygenRequestTimeKey, now)
		if err != nil {
			return err
		}
	}
}

func (node *Node) sendPriceInfo(ctx context.Context) error {
	amount := decimal.RequireFromString(node.conf.OperationPriceAmount)
	logger.Printf("node.sendPriceInfo(%s, %s)", node.conf.OperationPriceAssetId, amount)
	amount = amount.Mul(decimal.New(1, 8))
	if amount.Sign() <= 0 || !amount.IsInteger() || !amount.BigInt().IsInt64() {
		panic(node.conf.OperationPriceAmount)
	}
	id := common.UniqueId("OperationTypeSetOperationParams", node.conf.OperationPriceAssetId)
	id = common.UniqueId(id, amount.String())
	id = common.UniqueId(id, node.group.GenesisId())
	extra := uuid.Must(uuid.FromString(node.conf.OperationPriceAssetId)).Bytes()
	extra = binary.BigEndian.AppendUint64(extra, uint64(amount.IntPart()))
	return node.sendObserverTransactionToGroup(ctx, &common.Operation{
		Id:    id,
		Type:  OperationTypeSetOperationParams,
		Extra: extra,
	}, nil)
}

func (node *Node) checkNonceAccounts(ctx context.Context) error {
	calls, err := node.store.CountUserSystemCallByState(ctx, common.RequestStateInitial)
	if err != nil {
		panic(err)
	}
	if calls > 0 {
		return nil
	}
	calls, err = node.store.CountUserSystemCallByState(ctx, common.RequestStatePending)
	if err != nil {
		panic(err)
	}
	if calls > 0 {
		return nil
	}
	ns, err := node.store.ListLockedNonceAccounts(ctx)
	if err != nil {
		panic(err)
	}
	if len(ns) > 0 {
		return nil
	}

	ns, err = node.store.ListNonceAccounts(ctx)
	if err != nil {
		panic(err)
	}
	for _, nonce := range ns {
		hash, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
		if err != nil {
			panic(err)
		}
		if hash.String() == nonce.Hash {
			continue
		}
		logger.Printf("solana.checkNonceAccount(%s) => %s %s", nonce.Account().Address, nonce.Account().Hash, hash.String())
		err = node.store.UpdateNonceAccount(ctx, nonce.Address, hash.String(), "")
		if err != nil {
			panic(err)
		}
	}
	return nil
}

func (node *Node) initializeUsersLoop(ctx context.Context) {
	for {
		err := node.initializeUsers(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) deployOrConfirmAssetsLoop(ctx context.Context) {
	for {
		err := node.deployOrConfirmAssets(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) createNonceAccountLoop(ctx context.Context) {
	for {
		err := node.createNonceAccounts(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) releaseNonceAccountLoop(ctx context.Context) {
	for {
		err := node.releaseNonceAccounts(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) feeInfoLoop(ctx context.Context) {
	for {
		err := node.handleFeeInfo(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(7 * time.Minute)
	}
}

func (node *Node) withdrawalFeeLoop(ctx context.Context) {
	for {
		err := node.handleWithdrawalsFee(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) unconfirmedWithdrawalLoop(ctx context.Context) {
	for {
		err := node.handleUnconfirmedWithdrawals(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) unconfirmedCallLoop(ctx context.Context) {
	for {
		err := node.handleUnconfirmedCalls(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) unsignedCallLoop(ctx context.Context) {
	for {
		err := node.processUnsignedCalls(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) signedCallLoop(ctx context.Context) {
	for {
		err := node.handleSignedCalls(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) pendingBurnLoop(ctx context.Context) {
	for {
		err := node.handlePendingBurns(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
	}
}

func (node *Node) refreshAssetsLoop(ctx context.Context) {
	for {
		err := node.refreshAssets(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(time.Minute)
	}
}

func (node *Node) initializeUsers(ctx context.Context) error {
	offset := node.ReadPropertyAsTime(ctx, store.UserInitializeTimeKey)
	us, err := node.store.ListNewUsersAfter(ctx, offset)
	if err != nil || len(us) == 0 {
		return err
	}

	for _, u := range us {
		err := node.InitializeAccount(ctx, u.ChainAddress)
		if err != nil {
			return err
		}
		err = node.writeRequestTime(ctx, store.UserInitializeTimeKey, u.CreatedAt)
		if err != nil {
			return err
		}
		time.Sleep(loopInterval)
	}
	return nil
}

func (node *Node) deployOrConfirmAssets(ctx context.Context) error {
	es, err := node.store.ListUndeployedAssets(ctx)
	if err != nil || len(es) == 0 {
		return err
	}

	for _, a := range es {
		old, err := node.store.ReadDeployedAsset(ctx, a.AssetId)
		if err != nil {
			return err
		}
		if old != nil {
			continue
		}

		id, tx, assets, err := node.CreateMintTransaction(ctx, a.AssetId)
		if err != nil || tx == nil {
			return err
		}
		payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
		_, err = tx.PartialSign(solanaApp.BuildSignersGetter(payer))
		if err != nil {
			panic(err)
		}
		rpcTx, err := node.SendTransactionUtilConfirm(ctx, tx, nil)
		if err != nil {
			return err
		}
		tx, err = rpcTx.Transaction.GetTransaction()
		if err != nil {
			return err
		}

		extra := []byte{byte(len(assets))}
		for _, asset := range assets {
			extra = append(extra, uuid.Must(uuid.FromString(asset.AssetId)).Bytes()...)
			extra = append(extra, solana.MustPublicKeyFromBase58(asset.Address).Bytes()...)
		}
		err = node.sendObserverTransactionToGroup(ctx, &common.Operation{
			Id:    id,
			Type:  OperationTypeDeployExternalAssets,
			Extra: extra,
		}, nil)
		if err != nil {
			return err
		}

		return node.store.MarkExternalAssetDeployed(ctx, assets, tx.Signatures[0].String())
	}
	return nil
}

func (node *Node) createNonceAccounts(ctx context.Context) error {
	count, err := node.store.CountNonceAccounts(ctx)
	if err != nil || count > 100 {
		return err
	}
	requested := node.ReadPropertyAsTime(ctx, store.NonceAccountRequestTimeKey)
	if requested.Add(10 * time.Second).After(time.Now().UTC()) {
		return nil
	}
	address, hash, err := node.CreateNonceAccount(ctx, count)
	if err != nil {
		return fmt.Errorf("node.CreateNonceAccount() => %v", err)
	}
	err = node.store.WriteNonceAccount(ctx, address, hash)
	if err != nil {
		return fmt.Errorf("store.WriteNonceAccount(%s %s) => %v", address, hash, err)
	}
	return node.writeRequestTime(ctx, store.NonceAccountRequestTimeKey, time.Now().UTC())
}

func (node *Node) releaseNonceAccounts(ctx context.Context) error {
	payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
	as, err := node.store.ListLockedNonceAccounts(ctx)
	if err != nil {
		return err
	}
	for _, nonce := range as {
		if nonce.LockedByUserOnly() && nonce.Expired() {
			node.releaseLockedNonceAccount(ctx, nonce)
			continue
		}

		call, err := node.store.ReadSystemCallByRequestId(ctx, nonce.CallId.String, 0)
		if err != nil {
			return err
		}
		if call == nil || call.State == common.RequestStateInitial {
			if nonce.Expired() {
				node.releaseLockedNonceAccount(ctx, nonce)
			}
			continue
		}
		if nonce.Address != call.NonceAccount {
			node.releaseLockedNonceAccount(ctx, nonce)
			continue
		}
		switch call.State {
		case common.RequestStateDone, common.RequestStateFailed:
		default:
			continue
		}

		// release nonce account that is occupied by unconfirmed solana tx
		if call.State == common.RequestStateFailed {
			tx, err := solana.TransactionFromBase64(call.Raw)
			if err != nil {
				panic(err)
			}
			_, err = tx.PartialSign(solanaApp.BuildSignersGetter(payer))
			if err != nil {
				panic(err)
			}
			rpcTx, err := node.RPCGetTransaction(ctx, tx.Signatures[0].String())
			if err != nil {
				panic(err)
			}
			if rpcTx == nil {
				node.releaseLockedNonceAccount(ctx, nonce)
				continue
			}
			if rpcTx.Meta.Err == nil {
				panic(tx.Signatures[0].String())
			}
		}

		if nonce.UpdatedBy.Valid && nonce.UpdatedBy.String == call.RequestId {
			node.releaseLockedNonceAccount(ctx, nonce)
			continue
		}
		for {
			newNonceHash, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
			if err != nil {
				panic(err)
			}
			if newNonceHash.String() == nonce.Hash {
				time.Sleep(3 * time.Second)
				continue
			}
			err = node.store.UpdateNonceAccount(ctx, nonce.Address, newNonceHash.String(), call.RequestId)
			if err != nil {
				panic(err)
			}
			break
		}
	}
	return nil
}

func (node *Node) releaseLockedNonceAccount(ctx context.Context, nonce *store.NonceAccount) {
	logger.Printf("observer.releaseLockedNonceAccount(%v)", nonce)
	hash, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
	if err != nil {
		panic(err)
	}
	if hash.String() != nonce.Hash {
		panic(fmt.Errorf("observer.releaseLockedNonceAccount(%s) => inconsistent hash %s %s ",
			nonce.Address, nonce.Hash, hash.String()))
	}
	err = node.store.ReleaseLockedNonceAccount(ctx, nonce.Address)
	if err != nil {
		panic(err)
	}
}

func (node *Node) handleFeeInfo(ctx context.Context) error {
	xin, err := common.SafeReadAssetUntilSufficient(ctx, common.XINKernelAssetId)
	if err != nil {
		return err
	}
	sol, err := common.SafeReadAssetUntilSufficient(ctx, common.SafeSolanaChainId)
	if err != nil {
		return err
	}
	xinPrice := decimal.RequireFromString(xin.PriceUSD)
	solPrice := decimal.RequireFromString(sol.PriceUSD)
	ratio := xinPrice.Div(solPrice)

	id := uuid.Must(uuid.NewV4()).String()
	return node.store.WriteFeeInfo(ctx, id, ratio)
}

func (node *Node) handleWithdrawalsFee(ctx context.Context) error {
	txs := node.group.ListUnconfirmedWithdrawalTransactions(ctx, 500)
	for _, tx := range txs {
		if !tx.Destination.Valid {
			panic(tx.TraceId)
		}
		asset, err := common.SafeReadAssetUntilSufficient(ctx, tx.AssetId)
		if err != nil {
			return err
		}
		if asset.ChainID != common.SafeSolanaChainId {
			continue
		}
		fee, err := common.SafeReadWithdrawalFeeUntilSufficient(ctx, node.SafeUser(), asset.AssetID, common.SafeSolanaChainId, tx.Destination.String)
		if err != nil {
			return err
		}
		if fee.AssetID != common.SafeSolanaChainId {
			panic(fee.AssetID)
		}
		rid := common.UniqueId(tx.TraceId, "withdrawal_fee")
		amount := decimal.RequireFromString(fee.Amount)
		refs := []crypto.Hash{tx.Hash}
		_, err = common.SendTransactionUntilSufficient(ctx, node.wallet, node.mixin, []string{mtg.MixinFeeUserId}, 1, amount, rid, fee.AssetID, "", refs, node.conf.MTG.App.SpendPrivateKey)
		if err != nil {
			return err
		}
	}
	return nil
}

func (node *Node) handleUnconfirmedWithdrawals(ctx context.Context) error {
	start := node.ReadPropertyAsTime(ctx, store.WithdrawalConfirmRequestTimeKey)
	txs := node.group.ListConfirmedWithdrawalTransactionsAfter(ctx, start, 100)
	for _, tx := range txs {
		if !tx.WithdrawalHash.Valid {
			return fmt.Errorf("invalid withdrawal hash: %s", tx.TraceId)
		}
		cid := uuid.Must(uuid.FromString(tx.Memo)).String()
		call, err := node.store.ReadSystemCallByRequestId(ctx, cid, common.RequestStatePending)
		if err != nil || call == nil {
			return fmt.Errorf("store.ReadSystemCallByRequestId(%s %d) => %v %v", cid, common.RequestStatePending, call, err)
		}

		err = node.store.WriteConfirmedWithdrawal(ctx, &store.ConfirmedWithdrawal{
			Hash:      tx.WithdrawalHash.String,
			TraceId:   tx.TraceId,
			CallId:    call.RequestId,
			CreatedAt: tx.UpdatedAt,
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (node *Node) handleUnconfirmedCalls(ctx context.Context) error {
	calls, err := node.store.ListUnconfirmedSystemCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		logger.Printf("observer.handleUnconfirmedCall(%s)", call.RequestId)
		nonce, err := node.store.ReadNonceAccount(ctx, call.NonceAccount)
		if err != nil {
			return err
		}

		id := common.UniqueId(call.RequestId, "confirm-nonce")
		extra := []byte{ConfirmFlagNonceAvailable}
		extra = append(extra, uuid.Must(uuid.FromString(call.RequestId)).Bytes()...)

		if nonce == nil || !nonce.Valid(call.RequestId) {
			logger.Printf("observer.expireSystemCall(%v %v %v)", call, nonce, err)
			id = common.UniqueId(id, "expire-nonce")
			extra[0] = ConfirmFlagNonceExpired
			err = node.store.WriteFailedCallIfNotExist(ctx, call, "expired or invalid nonce")
			if err != nil {
				return err
			}
		} else {
			err := node.OccupyNonceAccountByCall(ctx, nonce, call.RequestId)
			if err != nil {
				return err
			}

			cid := common.UniqueId(id, "storage")
			fee, err := node.getSystemCallFeeFromXIN(ctx, call)
			if err != nil {
				return err
			}
			nonce = node.ReadSpareNonceAccountWithCall(ctx, cid)
			tx, err := node.CreatePrepareTransaction(ctx, call, nonce, fee)
			if err != nil {
				return err
			}
			if tx != nil {
				err := node.OccupyNonceAccountByCall(ctx, nonce, cid)
				if err != nil {
					return err
				}
				tb, err := tx.MarshalBinary()
				if err != nil {
					panic(err)
				}
				extra = attachSystemCall(extra, cid, tb)
			}
		}

		err = node.sendObserverTransactionToGroup(ctx, &common.Operation{
			Id:    id,
			Type:  OperationTypeConfirmNonce,
			Extra: extra,
		}, nil)
		if err != nil {
			return err
		}
		logger.Printf("observer.confirmNonce(%s %d %d)", call.RequestId, OperationTypeConfirmNonce, extra[0])
	}
	return nil
}

func (node *Node) processUnsignedCalls(ctx context.Context) error {
	calls, err := node.store.ListUnsignedCalls(ctx)
	if err != nil {
		return err
	}
	for _, call := range calls {
		now := time.Now().UTC()
		if call.RequestSignerAt.Valid && call.RequestSignerAt.Time.Add(20*time.Minute).After(now) {
			continue
		}
		logger.Printf("observer.processUnsignedCalls(%s %d)", call.RequestId, len(calls))
		offset := call.CreatedAt
		if call.RequestSignerAt.Valid {
			offset = call.RequestSignerAt.Time
		}
		id := common.UniqueId(call.RequestId, offset.String())
		extra := uuid.Must(uuid.FromString(call.RequestId)).Bytes()
		err = node.sendObserverTransactionToGroup(ctx, &common.Operation{
			Id:    id,
			Type:  OperationTypeSignInput,
			Extra: extra,
		}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

func (node *Node) handleSignedCalls(ctx context.Context) error {
	balance, err := node.solana.RPCGetBalance(ctx, node.SolanaPayer())
	if err != nil {
		return err
	}
	if balance < BalanceLimit {
		logger.Printf("insufficient balance to send tx: %d", balance)
		time.Sleep(30 * time.Second)
		return nil
	}

	callSequence, err := node.store.ListSignedCalls(ctx)
	if err != nil {
		return err
	}
	var wg sync.WaitGroup
	for key, calls := range callSequence {
		wg.Add(1)
		go node.handleSignedCallSequence(ctx, &wg, key, calls)
	}
	wg.Wait()
	return nil
}

func (node *Node) handleSignedCallSequence(ctx context.Context, wg *sync.WaitGroup, key string, calls []*store.SystemCall) {
	defer wg.Done()
	var ids []string
	for _, c := range calls {
		ids = append(ids, c.RequestId)
	}
	logger.Printf("node.handleSignedCallSequence(%s) => %s", key, strings.Join(ids, ","))
	if len(calls) > 2 {
		panic(fmt.Errorf("invalid call sequence length: %s", strings.Join(ids, ",")))
	}

	for _, c := range calls {
		if c.Type != store.CallTypeMain {
			continue
		}
		// should wait withdrawals getting confirmed
		unconfirmed, err := node.store.CheckUnconfirmedWithdrawals(ctx, c)
		if err != nil {
			panic(err)
		}
		if unconfirmed {
			return
		}
		// should be processed after previous main calls from the same user being confirmed
		previous, err := node.store.CheckUnfinishedPreviousMainCalls(ctx, c)
		if err != nil {
			panic(err)
		}
		if previous {
			return
		}
		// should be processed with its prepare call
		needPrepare, err := node.store.CheckUnfinishedSubCalls(ctx, c)
		if err != nil {
			panic(err)
		}
		if needPrepare && len(calls) != 2 {
			return
		}
	}

	if len(calls) == 1 {
		call := calls[0]
		tx, meta, err := node.handleSignedCall(ctx, call)
		if err != nil {
			err = node.processFailedCall(ctx, call, err)
			if err != nil {
				panic(err)
			}
			return
		}
		err = node.processSuccessedCall(ctx, call, tx, meta, []solana.Signature{tx.Signatures[0]})
		if err != nil {
			panic(err)
		}
		return
	}

	var sigs []solana.Signature
	preTx, _, err := node.handleSignedCall(ctx, calls[0])
	if err != nil {
		err = node.processFailedCall(ctx, calls[0], err)
		if err != nil {
			panic(err)
		}
		return
	}
	sigs = append(sigs, preTx.Signatures[0])

	err = node.checkCreatedAtaUntilSufficient(ctx, preTx)
	if err != nil {
		panic(err)
	}

	tx, meta, err := node.handleSignedCall(ctx, calls[1])
	if err != nil {
		err = node.processFailedCall(ctx, calls[1], err)
		if err != nil {
			panic(err)
		}
		return
	}
	sigs = append(sigs, tx.Signatures[0])

	err = node.processSuccessedCall(ctx, calls[1], tx, meta, sigs)
	if err != nil {
		panic(err)
	}
}

func (node *Node) handleSignedCall(ctx context.Context, call *store.SystemCall) (*solana.Transaction, *rpc.TransactionMeta, error) {
	logger.Printf("node.handleSignedCall(%s)", call.RequestId)
	payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
	publicKey := node.getUserSolanaPublicKeyFromCall(ctx, call)
	tx, err := solana.TransactionFromBase64(call.Raw)
	if err != nil {
		panic(err)
	}
	err = node.processTransactionWithAddressLookups(ctx, tx)
	if err != nil {
		panic(err)
	}
	_, err = tx.PartialSign(solanaApp.BuildSignersGetter(payer))
	if err != nil {
		panic(err)
	}

	index, err := solanaApp.GetSignatureIndexOfAccount(*tx, publicKey)
	if err != nil {
		panic(err)
	}
	if index >= 0 {
		sig, err := base64.StdEncoding.DecodeString(call.Signature.String)
		if err != nil {
			panic(err)
		}
		tx.Signatures[index] = solana.SignatureFromBytes(sig)
	}

	rpcTx, err := node.SendTransactionUtilConfirm(ctx, tx, call)
	if err != nil || rpcTx == nil {
		return nil, nil, fmt.Errorf("node.SendTransactionUtilConfirm(%s) => %v %v", call.RequestId, rpcTx, err)
	}
	txx, err := rpcTx.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	return txx, rpcTx.Meta, nil
}

// deposited assets to run system call and new assets received in system call are all handled here
func (node *Node) processSuccessedCall(ctx context.Context, call *store.SystemCall, txx *solana.Transaction, meta *rpc.TransactionMeta, hashes []solana.Signature) error {
	logger.Printf("node.processSuccessedCall(%s)", call.RequestId)
	id := common.UniqueId(call.RequestId, "confirm-success")
	extra := []byte{FlagConfirmCallSuccess}
	extra = append(extra, byte(len(hashes)))
	for _, hash := range hashes {
		extra = append(extra, hash[:]...)
	}

	if call.Type == store.CallTypeMain && !call.SkipPostProcess {
		cid := common.UniqueId(id, "post-process")
		nonce := node.ReadSpareNonceAccountWithCall(ctx, cid)
		tx := node.CreatePostProcessTransaction(ctx, call, nonce, txx, meta)
		if tx != nil {
			err := node.OccupyNonceAccountByCall(ctx, nonce, cid)
			if err != nil {
				return err
			}
			data, err := tx.MarshalBinary()
			if err != nil {
				panic(err)
			}
			extra = attachSystemCall(extra, cid, data)
		}
	}

	op := &common.Operation{
		Id:    id,
		Type:  OperationTypeConfirmCall,
		Extra: extra,
	}
	switch call.Type {
	case store.CallTypeDeposit, store.CallTypePostProcess:
		return node.handleBurnSystemCall(ctx, call, op)
	default:
		return node.sendObserverTransactionToGroup(ctx, op, nil)
	}
}

func (node *Node) processFailedCall(ctx context.Context, call *store.SystemCall, callError error) error {
	logger.Printf("node.processFailedCall(%s)", call.RequestId)
	id := common.UniqueId(call.RequestId, "confirm-fail")
	cid := common.UniqueId(id, "post-process")
	nonce := node.ReadSpareNonceAccountWithCall(ctx, cid)
	extra := []byte{FlagConfirmCallFail}
	extra = append(extra, uuid.Must(uuid.FromString(call.RequestId)).Bytes()...)

	var tx *solana.Transaction
	switch call.Type {
	case store.CallTypeMain:
		tx = node.CreatePostProcessTransaction(ctx, call, nonce, nil, nil)
	case store.CallTypePrepare:
		superior, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
		if err != nil {
			panic(err)
		}
		tx = node.CreateRefundWithdrawalTransaction(ctx, call, superior, nonce)
	}
	if tx != nil {
		err := node.OccupyNonceAccountByCall(ctx, nonce, cid)
		if err != nil {
			return err
		}
		data, err := tx.MarshalBinary()
		if err != nil {
			panic(err)
		}
		extra = attachSystemCall(extra, cid, data)
	}

	err := node.store.WriteFailedCallIfNotExist(ctx, call, callError.Error())
	if err != nil {
		return err
	}

	return node.sendObserverTransactionToGroup(ctx, &common.Operation{
		Id:    id,
		Type:  OperationTypeConfirmCall,
		Extra: extra,
	}, nil)
}

func (node *Node) handleBurnSystemCall(ctx context.Context, call *store.SystemCall, op *common.Operation) error {
	tx, err := solana.TransactionFromBase64(call.Raw)
	if err != nil {
		panic(err)
	}
	bs := solanaApp.ExtractBurnsFromTransaction(ctx, tx)
	// no burn instruction in system call
	// all assets are Solana native assets
	// would be handled when mtg receives the deposit from Solana
	if len(bs) == 0 {
		return node.sendObserverTransactionToGroup(ctx, op, nil)
	}

	var assets []string
	for _, burn := range bs {
		address := burn.GetMintAccount().PublicKey.String()
		da, err := node.store.ReadDeployedAssetByAddress(ctx, address)
		if err != nil || da == nil {
			panic(err)
		}
		assets = append(assets, da.AssetId)
	}
	return node.store.WritePendingBurnSystemCallIfNotExists(ctx, call, op, assets)
}

func (node *Node) handlePendingBurns(ctx context.Context) error {
	cs, am, err := node.store.ListPendingBurnSystemCalls(ctx)
	if err != nil {
		return err
	}
	for _, c := range cs {
		pending, err := node.store.CheckPendingBurnSystemCalls(ctx, c, am[c.Id])
		if err != nil {
			panic(err)
		}
		if pending {
			logger.Printf("store.CheckPendingBurnSystemCalls(%s)", c.Id)
			continue
		}
		call, err := node.store.ReadSystemCallByRequestId(ctx, c.Id, 0)
		if err != nil || call == nil {
			panic(fmt.Errorf("store.ReadSystemCallByRequestId(%s) => %v %v", c.Id, call, err))
		}
		err = node.confirmBurnRelatedSystemCallToGroup(ctx, &common.Operation{
			Id:    c.RequestId,
			Extra: c.ExtraBytes(),
			Type:  OperationTypeConfirmCall,
		}, call)
		if err != nil {
			return err
		}
	}

	return nil
}

func (node *Node) ReadSpareNonceAccountWithCall(ctx context.Context, cid string) *store.NonceAccount {
	nonce, err := node.store.ReadNonceAccountByCall(ctx, cid)
	if err != nil {
		panic(err)
	}
	if nonce != nil {
		return nonce
	}
	nonce, err = node.store.ReadSpareNonceAccount(ctx)
	if err != nil || nonce == nil {
		panic(fmt.Errorf("store.ReadSpareNonceAccount() => %v %v", nonce, err))
	}
	return nonce
}

func (node *Node) OccupyNonceAccountByCall(ctx context.Context, nonce *store.NonceAccount, cid string) error {
	if nonce.CallId.Valid {
		if nonce.CallId.String == cid {
			return nil
		}
		panic(fmt.Errorf("inconsistent call id: %s %s", nonce.CallId.String, cid))
	}
	return node.store.OccupyNonceAccountByCall(ctx, nonce.Address, cid)
}

func (node *Node) refreshAssets(ctx context.Context) error {
	ids, err := node.store.ListExternalAssetIds(ctx)
	if err != nil {
		return err
	}
	as, err := common.SafeReadAssetsUntilSufficient(ctx, ids, node.SafeUser())
	if err != nil {
		return err
	}
	return node.store.UpdateExternalAssetsInfo(ctx, as)
}

func (node *Node) checkSufficientBalanceForBurnSystemCall(ctx context.Context, call *store.SystemCall) bool {
	tx, err := solana.TransactionFromBase64(call.Raw)
	if err != nil {
		panic(err)
	}
	bs := solanaApp.ExtractBurnsFromTransaction(ctx, tx)
	if len(bs) == 0 {
		panic(call)
	}
	for _, burn := range bs {
		address := burn.GetMintAccount().PublicKey.String()
		da, err := node.store.ReadDeployedAssetByAddress(ctx, address)
		if err != nil || da == nil {
			panic(err)
		}
		amount := decimal.New(int64(*burn.Amount), -int32(da.Decimals))
		balance := node.getMtgAssetUnspentBalance(ctx, da.AssetId)
		if balance.Cmp(amount) < 0 {
			logger.Printf("insufficient balance to confirm burn system call: %s %s %s %s", call.RequestId, da.AssetId, amount.String(), balance.String())
			return false
		}
	}
	return true
}

func (node *Node) getMtgAssetUnspentAndPendingBalance(ctx context.Context, assetId string) decimal.Decimal {
	unspent := node.getMtgAssetUnspentBalance(ctx, assetId)

	os := node.group.ListOutputsForAsset(ctx, node.conf.AppId, assetId, node.conf.MTG.Genesis.Epoch, math.MaxInt64, mtg.SafeUtxoStateAssigned, 0)
	for _, o := range os {
		unspent = unspent.Add(o.Amount)
	}
	os = node.group.ListOutputsForAsset(ctx, node.conf.AppId, assetId, node.conf.MTG.Genesis.Epoch, math.MaxInt64, mtg.SafeUtxoStateSigned, 0)
	for _, o := range os {
		unspent = unspent.Add(o.Amount)
	}
	return unspent
}

func (node *Node) getMtgAssetUnspentBalance(ctx context.Context, assetId string) decimal.Decimal {
	os := node.group.ListOutputsForAsset(ctx, node.conf.AppId, assetId, node.conf.MTG.Genesis.Epoch, math.MaxInt64, mtg.SafeUtxoStateUnspent, 0)
	total := decimal.NewFromInt(0)
	for _, o := range os {
		total = total.Add(o.Amount)
	}
	return total
}
