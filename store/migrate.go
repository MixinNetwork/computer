package store

import (
	"context"
	"database/sql"
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

	key, val := "SCHEMA:VERSION:EXTERMA_ASSETS", ""
	row := tx.QueryRowContext(ctx, "SELECT value FROM properties WHERE key=?", key)
	err = row.Scan(&val)
	if err == nil || err != sql.ErrNoRows {
		return err
	}

	query := "ALTER TABLE external_assets ADD COLUMN chain_id VARCHAR NOT NULL DEFAULT '';\n"
	query += "ALTER TABLE external_assets ADD COLUMN name VARCHAR NOT NULL DEFAULT '';\n"
	query += "ALTER TABLE external_assets ADD COLUMN symbol VARCHAR NOT NULL DEFAULT '';\n"
	query += "ALTER TABLE external_assets ADD COLUMN price_usd VARCHAR NOT NULL DEFAULT '';\n"
	_, err = tx.ExecContext(ctx, query)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	_, err = tx.ExecContext(ctx, "INSERT INTO properties (key, value, created_at, updated_at) VALUES (?, ?, ?, ?)", key, query, now, now)
	if err != nil {
		return err
	}

	return tx.Commit()
}
