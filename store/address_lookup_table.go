package store

import (
	"context"
	"fmt"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/safe/common"
	sc "github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/program/address_lookup_table"
)

const (
	ExtendTableSize = 30
)

type AddressLookupTable struct {
	Account string
	Table   string
}

var addressLookupTableCols = []string{"account", "lookup_table", "created_at"}

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

func (s *SQLite3Store) FilterExistedAddressLookupTable(ctx context.Context, accounts []string) ([]sc.PublicKey, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer common.Rollback(tx)

	var as []sc.PublicKey
	for _, account := range accounts {
		existed, err := s.checkExistence(ctx, tx, "SELECT lookup_table FROM address_lookup_tables WHERE account=?", account)
		if err != nil {
			return nil, err
		}
		if existed {
			continue
		}
		as = append(as, sc.PublicKeyFromString(account))
	}
	return as, nil
}

func (s *SQLite3Store) ListAddressLookupTable(ctx context.Context) ([]solanaApp.LookupTableStats, error) {
	query := "SELECT lookup_table, COUNT(*) FROM address_lookup_tables GROUP BY lookup_table"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []solanaApp.LookupTableStats
	for rows.Next() {
		var table string
		var count uint
		err := rows.Scan(&table, &count)
		if err != nil {
			return nil, err
		}
		if count > address_lookup_table.LOOKUP_TABLE_MAX_ADDRESSES {
			panic(table)
		}
		tables = append(tables, solanaApp.LookupTableStats{
			Table: table,
			Space: address_lookup_table.LOOKUP_TABLE_MAX_ADDRESSES - count,
		})
	}
	return tables, nil
}
