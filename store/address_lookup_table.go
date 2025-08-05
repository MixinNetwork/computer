package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/MixinNetwork/safe/common"
	sc "github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/program/address_lookup_table"
)

type AddressLookupTable struct {
	Account   string
	Table     string
	CreatedAt time.Time
}

var addressLookupTableCols = []string{"account", "lookup_table", "created_at"}

func (s *SQLite3Store) WriteAddressLookupTable(ctx context.Context, a *AddressLookupTable) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	vals := []any{a.Account, a.Table, time.Now().UTC()}
	err = s.execOne(ctx, tx, buildInsertionSQL("address_lookup_tables", addressLookupTableCols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT address_lookup_tables %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) CheckAddressLookupTable(ctx context.Context, account string) (bool, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return true, err
	}
	defer common.Rollback(tx)

	return s.checkExistence(ctx, tx, "SELECT lookup_table FROM address_lookup_tables WHERE account=?", account)
}

func (s *SQLite3Store) GetLatestAddressLookupTable(ctx context.Context) (string, error) {
	query := "SELECT lookup_table, COUNT(*) FROM address_lookup_tables WHERE lookup_table IN (SELECT lookup_table FROM address_lookup_tables ORDER BY created_at DESC LIMIT 1)"
	row := s.db.QueryRowContext(ctx, query)

	var table string
	var count uint
	err := row.Scan(&table, &count)
	if err == sql.ErrNoRows || count == address_lookup_table.LOOKUP_TABLE_MAX_ADDRESSES {
		return "", nil
	}
	return table, err
}

func (s *SQLite3Store) WriteAddressLookupTables(ctx context.Context, table string, accounts []sc.PublicKey) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	for _, a := range accounts {
		vals := []any{a.String(), table, time.Now().UTC()}
		err = s.execOne(ctx, tx, buildInsertionSQL("address_lookup_tables", addressLookupTableCols), vals...)
		if err != nil {
			return fmt.Errorf("INSERT address_lookup_tables %v", err)
		}
	}

	return tx.Commit()
}
