package solana

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/gofrs/uuid/v5"
)

func (node *Node) processFailedPrepareCall(ctx context.Context, call *store.SystemCall) error {
	logger.Printf("node.processFailedPrepareCall(%s)", call.RequestId)
	id := common.UniqueId(call.RequestId, "confirm-prepare-fail")
	cid := common.UniqueId(id, "post-process")
	nonce := node.ReadSpareNonceAccountWithCall(ctx, cid)
	extra := []byte{FlagConfirmCallFail}
	extra = append(extra, uuid.Must(uuid.FromString(call.RequestId)).Bytes()...)

	superior, err := node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
	if err != nil {
		panic(err)
	}
	tx := node.CreateRefundWithdrawalTransaction(ctx, call, superior, nonce)
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

	return node.sendObserverTransactionToGroup(ctx, &common.Operation{
		Id:    id,
		Type:  OperationTypeConfirmCall,
		Extra: extra,
	}, nil)
}

func (node *Node) refundFailedPrepareCalls(ctx context.Context) error {
	// FIXME dev
	failedPrepareIds := []string{
		"e259dd59-7b40-38f5-88f2-e98ff8a9160e",
		"74d03590-1a28-3666-9225-f32b2c97ad51",
	}

	key := "REFUND:FAILED:PREPARE:" + strings.Join(failedPrepareIds, ",")
	val, err := node.store.ReadProperty(ctx, key)
	if err != nil || val != "" {
		return err
	}

	calls, err := node.store.ListFailedPrepareCalls(ctx)
	if err != nil {
		return err
	}
	for _, prepare := range calls {
		if !slices.Contains(failedPrepareIds, prepare.RequestId) {
			continue
		}
		err = node.processFailedPrepareCall(ctx, prepare)
		if err != nil {
			return err
		}
		time.Sleep(time.Second)
	}

	return node.store.WriteProperty(ctx, key, "refunded")
}
