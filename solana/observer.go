package solana

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/MixinNetwork/safe/util"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
)

const (
	loopInterval     = time.Second
	mpcRetryInterval = time.Minute * 5
	BalanceLimit     = 500000000
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
	go node.notificationLoop(ctx)
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
		hash, slot, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
		if err != nil {
			panic(err)
		}
		if hash == nil {
			return fmt.Errorf("nonce account missing: %s", nonce.Address)
		}
		if slot < nonce.HashSlot || hash.String() == nonce.Hash {
			continue
		}
		logger.Printf("solana.checkNonceAccount(%s) => %s %s", nonce.Account().Address, nonce.Account().Hash, hash.String())
		err = node.store.UpdateNonceAccount(ctx, nonce, hash.String(), slot, "")
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
			logger.Printf("node.deployOrConfirmAssets() => %v", err)
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
			logger.Printf("node.releaseNonceAccounts() => %v", err)
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
		if nonceRetryable(err) {
			logger.Printf("prepare calls waiting for a spare nonce")
		} else if err != nil {
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

		time.Sleep(loopInterval)
	}
}

func (node *Node) notificationLoop(ctx context.Context) {
	for {
		err := node.sendTransferNotification(ctx)
		if err != nil {
			panic(err)
		}

		time.Sleep(loopInterval)
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
		time.Sleep(time.Second)
		old, err := node.store.ReadDeployedAsset(ctx, a.AssetId)
		if err != nil {
			return err
		}
		if old != nil {
			continue
		}

		err = node.deployExternalAsset(ctx, a)
		if err != nil {
			logger.Printf("deploy external asset %s => %v", a.AssetId, err)
			continue
		}
	}
	return nil
}

func (node *Node) deployExternalAsset(ctx context.Context, a *store.ExternalAsset) error {
	id, tx, asset, err := node.CreateMintTransaction(ctx, a.AssetId)
	if err != nil {
		return err
	}
	if tx == nil {
		return fmt.Errorf("missing mint transaction for %s", a.AssetId)
	}
	mint, err := node.solana.RPCGetConfirmedAsset(ctx, asset.Address)
	if err != nil {
		return err
	}
	var hash string
	if mint != nil {
		if err := validateExternalAssetMint(asset.Address, node.getMTGAddress(ctx).String(), mint); err != nil {
			return err
		}
		hash, err = node.solana.RPCGetMintCreationSignature(ctx, asset.Address)
		if err != nil {
			return err
		}
	} else {
		payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
		_, err = tx.PartialSign(solanaApp.BuildSignersGetter(payer))
		if err != nil {
			return err
		}
		rpcTx, err := node.SendTransactionUtilConfirm(ctx, tx, nil)
		if err != nil {
			return err
		}
		tx, err = rpcTx.Transaction.GetTransaction()
		if err != nil {
			return err
		}
		hash = tx.Signatures[0].String()
	}
	extra := []byte{1}
	extra = append(extra, uuid.Must(uuid.FromString(asset.AssetId)).Bytes()...)
	extra = append(extra, solana.MustPublicKeyFromBase58(asset.Address).Bytes()...)
	err = node.sendObserverTransactionToGroup(ctx, &common.Operation{
		Id: id, Type: OperationTypeDeployExternalAssets, Extra: extra,
	}, nil)
	if err != nil {
		return err
	}
	return node.store.MarkExternalAssetDeployed(ctx, []*solanaApp.DeployedAsset{asset}, hash)
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
	address, hash, slot, err := node.CreateNonceAccount(ctx, count)
	if err != nil {
		return fmt.Errorf("node.CreateNonceAccount() => %v", err)
	}
	err = node.store.WriteNonceAccount(ctx, address, hash, slot)
	if err != nil {
		return fmt.Errorf("store.WriteNonceAccount(%s %s) => %v", address, hash, err)
	}
	return node.writeRequestTime(ctx, store.NonceAccountRequestTimeKey, time.Now().UTC())
}

func (node *Node) releaseNonceAccounts(ctx context.Context) error {
	// Catch up accepted proposals before classifying missing calls as abandoned.
	if !node.synced(ctx) {
		return nil
	}
	accounts, err := node.store.ListLockedNonceAccounts(ctx)
	if err != nil {
		return err
	}
	var failures []error
	for _, nonce := range accounts {
		if err := node.releaseNonceAccount(ctx, nonce); err != nil {
			failures = append(failures, fmt.Errorf("nonce %s: %w", nonce.Address, err))
		}
	}
	return errors.Join(failures...)
}

func (node *Node) releaseNonceAccount(ctx context.Context, nonce *store.NonceAccount) error {
	if nonce.LockedByUserOnly() && nonce.Expired() {
		return node.releaseLockedNonceAccount(ctx, nonce)
	}
	call, err := node.store.ReadSystemCallByRequestId(ctx, nonce.CallId.String, 0)
	if err != nil {
		return err
	}
	// An accepted Main keeps its nonce while it waits for Prepare.
	if call != nil && call.Type == store.CallTypeMain && call.State == common.RequestStateInitial && call.NonceAccount == nonce.Address {
		return nil
	}
	if call == nil || call.State == common.RequestStateInitial {
		if nonce.Expired() {
			return node.releaseLockedNonceAccount(ctx, nonce)
		}
		return nil
	}
	if nonce.Address != call.NonceAccount {
		return node.releaseLockedNonceAccount(ctx, nonce)
	}
	switch call.State {
	case common.RequestStatePending, common.RequestStateDone, common.RequestStateFailed:
	default:
		return nil
	}

	hash, slot, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
	if err != nil {
		return err
	}
	if hash == nil {
		return fmt.Errorf("nonce account missing: %s", nonce.Address)
	}
	if slot < nonce.HashSlot {
		return fmt.Errorf("%w: nonce RPC slot %d < %d", store.ErrNonceAccountChanged, slot, nonce.HashSlot)
	}
	if hash.String() != nonce.Hash {
		// The finalized nonce has advanced, including on execution failure.
		// Free it even if MTG confirmation is waiting for a cleanup nonce.
		return node.store.UpdateNonceAccount(ctx, nonce, hash.String(), slot, call.RequestId)
	}
	if call.State == common.RequestStatePending {
		return nil
	}
	if nonce.UpdatedBy.Valid && nonce.UpdatedBy.String == call.RequestId {
		return node.store.ReleaseNonceAccountAfterCall(ctx, nonce, call)
	}
	if call.State == common.RequestStateDone {
		// Finalized account state can lag transaction confirmation. Check the
		// remaining accounts now, then retry this one on the next pass.
		return nil
	}

	// A failed Main may never have been submitted (for example, Prepare
	// failed first). Only that case can release an unchanged nonce.
	tx, err := solana.TransactionFromBase64(call.Raw)
	if err != nil {
		return err
	}
	payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
	if _, err := tx.PartialSign(solanaApp.BuildSignersGetter(payer)); err != nil {
		return err
	}
	rpcTx, err := node.RPCGetTransaction(ctx, tx.Signatures[0].String())
	if err != nil {
		return err
	}
	if rpcTx == nil {
		return node.store.ReleaseNonceAccountAfterCall(ctx, nonce, call)
	}
	if rpcTx.Meta.Err == nil {
		return fmt.Errorf("successful transaction for failed call: %s", call.RequestId)
	}
	return nil
}

func (node *Node) releaseLockedNonceAccount(ctx context.Context, nonce *store.NonceAccount) error {
	logger.Printf("observer.releaseLockedNonceAccount(%v)", nonce)
	hash, slot, err := node.solana.GetNonceAccountHash(ctx, nonce.Account().Address)
	if err != nil {
		return err
	}
	if hash == nil {
		return fmt.Errorf("nonce account missing: %s", nonce.Address)
	}
	if slot < nonce.HashSlot {
		return fmt.Errorf("%w: nonce RPC slot %d < %d", store.ErrNonceAccountChanged, slot, nonce.HashSlot)
	}
	if hash.String() != nonce.Hash {
		// Re-read on the next pass rather than clearing an uncertain owner.
		return fmt.Errorf("inconsistent nonce hash: %s %s %s", nonce.Address, nonce.Hash, hash.String())
	}
	return node.store.ReleaseLockedNonceAccount(ctx, nonce)
}

func (node *Node) handleFeeInfo(ctx context.Context) error {
	xin, err := common.SafeReadAssetUntilSufficient(ctx, common.XINKernelAssetId, node.SafeUser())
	if err != nil {
		return err
	}
	sol, err := common.SafeReadAssetUntilSufficient(ctx, common.SafeSolanaChainId, node.SafeUser())
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
		asset, err := common.SafeReadAssetUntilSufficient(ctx, tx.AssetId, node.SafeUser())
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
		// Leave cleanup capacity for the earlier Main that this call must wait for.
		previous, err := node.store.CheckUnfinishedPreviousMainCalls(ctx, call)
		if err != nil {
			return err
		}
		if previous {
			continue
		}
		user, err := node.store.ReadUser(ctx, call.UserIdFromPublicPath())
		if err != nil {
			return err
		}
		nonce, err := node.store.ReadNonceAccount(ctx, call.NonceAccount)
		if err != nil {
			return err
		}

		id := common.UniqueId(call.RequestId, "confirm-nonce")
		extra := []byte{ConfirmFlagNonceAvailable}
		extra = append(extra, uuid.Must(uuid.FromString(call.RequestId)).Bytes()...)

		failureReason := ""
		retryId := ""
		var proposalNonce *store.NonceAccount
		if user == nil || nonce == nil || !nonce.Valid(call.RequestId) || nonce.Mix.String != user.MixAddress {
			failureReason = "expired or invalid nonce"
		} else {
			req, err := node.store.ReadRequestByHash(ctx, call.RequestHash)
			if err != nil {
				return err
			}
			os, _, err := node.GetSystemCallReferenceOutputs(ctx, call.UserIdFromPublicPath(), call.RequestHash, common.RequestStatePending)
			if err != nil {
				return err
			}
			err = node.validateSystemCallParameters(ctx, req, os)
			if err != nil {
				failureReason = err.Error()
			}
		}

		if failureReason == "" {
			nonceHash := solana.MustHashFromBase58(nonce.Hash)
			extra = append(extra, nonceHash[:]...)
			err = node.OccupyNonceAccountByCall(ctx, nonce, call.RequestId)
			if errors.Is(err, store.ErrNonceAccountChanged) {
				continue // Revalidate the current reservation on the next pass.
			}
			if err != nil {
				return err
			}
			fee, err := node.getSystemCallFeeFromXIN(ctx, call)
			if err != nil {
				return err
			}
			cid := common.UniqueId(id, "storage")
			prepareNonce, err := node.store.ReserveNonceAccountForCall(ctx, cid)
			if err != nil {
				return err
			}
			sent, retry, err := node.resendNonceProposal(ctx, id, prepareNonce)
			if err != nil {
				return err
			} else if sent {
				continue
			}
			retryId = retry
			prepareTx, err := node.CreatePrepareTransaction(ctx, call, prepareNonce, fee)
			if errors.Is(err, solanaApp.ErrTransactionTooLarge) {
				failureReason = err.Error()
			} else if err != nil {
				return err
			}

			if prepareTx != nil {
				proposalNonce = prepareNonce
				tb, err := prepareTx.MarshalBinary()
				if err != nil {
					panic(err)
				}
				extra = attachSystemCall(extra, cid, tb)
			} else if err := node.store.ReleaseNonceAccountForCall(ctx, prepareNonce.Address, cid); err != nil {
				return err
			}
		}

		if failureReason != "" {
			logger.Printf("observer.expireSystemCall(%v %v %s)", call, nonce, failureReason)
			id = common.UniqueId(id, "expire-nonce")
			extra[0] = ConfirmFlagNonceExpired
			err = node.store.WriteFailedCallIfNotExist(ctx, call, failureReason)
			if err != nil {
				return err
			}
		}

		op := &common.Operation{
			Id:    id,
			Type:  OperationTypeConfirmNonce,
			Extra: extra,
		}
		if proposalNonce != nil {
			err = node.sendObserverTransactionWithNonce(ctx, op, proposalNonce)
		} else {
			if retryId != "" {
				op.Id = common.UniqueId(op.Id, retryId)
			}
			err = node.sendObserverTransactionToGroup(ctx, op, nil)
		}
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
		if call.RequestSignerAt.Valid && call.RequestSignerAt.Time.Add(mpcRetryInterval).After(now) {
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
		wg.Go(func() {
			err := node.handleSignedCallSequence(ctx, key, calls)
			if nonceRetryable(err) {
				logger.Printf("cleanup for %s waiting for a spare nonce", key)
			} else if err != nil {
				panic(err)
			}
		})
	}
	wg.Wait()
	return nil
}

func (node *Node) handleSignedCallSequence(ctx context.Context, key string, calls []*store.SystemCall) error {
	var ids []string
	for _, c := range calls {
		ids = append(ids, c.RequestId)
		tx, err := solana.TransactionFromBase64(c.Raw)
		if err != nil {
			panic(fmt.Errorf("solana.TransactionFromBase64(%s) => %v", c.RequestId, err))
		}
		err = solanaApp.ValidateWireTransaction(tx)
		if errors.Is(err, solanaApp.ErrTransactionTooLarge) {
			logger.Printf("node.handleSignedCallSequence(%s) => skip oversized call %s: %v", key, c.RequestId, err)
			return nil
		}
		if err != nil {
			panic(fmt.Errorf("solana.ValidateWireTransaction(%s) => %v", c.RequestId, err))
		}
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
			return nil
		}
		// should be processed after previous main calls from the same user being confirmed
		previous, err := node.store.CheckUnfinishedPreviousMainCalls(ctx, c)
		if err != nil {
			panic(err)
		}
		if previous {
			return nil
		}
		// should be processed with its prepare call
		needPrepare, err := node.store.CheckUnfinishedSubCalls(ctx, c)
		if err != nil {
			panic(err)
		}
		if needPrepare && len(calls) != 2 {
			return nil
		}
	}

	if len(calls) == 1 {
		call := calls[0]
		tx, meta, err := node.handleSignedCall(ctx, call)
		if err != nil {
			return node.processFailedCall(ctx, call, tx.Signatures[0], nil, err)
		}
		return node.processSuccessedCall(ctx, call, tx, meta, []confirmCallRecord{{
			Status:    FlagConfirmCallSuccess,
			CallId:    call.RequestId,
			Signature: tx.Signatures[0],
		}})
	}

	var sigs []solana.Signature
	preTx, _, err := node.handleSignedCall(ctx, calls[0])
	if err != nil {
		return node.processFailedCall(ctx, calls[0], preTx.Signatures[0], nil, err)
	}
	sigs = append(sigs, preTx.Signatures[0])

	err = node.checkCreatedAtaUntilSufficient(ctx, preTx)
	if err != nil {
		panic(err)
	}

	tx, meta, err := node.handleSignedCall(ctx, calls[1])
	if err != nil {
		return node.processFailedCall(ctx, calls[1], tx.Signatures[0], []confirmCallRecord{{
			Status:    FlagConfirmCallSuccess,
			CallId:    calls[0].RequestId,
			Signature: preTx.Signatures[0],
		}}, err)
	}
	sigs = append(sigs, tx.Signatures[0])

	return node.processSuccessedCall(ctx, calls[1], tx, meta, []confirmCallRecord{
		{Status: FlagConfirmCallSuccess, CallId: calls[0].RequestId, Signature: sigs[0]},
		{Status: FlagConfirmCallSuccess, CallId: calls[1].RequestId, Signature: sigs[1]},
	})
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
	if tx.IsSigner(publicKey) && index >= 0 {
		sig, err := base64.StdEncoding.DecodeString(call.Signature.String)
		if err != nil {
			panic(err)
		}
		tx.Signatures[index] = solana.SignatureFromBytes(sig)
	}
	rb, err := tx.MarshalBinary()
	if err != nil {
		panic(err)
	}

	rpcTx, err := node.SendTransactionUtilConfirm(ctx, tx, call)
	logger.Printf("node.SendTransactionUtilConfirm(%s, %x) => %v %v", call.RequestId, rb, rpcTx, err)
	if err != nil || rpcTx == nil {
		return tx, nil, fmt.Errorf("node.SendTransactionUtilConfirm(%s) => %v %v", call.RequestId, rpcTx, err)
	}
	txx, err := rpcTx.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	return txx, rpcTx.Meta, nil
}

// deposited assets to run system call and new assets received in system call are all handled here
func (node *Node) processSuccessedCall(ctx context.Context, call *store.SystemCall, txx *solana.Transaction, meta *rpc.TransactionMeta, records []confirmCallRecord) error {
	logger.Printf("node.processSuccessedCall(%s)", call.RequestId)
	id := common.UniqueId(call.RequestId, "confirm-success")
	retryId := ""
	extra := encodeConfirmCallRecords(records)
	var proposalNonce *store.NonceAccount

	if call.Type == store.CallTypeMain && !call.SkipPostProcess {
		cid := common.UniqueId(id, "post-process")
		nonce, err := node.store.ReserveNonceAccountForCall(ctx, cid)
		if err != nil {
			return err
		}
		sent, retry, err := node.resendNonceProposal(ctx, id, nonce)
		if sent || err != nil {
			return err
		}
		retryId = retry
		tx := node.CreatePostProcessTransaction(ctx, call, nonce, txx, meta)
		if tx != nil {
			proposalNonce = nonce
			data, err := tx.MarshalBinary()
			if err != nil {
				panic(err)
			}
			extra = attachSystemCall(extra, cid, data)
		} else if err := node.store.ReleaseNonceAccountForCall(ctx, nonce.Address, cid); err != nil {
			return err
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
		if proposalNonce != nil {
			return node.sendObserverTransactionWithNonce(ctx, op, proposalNonce)
		}
		if retryId != "" {
			op.Id = common.UniqueId(op.Id, retryId)
		}
		return node.sendObserverTransactionToGroup(ctx, op, nil)
	}
}

func (node *Node) processFailedCall(ctx context.Context, call *store.SystemCall, signature solana.Signature, successful []confirmCallRecord, callError error) error {
	logger.Printf("node.processFailedCall(%s)", call.RequestId)
	// Builders use the same confirmed balance snapshot as confirmation replay.
	failed := *call
	failed.Hash = sql.NullString{Valid: true, String: signature.String()}
	call = &failed
	id := common.UniqueId(call.RequestId, "confirm-fail")
	retryId := ""
	cid := common.UniqueId(id, "post-process")
	records := append(successful, confirmCallRecord{
		Status:    FlagConfirmCallFail,
		CallId:    call.RequestId,
		Signature: signature,
	})
	extra := encodeConfirmCallRecords(records)
	var proposalNonce *store.NonceAccount

	if call.Type == store.CallTypeMain || call.Type == store.CallTypePrepare {
		nonce, err := node.store.ReserveNonceAccountForCall(ctx, cid)
		if err != nil {
			return err
		}
		sent, retry, err := node.resendNonceProposal(ctx, id, nonce)
		if sent || err != nil {
			return err
		}
		retryId = retry
		var tx *solana.Transaction
		if call.Type == store.CallTypeMain {
			tx = node.CreatePostProcessTransaction(ctx, call, nonce, nil, nil)
		} else {
			superior, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
			if err != nil {
				panic(err)
			}
			tx = node.CreateRefundWithdrawalTransaction(ctx, call, superior, nonce)
		}
		if tx != nil {
			proposalNonce = nonce
			data, err := tx.MarshalBinary()
			if err != nil {
				panic(err)
			}
			extra = attachSystemCall(extra, cid, data)
		} else if err := node.store.ReleaseNonceAccountForCall(ctx, nonce.Address, cid); err != nil {
			return err
		}
	}

	err := node.store.WriteFailedCallIfNotExist(ctx, call, callError.Error())
	if err != nil {
		return err
	}

	op := &common.Operation{
		Id:    id,
		Type:  OperationTypeConfirmCall,
		Extra: extra,
	}
	if proposalNonce != nil {
		return node.sendObserverTransactionWithNonce(ctx, op, proposalNonce)
	}
	if retryId != "" {
		op.Id = common.UniqueId(op.Id, retryId)
	}
	return node.sendObserverTransactionToGroup(ctx, op, nil)
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
		call, err := node.store.ReadSystemCallByRequestId(ctx, c.Id, 0)
		if err != nil || call == nil {
			panic(fmt.Errorf("store.ReadSystemCallByRequestId(%s) => %v %v", c.Id, call, err))
		}
		logger.Printf("node.handlePendingBurn(%s) => %d %s", call.RequestId, call.State, call.Hash.String)
		pending, err := node.store.CheckPendingBurnSystemCalls(ctx, c, am[c.Id])
		if err != nil {
			panic(err)
		}
		if pending {
			logger.Printf("store.CheckPendingBurnSystemCalls(%s)", c.Id)
			continue
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

func (node *Node) OccupyNonceAccountByCall(ctx context.Context, nonce *store.NonceAccount, cid string) error {
	return node.store.OccupyNonceAccountByCall(ctx, nonce, cid)
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

func (node *Node) sendTransferNotification(ctx context.Context) error {
	ns, err := node.store.ListInitialNotifications(ctx)
	if err != nil {
		return err
	}

	for _, n := range ns {
		hash := node.group.ReadFinishedTxHashByTraceId(ctx, n.TraceId)
		if hash == "" {
			continue
		}
		req, err := common.SafeReadTransactionRequestUntilSufficient(ctx, node.mixin, n.TraceId)
		if err != nil {
			return err
		}
		if req.SnapshotHash == "" {
			continue
		}

		_, err = bot.SafeNotifySnapshot(ctx, hash, 0, n.OpponentId, node.SafeUser())
		if err != nil {
			logger.Printf("bot.SafeNotifySnapshot(%s %s %s) => %v", n.TraceId, hash, n.OpponentId, err)
			if util.IsErrorCodes(err, 403) {
				continue
			}
			return err
		}
		err = node.store.MarkNotificationDone(ctx, n.TraceId)
		if err != nil {
			return err
		}
	}

	return nil
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

	os := node.group.ListOutputsForAsset(ctx, node.conf.AppId, assetId, node.conf.MTG.Genesis.Epoch, math.MaxInt64, mtg.SafeUtxoStateUnreceived, 0)
	for _, o := range os {
		unspent = unspent.Add(o.Amount)
	}
	os = node.group.ListOutputsForAsset(ctx, node.conf.AppId, assetId, node.conf.MTG.Genesis.Epoch, math.MaxInt64, mtg.SafeUtxoStateAssigned, 0)
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
