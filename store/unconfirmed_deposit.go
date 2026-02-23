package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/safe/mtg"
)

type UnconfirmedDeposit struct {
	OutputId  string
	Hash      string
	Index     int64
	AssetId   string
	Amount    string
	HandledBy sql.NullString
	CreatedAt time.Time
}

var unconfirmedDepositCols = []string{"output_id", "mixin_hash", "mixin_index", "asset_id", "amount", "handled_by", "created_at"}

func unconfirmedDepositFromRow(row Row) (*UnconfirmedDeposit, error) {
	var d UnconfirmedDeposit
	err := row.Scan(&d.OutputId, &d.Hash, &d.Index, &d.AssetId, &d.Amount, &d.HandledBy, &d.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &d, err
}

func (s *SQLite3Store) writeFailedDeposit(ctx context.Context, tx *sql.Tx, out *mtg.Action) error {
	existed, err := s.checkExistence(ctx, tx, "SELECT output_id FROM unconfirmed_deposits WHERE output_id=?", out.OutputId)
	if err != nil || existed {
		return err
	}

	vals := []any{out.OutputId, out.DepositHash.String, out.DepositIndex, out.AssetId, out.Amount.String(), nil, out.SequencerCreatedAt}
	err = s.execOne(ctx, tx, buildInsertionSQL("unconfirmed_deposits", unconfirmedDepositCols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT unconfirmed_deposits %v", err)
	}
	return nil
}

func (s *SQLite3Store) handleFailedDepositByRequest(ctx context.Context, tx *sql.Tx, d *UnconfirmedDeposit, req *Request) error {
	err := s.execOne(ctx, tx, "UPDATE unconfirmed_deposits SET handled_by=? WHERE output_id=? AND handled_by IS NULL", req.Id, d.OutputId)
	if err != nil {
		return fmt.Errorf("UPDATE unconfirmed_deposits %v", err)
	}
	return nil
}

func (s *SQLite3Store) ReadFailedDepositByHash(ctx context.Context, hash string) (*UnconfirmedDeposit, error) {
	query := fmt.Sprintf("SELECT %s FROM unconfirmed_deposits WHERE mixin_hash=?", strings.Join(unconfirmedDepositCols, ","))
	row := s.db.QueryRowContext(ctx, query, hash)

	return unconfirmedDepositFromRow(row)
}
