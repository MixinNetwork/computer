package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/MixinNetwork/safe/common"
)

const (
	stalePostProcessNonceMigrationKey = "SCHEMA:VERSION:STALE_POST_PROCESS_NONCE_E3AAD597"
	stalePostProcessSystemCallID      = "e3aad597-2f6a-334d-b4fb-de634cfd6d81"
	stalePostProcessNonceHash         = "9nFUY4moFN6mEc6MhstcEeHhevziLx4kGk32sny9TitE"
)

func (s *SQLite3Store) Migrate(ctx context.Context, isObserver bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	err = s.migrateStalePostProcessNonce(ctx, tx, isObserver)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *SQLite3Store) migrateStalePostProcessNonce(ctx context.Context, tx *sql.Tx, isObserver bool) error {
	applied, err := s.checkExistence(ctx, tx, "SELECT value FROM properties WHERE key=?", stalePostProcessNonceMigrationKey)
	if err != nil || applied {
		return err
	}

	query := fmt.Sprintf("SELECT %s FROM system_calls WHERE id=?", strings.Join(systemCallCols, ","))
	call, err := systemCallFromRow(tx.QueryRowContext(ctx, query, stalePostProcessSystemCallID))
	if err != nil || call == nil {
		return fmt.Errorf("SELECT stale post-process system call %v %v", call, err)
	}
	if call.Type != CallTypePostProcess || call.State != common.RequestStatePending {
		return fmt.Errorf("invalid system call type for stale post-process nonce migration: %s %d", call.Type, call.State)
	}

	now := time.Now().UTC()
	query = "UPDATE system_calls SET state=?, updated_at=? WHERE id=? AND call_type=? AND state=?"
	err = s.execOne(ctx, tx, query, common.RequestStateFailed, now, stalePostProcessSystemCallID, CallTypePostProcess, common.RequestStatePending)
	if err != nil {
		return fmt.Errorf("UPDATE stale post-process system_calls %v", err)
	}

	if isObserver {
		query = "UPDATE nonce_accounts SET hash=?, mix=NULL, call_id=NULL, updated_at=? WHERE address=?"
		err = s.execOne(ctx, tx, query, stalePostProcessNonceHash, now, call.NonceAccount)
		if err != nil {
			return fmt.Errorf("UPDATE stale post-process nonce_accounts %v", err)
		}
	}

	return s.writeProperty(ctx, tx, stalePostProcessNonceMigrationKey, "done")
}
