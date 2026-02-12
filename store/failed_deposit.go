package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/safe/mtg"
)

type FailedDeposit struct {
	OutputId  string
	Hash      string
	AssetId   string
	Amount    string
	HandledBy sql.NullString
	CreatedAt time.Time
}

var failedDepositCols = []string{"output_id", "hash", "asset_id", "amount", "handled_by", "created_at"}

func failedDepositFromRow(row Row) (*FailedDeposit, error) {
	var d FailedDeposit
	err := row.Scan(&d.OutputId, &d.Hash, &d.AssetId, &d.Amount, &d.HandledBy, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &d, err
}

func (s *SQLite3Store) writeFailedDeposit(ctx context.Context, tx *sql.Tx, out *mtg.Action) error {
	existed, err := s.checkExistence(ctx, tx, "SELECT output_id FROM failed_deposits WHERE output_id=?", out.OutputId)
	if err != nil || existed {
		return err
	}

	vals := []any{out.OutputId, out.DepositHash.String, out.AssetId, out.Amount.String(), nil, out.SequencerCreatedAt}
	err = s.execOne(ctx, tx, buildInsertionSQL("failed_deposits", failedDepositCols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT failed_deposits %v", err)
	}
	return nil
}

func (s *SQLite3Store) handleFailedDepositByRequest(ctx context.Context, tx *sql.Tx, d *FailedDeposit, req *Request) error {
	err := s.execOne(ctx, tx, "UPDATE failed_deposits SET handled_by=? WHERE output_id=? AND handled_by IS NULL", req.Id, d.OutputId)
	if err != nil {
		return fmt.Errorf("UPDATE failed_deposits %v", err)
	}
	return nil
}

func (s *SQLite3Store) ReadFailedDepositByHash(ctx context.Context, hash string) (*FailedDeposit, error) {
	query := fmt.Sprintf("SELECT %s FROM failed_deposits WHERE hash=?", strings.Join(failedDepositCols, ","))
	row := s.db.QueryRowContext(ctx, query, hash)

	return failedDepositFromRow(row)
}
