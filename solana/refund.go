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
	failedPrepareIds := []string{
		"ccbb4fc6-737c-3428-a7b0-78d1de6ccbdd",
		"933b85e2-2863-3253-99b7-2b5e6a512d32",
		"eca70deb-8822-372e-a63b-f84a7cbaa129",
		"a83cee37-f113-31e8-9c95-2e3490263c87",
		"f9d47737-ca6e-3038-9307-b28c7ce59df7",
		"5dea077c-ec74-3d92-a6f7-6fafc5f93697",
		"39891351-d37f-322c-9fd8-b049cc629fe2",
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
