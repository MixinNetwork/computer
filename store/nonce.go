package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
)

type NonceAccount struct {
	Address   string
	Hash      string
	HashSlot  uint64
	Mix       sql.NullString
	CallId    sql.NullString
	UpdatedBy sql.NullString
	CreatedAt time.Time
	UpdatedAt time.Time
}

const MaxNonceAccountsPerMix = 5

var ErrNonceAccountLimit = errors.New("nonce account limit reached")

var ErrNoSpareNonceAccount = errors.New("no spare nonce account available")

var ErrNonceAccountChanged = errors.New("nonce account reservation changed")

type NonceProposal struct {
	Address    string
	Hash       string
	Action     uint8
	Extra      string
	RetryId    string
	RejectedBy string
}

// An accepted child call, or a request awaiting compaction, must never be
// replaced. A terminal rejection can be rebuilt under a new submission ID.
// Only requests sequenced after that rejection can finish the new attempt,
// even when the rebuilt transaction has exactly the same bytes.
const nonceProposalQuery = `SELECT p.nonce_address,p.nonce_hash,p.action,p.extra,p.retry_id,
 COALESCE((SELECT r.request_id FROM requests r JOIN action_results a ON a.output_id=r.request_id
 WHERE r.action=p.action AND r.extra=p.extra AND r.state=? AND a.compaction=''
 AND (p.retry_id='' OR r.sequence>(SELECT sequence FROM requests WHERE request_id=p.retry_id))
 AND NOT EXISTS (SELECT 1 FROM system_calls WHERE id=p.call_id)
 ORDER BY r.created_at,r.request_id LIMIT 1),'')
 FROM nonce_proposals p WHERE p.call_id=?`

func nonceProposalFromRow(row Row) (*NonceProposal, error) {
	var p NonceProposal
	err := row.Scan(&p.Address, &p.Hash, &p.Action, &p.Extra, &p.RetryId, &p.RejectedBy)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &p, err
}

// Compare the complete reservation, including its generation, after RPC work.
const nonceSnapshotWhere = "address=? AND hash=? AND hash_slot=? AND mix IS ? AND call_id IS ? AND updated_at=?"

// Compaction can replay a failed request, so only a terminal action result
// makes an unaccepted proposal safe to discard.
const pendingNonceProposal = `SELECT 1 FROM nonce_proposals p
 WHERE p.call_id=nonce_accounts.call_id AND p.nonce_address=nonce_accounts.address AND p.nonce_hash=nonce_accounts.hash
 AND NOT EXISTS (SELECT 1 FROM requests r JOIN action_results a ON a.output_id=r.request_id
 WHERE r.action=p.action AND r.extra=p.extra AND r.state IN (?,?) AND a.compaction=''
 AND (p.retry_id='' OR r.sequence>(SELECT sequence FROM requests WHERE request_id=p.retry_id)))`

func (a *NonceAccount) snapshotArgs() []any {
	return []any{a.Address, a.Hash, a.HashSlot, a.Mix, a.CallId, a.UpdatedAt}
}

var nonceAccountCols = []string{"address", "hash", "hash_slot", "mix", "call_id", "updated_by", "created_at", "updated_at"}

func nonceAccountFromRow(row Row) (*NonceAccount, error) {
	var a NonceAccount
	err := row.Scan(&a.Address, &a.Hash, &a.HashSlot, &a.Mix, &a.CallId, &a.UpdatedBy, &a.CreatedAt, &a.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (a *NonceAccount) Account() solanaApp.NonceAccount {
	return solanaApp.NonceAccount{
		Address: solana.MustPublicKeyFromBase58(a.Address),
		Hash:    solana.MustHashFromBase58(a.Hash),
	}
}

func (a *NonceAccount) LockedByUserOnly() bool {
	return a.Mix.Valid && !a.CallId.Valid
}

func (a *NonceAccount) Valid(cid string) bool {
	return a.Mix.Valid && (!a.CallId.Valid || a.CallId.String == cid)
}

func (a *NonceAccount) Expired() bool {
	return a.UpdatedAt.Add(20 * time.Minute).Before(time.Now())
}

func (s *SQLite3Store) WriteNonceAccount(ctx context.Context, address, hash string, slot uint64) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	now := time.Now().UTC()
	vals := []any{address, hash, slot, nil, nil, nil, now, now}
	err = s.execOne(ctx, tx, buildInsertionSQL("nonce_accounts", nonceAccountCols), vals...)
	if err != nil {
		return fmt.Errorf("INSERT nonce_accounts %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) UpdateNonceAccount(ctx context.Context, nonce *NonceAccount, hash string, slot uint64, call string) error {
	if slot < nonce.HashSlot {
		return fmt.Errorf("%w: nonce RPC slot %d < %d", ErrNonceAccountChanged, slot, nonce.HashSlot)
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	now := time.Now().UTC()
	args := append([]any{hash, slot, call, now}, nonce.snapshotArgs()...)
	_, err = tx.ExecContext(ctx, "UPDATE nonce_accounts SET hash=?, hash_slot=?, mix=NULL, call_id=NULL, updated_by=?, updated_at=? WHERE "+nonceSnapshotWhere, args...)
	if err != nil {
		return fmt.Errorf("UPDATE nonce_accounts %v", err)
	}

	return tx.Commit()
}

func (s *SQLite3Store) LockNonceAccountWithMix(ctx context.Context, address, mix string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	var count int
	err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nonce_accounts WHERE mix=?", mix).Scan(&count)
	if err != nil {
		return fmt.Errorf("SELECT nonce_accounts %v", err)
	}
	if count >= MaxNonceAccountsPerMix {
		return fmt.Errorf("%w: %s", ErrNonceAccountLimit, mix)
	}

	err = s.execOne(ctx, tx, "UPDATE nonce_accounts SET mix=?, updated_at=? WHERE address=? AND mix IS NULL AND call_id IS NULL",
		mix, time.Now().UTC(), address)
	if err != nil {
		return fmt.Errorf("UPDATE nonce_accounts %v", err)
	}

	return tx.Commit()
}

// ReserveNonceAccountForMix selects and locks a user nonce in one transaction.
// Leave one account for Prepare; otherwise accepted Main calls can occupy the
// entire pool while every one of them waits for a Prepare nonce.
func (s *SQLite3Store) ReserveNonceAccountForMix(ctx context.Context, mix string) (*NonceAccount, error) {
	if mix == "" {
		return nil, fmt.Errorf("missing mix address")
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer common.Rollback(tx)
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nonce_accounts WHERE mix=?", mix).Scan(&count); err != nil {
		return nil, err
	}
	if count >= MaxNonceAccountsPerMix {
		return nil, ErrNonceAccountLimit
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM nonce_accounts WHERE mix IS NULL AND call_id IS NULL").Scan(&count); err != nil {
		return nil, err
	}
	if count <= 1 {
		return nil, ErrNoSpareNonceAccount
	}
	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE mix IS NULL AND call_id IS NULL ORDER BY updated_at, address LIMIT 1", strings.Join(nonceAccountCols, ","))
	nonce, err := nonceAccountFromRow(tx.QueryRowContext(ctx, query))
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if err := s.execOne(ctx, tx, "UPDATE nonce_accounts SET mix=?, updated_at=? WHERE address=? AND mix IS NULL AND call_id IS NULL", mix, now, nonce.Address); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	nonce.Mix = sql.NullString{Valid: true, String: mix}
	nonce.UpdatedAt = now
	return nonce, nil
}

func (s *SQLite3Store) OccupyNonceAccountByCall(ctx context.Context, nonce *NonceAccount, call string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	if call == "" || (nonce.CallId.Valid && nonce.CallId.String != call) {
		return ErrNonceAccountChanged
	}
	existed, err := s.checkExistence(ctx, tx, "SELECT address FROM nonce_accounts WHERE "+nonceSnapshotWhere, nonce.snapshotArgs()...)
	if err != nil {
		return err
	}
	if !existed {
		return ErrNonceAccountChanged
	}
	if nonce.CallId.Valid {
		return nil
	}

	err = s.execOne(ctx, tx, "UPDATE nonce_accounts SET call_id=?, updated_at=? WHERE address=? AND call_id IS NULL",
		call, time.Now().UTC(), nonce.Address)
	if err != nil {
		return fmt.Errorf("UPDATE nonce_accounts %v", err)
	}

	return tx.Commit()
}

// ReserveNonceAccountForCall selects and claims a spare nonce atomically.
// A retry of the same call returns its existing reservation, even if the pool is full.
func (s *SQLite3Store) ReserveNonceAccountForCall(ctx context.Context, call string) (*NonceAccount, error) {
	if call == "" {
		return nil, fmt.Errorf("missing call id for nonce reservation")
	}
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer common.Rollback(tx)

	cols := strings.Join(nonceAccountCols, ",")
	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE call_id=?", cols)
	nonce, err := nonceAccountFromRow(tx.QueryRowContext(ctx, query, call))
	if err != nil {
		return nil, err
	}
	if nonce != nil {
		// Retries renew an unpublished reservation while its builder is active.
		now := time.Now().UTC()
		if err := s.execOne(ctx, tx, "UPDATE nonce_accounts SET updated_at=? WHERE address=? AND call_id=?", now, nonce.Address, call); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		nonce.UpdatedAt = now
		return nonce, nil
	}

	query = fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE mix IS NULL AND call_id IS NULL ORDER BY updated_at ASC, address ASC LIMIT 1", cols)
	nonce, err = nonceAccountFromRow(tx.QueryRowContext(ctx, query))
	if err != nil {
		return nil, err
	}
	if nonce == nil {
		return nil, ErrNoSpareNonceAccount
	}

	now := time.Now().UTC()
	query = "UPDATE nonce_accounts SET call_id=?, updated_at=? WHERE address=? AND mix IS NULL AND call_id IS NULL"
	if err := s.execOne(ctx, tx, query, call, now, nonce.Address); err != nil {
		return nil, fmt.Errorf("reserve nonce account: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	nonce.CallId = sql.NullString{Valid: true, String: call}
	nonce.UpdatedAt = now
	return nonce, nil
}

// ReleaseNonceAccountForCall returns an unused reservation without releasing
// a nonce that has since been assigned to another call or locked by a user.
func (s *SQLite3Store) ReleaseNonceAccountForCall(ctx context.Context, address, call string) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	_, err := s.db.ExecContext(ctx, "UPDATE nonce_accounts SET call_id=NULL, updated_at=? WHERE address=? AND call_id=? AND mix IS NULL"+
		" AND NOT EXISTS (SELECT 1 FROM system_calls WHERE id=nonce_accounts.call_id AND nonce_account=nonce_accounts.address)"+
		" AND NOT EXISTS ("+pendingNonceProposal+")",
		time.Now().UTC(), address, call, common.RequestStateDone, common.RequestStateFailed)
	return err
}

func (s *SQLite3Store) ReleaseLockedNonceAccount(ctx context.Context, nonce *NonceAccount) error {
	return s.releaseNonceAccount(ctx, nonce, nil)
}

// ReleaseNonceAccountAfterCall is for a terminal call whose nonce was already
// refreshed, or whose transaction was never submitted. The caller verifies RPC.
func (s *SQLite3Store) ReleaseNonceAccountAfterCall(ctx context.Context, nonce *NonceAccount, call *SystemCall) error {
	if call.RequestId != nonce.CallId.String || call.NonceAccount != nonce.Address || (call.State != common.RequestStateDone && call.State != common.RequestStateFailed) {
		return ErrNonceAccountChanged
	}
	return s.releaseNonceAccount(ctx, nonce, call)
}

func (s *SQLite3Store) releaseNonceAccount(ctx context.Context, nonce *NonceAccount, call *SystemCall) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	args := append([]any{time.Now().UTC()}, nonce.snapshotArgs()...)
	// Call state is part of the decision too: a call can arrive or finish
	// while RPC is in flight without changing the nonce row.
	query := "UPDATE nonce_accounts SET mix=NULL, call_id=NULL, updated_at=? WHERE " + nonceSnapshotWhere
	if nonce.LockedByUserOnly() {
		// Initial calls can still be queued behind another call waiting for
		// Prepare. Their user reservation is already in use, even before the
		// observer fills call_id. Check inside the same transaction as release.
		user, err := userFromRow(tx.QueryRowContext(ctx, "SELECT "+strings.Join(userCols, ",")+" FROM users WHERE mix_address=?", nonce.Mix.String))
		if err != nil {
			return err
		}
		if user != nil {
			query += " AND NOT EXISTS (SELECT 1 FROM system_calls WHERE nonce_account=nonce_accounts.address AND call_type=? AND state=? AND substr(public,17)=?)"
			args = append(args, CallTypeMain, common.RequestStateInitial, hex.EncodeToString(user.IdBytes()))
		}
	}
	if call == nil {
		query += " AND NOT EXISTS (SELECT 1 FROM system_calls WHERE id=nonce_accounts.call_id AND nonce_account=nonce_accounts.address)"
	} else {
		query += " AND EXISTS (SELECT 1 FROM system_calls WHERE id=nonce_accounts.call_id AND nonce_account=nonce_accounts.address AND state=? AND updated_at=?)"
		args = append(args, call.State, call.UpdatedAt)
	}
	query += " AND NOT EXISTS (" + pendingNonceProposal + ")"
	args = append(args, common.RequestStateDone, common.RequestStateFailed)
	_, err = tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("UPDATE nonce_accounts %s %v", nonce.Address, err)
	}

	return tx.Commit()
}

// WriteNonceProposal commits the intent before any network submission. In-flight
// retries keep the original bytes; terminal rejections may start a new attempt.
func (s *SQLite3Store) WriteNonceProposal(ctx context.Context, nonce *NonceAccount, action uint8, extra string) (*NonceProposal, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer common.Rollback(tx)
	if !nonce.CallId.Valid {
		return nil, ErrNonceAccountChanged
	}
	exists, err := s.checkExistence(ctx, tx, "SELECT address FROM nonce_accounts WHERE "+nonceSnapshotWhere, nonce.snapshotArgs()...)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNonceAccountChanged
	}
	p, err := nonceProposalFromRow(tx.QueryRowContext(ctx, nonceProposalQuery, common.RequestStateFailed, nonce.CallId.String))
	if err != nil {
		return nil, err
	}
	next := &NonceProposal{Address: nonce.Address, Hash: nonce.Hash, Action: action, Extra: extra}
	switch {
	case p == nil:
		err = s.execOne(ctx, tx, "INSERT INTO nonce_proposals (call_id,nonce_address,nonce_hash,action,extra) VALUES (?,?,?,?,?)", nonce.CallId.String, next.Address, next.Hash, action, extra)
	case p.RejectedBy != "":
		next.RetryId = p.RejectedBy
		err = s.execOne(ctx, tx, "UPDATE nonce_proposals SET nonce_address=?,nonce_hash=?,action=?,extra=?,retry_id=? WHERE call_id=?", next.Address, next.Hash, action, extra, next.RetryId, nonce.CallId.String)
	case p.Address != nonce.Address || p.Hash != nonce.Hash:
		return nil, ErrNonceAccountChanged
	case p.Action != action:
		return nil, ErrNonceAccountChanged
	default:
		next = p
	}
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return next, nil
}

func (s *SQLite3Store) ReadNonceProposal(ctx context.Context, nonce *NonceAccount) (*NonceProposal, error) {
	p, err := nonceProposalFromRow(s.db.QueryRowContext(ctx, nonceProposalQuery, common.RequestStateFailed, nonce.CallId.String))
	if err == nil && p != nil && p.RejectedBy == "" && (p.Address != nonce.Address || p.Hash != nonce.Hash) {
		return nil, ErrNonceAccountChanged
	}
	return p, err
}

func (s *SQLite3Store) ListLockedNonceAccounts(ctx context.Context) ([]*NonceAccount, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	sql := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE mix IS NOT NULL OR call_id IS NOT NULL ORDER BY updated_at ASC", strings.Join(nonceAccountCols, ","))
	rows, err := s.db.QueryContext(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var as []*NonceAccount
	for rows.Next() {
		nonce, err := nonceAccountFromRow(rows)
		if err != nil {
			return nil, err
		}
		as = append(as, nonce)
	}
	return as, rows.Err()
}

func (s *SQLite3Store) ListNonceAccountsByMix(ctx context.Context, mix string) ([]*NonceAccount, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE mix=? ORDER BY updated_at ASC", strings.Join(nonceAccountCols, ","))
	rows, err := s.db.QueryContext(ctx, query, mix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var as []*NonceAccount
	for rows.Next() {
		nonce, err := nonceAccountFromRow(rows)
		if err != nil {
			return nil, err
		}
		as = append(as, nonce)
	}
	return as, rows.Err()
}

func (s *SQLite3Store) ListNonceAccounts(ctx context.Context) ([]*NonceAccount, error) {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	sql := fmt.Sprintf("SELECT %s FROM nonce_accounts LIMIT 500", strings.Join(nonceAccountCols, ","))
	rows, err := s.db.QueryContext(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var as []*NonceAccount
	for rows.Next() {
		nonce, err := nonceAccountFromRow(rows)
		if err != nil {
			return nil, err
		}
		as = append(as, nonce)
	}
	return as, nil
}

func (s *SQLite3Store) ReadNonceAccount(ctx context.Context, address string) (*NonceAccount, error) {
	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE address=?", strings.Join(nonceAccountCols, ","))
	row := s.db.QueryRowContext(ctx, query, address)

	return nonceAccountFromRow(row)
}

func (s *SQLite3Store) ReadNonceAccountByCall(ctx context.Context, callId string) (*NonceAccount, error) {
	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE call_id=?", strings.Join(nonceAccountCols, ","))
	row := s.db.QueryRowContext(ctx, query, callId)

	return nonceAccountFromRow(row)
}

func (s *SQLite3Store) ReadSpareNonceAccount(ctx context.Context) (*NonceAccount, error) {
	query := fmt.Sprintf("SELECT %s FROM nonce_accounts WHERE mix IS NULL AND call_id IS NULL ORDER BY updated_at ASC LIMIT 1", strings.Join(nonceAccountCols, ","))
	row := s.db.QueryRowContext(ctx, query)

	return nonceAccountFromRow(row)
}

func (s *SQLite3Store) CountNonceAccounts(ctx context.Context) (int, error) {
	query := "SELECT COUNT(*) FROM nonce_accounts"
	row := s.db.QueryRowContext(ctx, query)

	var count int
	err := row.Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}

func (s *SQLite3Store) CountSpareNonceAccounts(ctx context.Context) (int, error) {
	query := "SELECT COUNT(*) FROM nonce_accounts WHERE mix IS NULL AND call_id IS NULL"
	row := s.db.QueryRowContext(ctx, query)

	var count int
	err := row.Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}
