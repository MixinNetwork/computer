package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
)

type Notification struct {
	TraceId    string
	OpponentId string
	State      byte
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

var notificationCols = []string{"trace_id", "opponent_id", "state", "created_at", "updated_at"}

func notificationFromRow(row Row) (*Notification, error) {
	var n Notification
	err := row.Scan(&n.TraceId, &n.OpponentId, &n.State, &n.CreatedAt, &n.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &n, err
}

func (s *SQLite3Store) writeNotifications(ctx context.Context, tx *sql.Tx, txs []*mtg.Transaction) error {
	now := time.Now().UTC()

	for _, t := range txs {
		if len(t.Receivers) != 1 {
			continue
		}
		vals := []any{t.TraceId, t.Receivers[0], common.RequestStateInitial, now, now}
		err := s.execOne(ctx, tx, buildInsertionSQL("tx_notifications", notificationCols), vals...)
		if err != nil {
			return fmt.Errorf("INSERT tx_notifications %v", err)
		}
	}

	return nil
}

func (s *SQLite3Store) ListInitialNotifications(ctx context.Context) ([]*Notification, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	query := fmt.Sprintf("SELECT %s FROM tx_notifications WHERE state=? ORDER BY created_at ASC LIMIT 20", strings.Join(notificationCols, ","))
	rows, err := s.db.QueryContext(ctx, query, common.RequestStateInitial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ns []*Notification
	for rows.Next() {
		n, err := notificationFromRow(rows)
		if err != nil {
			return nil, err
		}
		ns = append(ns, n)
	}
	return ns, nil
}

func (s *SQLite3Store) MarkNotificationDone(ctx context.Context, traceId string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	query := "UPDATE tx_notifications SET state=?,updated_at=? WHERE trace_id=? AND state=?"
	err = s.execOne(ctx, tx, query, common.RequestStateDone, time.Now().UTC(), traceId, common.RequestStateInitial)
	if err != nil {
		return fmt.Errorf("UPDATE tx_notifications %v", err)
	}

	return tx.Commit()
}
