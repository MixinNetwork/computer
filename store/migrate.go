package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
)

const (
	oversizedDepositMigrationKey = "SCHEMA:VERSION:OVERSIZED_DEPOSIT_7E823E4C"
	oversizedDepositSystemCallID = "7e823e4c-b389-320a-b241-ff96c30d730b"
)

func (s *SQLite3Store) Migrate(ctx context.Context) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	err = s.migrateOversizedDepositSystemCall(ctx, tx)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLite3Store) migrateOversizedDepositSystemCall(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.checkExistence(ctx, tx, "SELECT value FROM properties WHERE key=?", oversizedDepositMigrationKey)
	if err != nil || applied {
		return err
	}

	call, err := s.ReadSystemCallByRequestId(ctx, oversizedDepositSystemCallID, common.RequestStatePending)
	if err != nil {
		return fmt.Errorf("store.ReadSystemCallByRequestId(%s) => %v", oversizedDepositSystemCallID, err)
	}
	if call == nil {
		return s.writeProperty(ctx, tx, oversizedDepositMigrationKey, "system call not found")
	}
	if call.Type != CallTypeDeposit {
		return fmt.Errorf("invalid system call type for oversized deposit migration: %s", call.Type)
	}

	solanaTx, err := solana.TransactionFromBase64(call.Raw)
	if err != nil {
		return fmt.Errorf("solana.TransactionFromBase64(%s) => %v", oversizedDepositSystemCallID, err)
	}
	sizeErr := solanaApp.ValidateTransactionSize(solanaTx)
	if sizeErr == nil {
		return s.writeProperty(ctx, tx, oversizedDepositMigrationKey, "transaction within size limit")
	}
	if !errors.Is(sizeErr, solanaApp.ErrTransactionTooLarge) {
		return fmt.Errorf("solana.ValidateTransactionSize(%s) => %v", oversizedDepositSystemCallID, sizeErr)
	}
	logger.Printf("store.migrateOversizedDepositSystemCall(%s) => %v", oversizedDepositSystemCallID, sizeErr)

	query := "UPDATE system_calls SET state=?, updated_at=? WHERE id=? AND call_type=? AND state=?"
	err = s.execOne(ctx, tx, query, common.RequestStateFailed, time.Now().UTC(), oversizedDepositSystemCallID, CallTypeDeposit, common.RequestStatePending)
	if err != nil {
		return fmt.Errorf("SQLite3Store UPDATE oversized deposit system_calls %v", err)
	}

	return s.writeProperty(ctx, tx, oversizedDepositMigrationKey, sizeErr.Error())
}
