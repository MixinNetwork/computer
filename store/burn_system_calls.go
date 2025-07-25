package store

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/safe/common"
)

type BurnSystemCall struct {
	Id        string
	AssetId   string
	RequestId string
	ExtraHex  string
	State     byte
	CreatedAt time.Time
	UpdatedAt time.Time
}

var burnSystemCallCols = []string{"call_id", "asset_id", "request_id", "extra", "state", "created_at", "updated_at"}

func (c *BurnSystemCall) ExtraBytes() []byte {
	return common.DecodeHexOrPanic(c.ExtraHex)
}

func (s *SQLite3Store) WritePendingBurnSystemCallIfNotExists(ctx context.Context, call *SystemCall, op *common.Operation, as []string) error {
	switch call.Type {
	case CallTypeDeposit, CallTypePostProcess:
	default:
		panic(call)
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	now := time.Now().UTC()
	for _, id := range as {
		existed, err := s.checkExistence(ctx, tx, "SELECT request_id FROM burn_system_calls WHERE call_id=? AND asset_id=?", call.RequestId, id)
		if err != nil {
			return err
		}
		if existed {
			continue
		}

		vals := []any{call.RequestId, id, op.Id, hex.EncodeToString(op.Extra), common.RequestStateInitial, now, now}
		err = s.execOne(ctx, tx, buildInsertionSQL("burn_system_calls", burnSystemCallCols), vals...)
		if err != nil {
			return fmt.Errorf("INSERT burn_system_calls %v", err)
		}
	}

	return tx.Commit()
}

func (s *SQLite3Store) UpdatePendingBurnSystemCallRequestId(ctx context.Context, callId, oldRid, rid string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	query := "UPDATE burn_system_calls SET request_id=?, updated_at=? WHERE call_id=? AND request_id=? AND state=?"
	_, err = tx.ExecContext(ctx, query, rid, time.Now().UTC(), callId, oldRid, common.RequestStateInitial)
	if err != nil {
		return fmt.Errorf("UPDATE burn_system_calls %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) ConfirmPendingBurnSystemCall(ctx context.Context, callId string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	query := "UPDATE burn_system_calls SET state=?, updated_at=? WHERE call_id=? AND state=?"
	_, err = tx.ExecContext(ctx, query, common.RequestStateDone, time.Now().UTC(), callId, common.RequestStateInitial)
	if err != nil {
		return fmt.Errorf("UPDATE burn_system_calls %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) ListPendingBurnSystemCalls(ctx context.Context) ([]*BurnSystemCall, map[string][]string, error) {
	query := fmt.Sprintf("SELECT %s FROM burn_system_calls WHERE state=? ORDER BY created_at ASC LIMIT 100", strings.Join(burnSystemCallCols, ","))
	rows, err := s.db.QueryContext(ctx, query, common.RequestStateInitial)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var cs []*BurnSystemCall
	am := make(map[string][]string)
	for rows.Next() {
		var c BurnSystemCall
		err := rows.Scan(&c.Id, &c.AssetId, &c.RequestId, &c.ExtraHex, &c.State, &c.CreatedAt, &c.UpdatedAt)
		if err != nil {
			return nil, nil, err
		}
		cs = append(cs, &c)
		am[c.Id] = append(am[c.Id], c.AssetId)
	}
	return cs, am, nil
}

func (s *SQLite3Store) CheckPendingBurnSystemCalls(ctx context.Context, call *BurnSystemCall, as []string) (bool, error) {
	if len(as) == 0 {
		panic(call)
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer common.Rollback(tx)

	for _, id := range as {
		existed, err := s.checkExistence(ctx, tx, "SELECT call_id FROM burn_system_calls WHERE asset_id=? AND state=? AND created_at<?", id, common.RequestStateInitial, call.CreatedAt)
		if err != nil || existed {
			return true, err
		}
	}

	return false, nil
}
