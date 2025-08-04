package solana

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	"github.com/MixinNetwork/computer/store"
	mc "github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/shopspring/decimal"
)

func (node *Node) readStorageExtraFromObserver(ctx context.Context, ref crypto.Hash) []byte {
	if common.CheckTestEnvironment(ctx) {
		val, err := node.store.ReadProperty(ctx, ref.String())
		if err != nil {
			panic(ref.String())
		}
		raw, err := base64.RawURLEncoding.DecodeString(val)
		if err != nil {
			panic(ref.String())
		}
		return raw
	}

	ver, err := node.group.ReadKernelTransactionUntilSufficient(ctx, ref.String())
	if err != nil {
		panic(ref.String())
	}

	return ver.Extra
}

func (node *Node) checkTransaction(ctx context.Context, act *mtg.Action, assetId string, receivers []string, threshold int, destination, tag, amount string, memo []byte, traceId string) string {
	if common.CheckTestEnvironment(ctx) {
		v := common.MarshalJSONOrPanic(map[string]any{
			"asset_id":    assetId,
			"amount":      amount,
			"receivers":   receivers,
			"threshold":   threshold,
			"destination": destination,
			"tag":         tag,
			"memo":        hex.EncodeToString(memo),
		})
		err := node.store.WriteProperty(ctx, traceId, string(v))
		if err != nil {
			panic(err)
		}
	}
	balance := act.CheckAssetBalanceAt(ctx, assetId)
	logger.Printf("group.CheckAssetBalanceAt(%s, %d) => %s %s %s", assetId, act.Sequence, traceId, amount, balance)
	amt := decimal.RequireFromString(amount)
	if balance.Cmp(amt) < 0 {
		return ""
	}

	if !common.CheckTestEnvironment(ctx) {
		da, err := node.store.ReadDeployedAsset(ctx, assetId)
		if err != nil {
			panic(err)
		}
		if da != nil {
			supply := node.RPCMintSupply(ctx, da.Address)
			balance = node.getMtgAssetBalance(ctx, da.AssetId)
			if balance.Sub(amt).Cmp(supply) < 0 {
				panic(fmt.Errorf("invalid balance of mtg asset %s %s: %s %s %s", da.AssetId, da.Address, balance, amt, supply))
			}
		}
	}

	nextId := common.UniqueId(node.group.GenesisId(), traceId)
	logger.Printf("node.checkTransaction(%s) => %s", traceId, nextId)
	return nextId
}

func (node *Node) buildWithdrawalTransaction(ctx context.Context, act *mtg.Action, assetId, amount string, memo []byte, destination, tag, traceId string) *mtg.Transaction {
	logger.Printf("node.buildTransactionWithReferences(%s, %s, %x, %s, %s, %s)", assetId, amount, memo, destination, tag, traceId)
	traceId = node.checkTransaction(ctx, act, assetId, nil, 0, destination, tag, amount, memo, traceId)
	if traceId == "" {
		return nil
	}

	return act.BuildWithdrawTransaction(ctx, traceId, assetId, amount, string(memo), destination, tag)
}

func (node *Node) buildTransaction(ctx context.Context, act *mtg.Action, opponentAppId, assetId string, receivers []string, threshold int, amount string, memo []byte, traceId string) *mtg.Transaction {
	logger.Printf("node.buildTransaction(%s, %s, %v, %d, %s, %x, %s)", opponentAppId, assetId, receivers, threshold, amount, memo, traceId)
	return node.buildTransactionWithReferences(ctx, act, opponentAppId, assetId, receivers, threshold, amount, memo, traceId, crypto.Hash{})
}

func (node *Node) buildTransactionWithReferences(ctx context.Context, act *mtg.Action, opponentAppId, assetId string, receivers []string, threshold int, amount string, memo []byte, traceId string, tx crypto.Hash) *mtg.Transaction {
	logger.Printf("node.buildTransactionWithReferences(%s, %v, %d, %s, %x, %s, %s)", assetId, receivers, threshold, amount, memo, traceId, tx)
	traceId = node.checkTransaction(ctx, act, assetId, receivers, threshold, "", "", amount, memo, traceId)
	if traceId == "" {
		return nil
	}

	if tx.HasValue() {
		return act.BuildTransactionWithReference(ctx, traceId, opponentAppId, assetId, amount, string(memo), receivers, threshold, tx)
	}
	return act.BuildTransaction(ctx, traceId, opponentAppId, assetId, amount, string(memo), receivers, threshold)
}

func (node *Node) confirmBurnRelatedSystemCallToGroup(ctx context.Context, op *common.Operation, call *store.SystemCall) error {
	switch call.State {
	case common.RequestStateDone, common.RequestStateFailed:
		err := node.store.ConfirmPendingBurnSystemCall(ctx, call.RequestId)
		if err != nil {
			return fmt.Errorf("store.ConfirmPendingBurnSystemCall(%s) => %v", call.RequestId, err)
		}
	}

	request, err := node.store.ReadRequest(ctx, op.Id)
	if err != nil {
		panic(err)
	}
	if request == nil {
		sufficient := node.checkSufficientBalanceForBurnSystemCall(ctx, call)
		if !sufficient {
			logger.Printf("store.checkSufficientBalanceForBurnSystemCall(%s)", call.RequestId)
			return nil
		}
		return node.sendObserverTransactionToGroup(ctx, op, nil)
	}

	switch request.State {
	case common.RequestStateInitial:
		return nil
	case common.RequestStateDone:
		err = node.store.ConfirmPendingBurnSystemCall(ctx, call.RequestId)
		if err != nil {
			return fmt.Errorf("store.ConfirmPendingBurnSystemCall(%s) => %v", call.RequestId, err)
		}
	case common.RequestStateFailed:
		id := common.UniqueId(op.Id, "RETRY")
		err = node.store.UpdatePendingBurnSystemCallRequestId(ctx, call.RequestId, request.Id, id)
		if err != nil {
			return fmt.Errorf("store.UpdatePendingBurnSystemCallRequestId(%s %s %s) => %v", call.RequestId, request.Id, id, err)
		}
	}
	return nil
}

func (node *Node) sendObserverTransactionToGroup(ctx context.Context, op *common.Operation, references []crypto.Hash) error {
	logger.Printf("node.sendObserverTransactionToGroup(%v)", op)
	extra := encodeOperation(op)
	extra = node.signObserverExtra(extra)

	traceId := fmt.Sprintf("SESSION:%s:OBSERVER:%s", op.Id, string(node.id))
	return node.sendTransactionToGroupUntilSufficient(ctx, extra, "0.00000001", bot.XINAssetId, traceId, references)
}

func (node *Node) sendSignerTransactionToGroup(ctx context.Context, traceId string, op *common.Operation, references []crypto.Hash) error {
	logger.Printf("node.sendSignerTransactionToGroup(%s %v)", node.id, op)
	extra := encodeOperation(op)

	return node.sendTransactionToGroupUntilSufficient(ctx, extra, "1", node.conf.AssetId, traceId, references)
}

func (node *Node) sendTransactionToGroupUntilSufficient(ctx context.Context, memo []byte, amount, assetId, traceId string, references []crypto.Hash) error {
	receivers := node.GetMembers()
	threshold := node.conf.MTG.Genesis.Threshold
	amt := decimal.RequireFromString(amount)
	traceId = common.UniqueId(traceId, fmt.Sprintf("MTG:%v:%d", receivers, threshold))

	if common.CheckTestEnvironment(ctx) {
		return node.mtgQueueTestOutput(ctx, memo)
	}
	m := mtg.EncodeMixinExtraBase64(node.conf.AppId, memo)
	if len([]byte(m)) <= mc.ExtraSizeGeneralLimit {
		_, err := common.SendTransactionUntilSufficient(ctx, node.wallet, node.mixin, receivers, threshold, amt, traceId, assetId, m, references, node.conf.MTG.App.SpendPrivateKey)
		logger.Printf("node.SendTransactionUntilSufficient(%s) => %v", traceId, err)
		return err
	}
	if assetId != bot.XINAssetId {
		err := fmt.Errorf("node.sendTransactionToGroupUntilSufficient(%s, %s, %s) => %d", traceId, assetId, amount, len(memo))
		panic(err)
	}

	_, err := common.CreateObjectStorageUntilSufficient(ctx, node.wallet, node.mixin, []*bot.TransactionRecipient{{
		MixAddress: bot.NewUUIDMixAddress(node.conf.MTG.Genesis.Members, byte(node.conf.MTG.Genesis.Threshold)),
		Amount:     amount,
	}}, []byte(m), traceId, *node.SafeUser())
	logger.Printf("node.CreateObjectStorageUntilSufficient(%s) => %v", traceId, err)
	return err
}

func encodeOperation(op *common.Operation) []byte {
	extra := []byte{op.Type}
	extra = append(extra, op.Extra...)
	return extra
}

func decodeOperation(extra []byte) *common.Operation {
	return &common.Operation{
		Type:  extra[0],
		Extra: extra[1:],
	}
}
