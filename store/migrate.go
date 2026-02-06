package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MixinNetwork/safe/common"
)

func (s *SQLite3Store) Migrate(ctx context.Context) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	key, val := "SCHEMA:VERSION:FAILED_BURN", ""
	row := tx.QueryRowContext(ctx, "SELECT value FROM properties WHERE key=?", key)
	err = row.Scan(&val)
	if err == nil || err != sql.ErrNoRows {
		return err
	}
	now := time.Now().UTC()

	query := "UPDATE system_calls SET state=? WHERE id=? AND state=?"
	_, err = tx.ExecContext(ctx, query, common.RequestStatePending, "035d4b18-451d-336c-abf1-ee9909f4e931", common.RequestStateFailed)
	if err != nil {
		return fmt.Errorf("SQLite3Store UPDATE system_calls %v", err)
	}

	_, err = tx.ExecContext(ctx, "INSERT INTO properties (key, value, created_at, updated_at) VALUES (?, ?, ?, ?)", key, query, now, now)
	if err != nil {
		return err
	}

	return tx.Commit()
}
