package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MixinNetwork/safe/common"
)

type Cache struct {
	Key       string
	Value     string
	CreatedAt time.Time
}

const CacheTTL = time.Hour

func (s *SQLite3Store) ReadCache(ctx context.Context, k string) (string, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	row := s.db.QueryRowContext(ctx, "SELECT value,expired_at FROM caches WHERE key=?", k)
	var value string
	var expiredAt time.Time
	err := row.Scan(&value, &expiredAt)
	if err == sql.ErrNoRows {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if expiredAt.Before(time.Now()) {
		return "", nil
	}
	return value, nil
}

func (s *SQLite3Store) WriteCache(ctx context.Context, k, v string, duration time.Duration) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	threshold := time.Now().Add(-duration).UTC()
	_, err = tx.ExecContext(ctx, "DELETE FROM caches WHERE created_at<?", threshold)
	if err != nil {
		return err
	}

	existed, err := s.checkExistence(ctx, tx, "SELECT key FROM caches WHERE key=?", k)
	if err != nil || existed {
		return err
	}

	now := time.Now().UTC()
	exp := now.Add(duration)
	cols := []string{"key", "value", "created_at", "expired_at"}
	vals := []any{k, v, now, exp}
	err = s.execOne(ctx, tx, buildInsertionSQL("caches", cols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT caches %v", err)
	}
	return tx.Commit()
}
