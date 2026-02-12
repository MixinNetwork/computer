package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MixinNetwork/safe/mtg"
)

type FailedDeposit struct {
	OutputId  string
	Hash      string
	Amount    string
	HandledBy sql.NullString
	CreatedAt time.Time
}

var failedDepositCols = []string{"output_id", "hash", "amount", "handled_by", "created_at"}

func failedDepositFromRow(row Row) (*FailedDeposit, error) {
	var d FailedDeposit
	err := row.Scan(&d.OutputId, &d.Hash, &d.Amount, &d.HandledBy, &d.CreatedAt)
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

	vals := []any{out.OutputId, out.DepositHash.String, out.Amount.String(), nil, out.SequencerCreatedAt}
	err = s.execOne(ctx, tx, buildInsertionSQL("failed_deposits", failedDepositCols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT failed_deposits %v", err)
	}
	return nil
}
