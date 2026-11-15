package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/stretchr/testify/require"
)

func TestReserveNonceAccountForCallConcurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nonces.sqlite")
	s, err := OpenSQLite3Store(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	const calls = 16
	const retries = 4
	for i := range calls {
		require.NoError(t, s.WriteNonceAccount(ctx, fmt.Sprintf("nonce-%d", i), fmt.Sprintf("hash-%d", i), 0))
	}

	type result struct {
		call  string
		nonce *NonceAccount
		err   error
	}
	results := make(chan result, calls*retries)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range calls * retries {
		wg.Go(func() {
			<-start
			call := fmt.Sprintf("call-%d", i%calls)
			nonce, err := s.ReserveNonceAccountForCall(ctx, call)
			results <- result{call, nonce, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)

	byCall := make(map[string]string)
	byAddress := make(map[string]string)
	for result := range results {
		require.NoError(t, result.err)
		require.NotNil(t, result.nonce)
		require.True(t, result.nonce.CallId.Valid)
		require.Equal(t, result.call, result.nonce.CallId.String)
		if address, ok := byCall[result.call]; ok {
			require.Equal(t, address, result.nonce.Address, "retries must reuse their reservation")
		}
		if call, ok := byAddress[result.nonce.Address]; ok {
			require.Equal(t, call, result.call, "different calls must not share a nonce")
		}
		byCall[result.call] = result.nonce.Address
		byAddress[result.nonce.Address] = result.call
	}
	require.Len(t, byCall, calls)
	require.Len(t, byAddress, calls)
	count, err := s.CountSpareNonceAccounts(ctx)
	require.NoError(t, err)
	require.Zero(t, count)

	// Reservations survive restart, including retries while the pool is full.
	require.NoError(t, s.Close())
	s, err = OpenSQLite3Store(path)
	require.NoError(t, err)
	for call, address := range byCall {
		nonce, err := s.ReserveNonceAccountForCall(ctx, call)
		require.NoError(t, err)
		require.Equal(t, address, nonce.Address)
	}
}

func TestNonceSnapshotGuards(t *testing.T) {
	ctx := common.EnableTestEnvironment(context.Background())
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	read := func() *NonceAccount {
		n, err := s.ReadNonceAccount(ctx, "nonce")
		require.NoError(t, err)
		return n
	}
	require.NoError(t, s.WriteNonceAccount(ctx, "nonce", "hash", 0))
	require.NoError(t, s.LockNonceAccountWithMix(ctx, "nonce", "user"))
	beforeClaim := read()
	require.NoError(t, s.OccupyNonceAccountByCall(ctx, beforeClaim, "call"))
	claimed := read()
	// Cleanup and hash refresh both started before the call claimed the nonce.
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, beforeClaim))
	require.NoError(t, s.UpdateNonceAccount(ctx, beforeClaim, "stale-hash", 1, "old-call"))
	require.Equal(t, claimed, read())
	require.ErrorIs(t, s.OccupyNonceAccountByCall(ctx, beforeClaim, "other-call"), ErrNonceAccountChanged)

	// Even reassignment to the same user and call is a new reservation.
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, claimed))
	require.NoError(t, s.LockNonceAccountWithMix(ctx, "nonce", "user"))
	require.NoError(t, s.OccupyNonceAccountByCall(ctx, read(), "call"))
	current := read()
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, claimed))
	require.NoError(t, s.UpdateNonceAccount(ctx, claimed, "stale-hash", 1, "old-call"))
	require.Equal(t, current, read())
	require.ErrorIs(t, s.OccupyNonceAccountByCall(ctx, claimed, "call"), ErrNonceAccountChanged)

	// A proposal can become an active call after cleanup read a missing call.
	for _, state := range []int64{common.RequestStateInitial, common.RequestStatePending, common.RequestStateDone, common.RequestStateFailed} {
		_, err := s.db.ExecContext(ctx, "DELETE FROM system_calls")
		require.NoError(t, err)
		require.NoError(t, s.TestWriteCall(ctx, &SystemCall{
			RequestId: "call", NonceAccount: "nonce", Type: CallTypeMain, State: state,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}))
		require.NoError(t, s.ReleaseLockedNonceAccount(ctx, current))
		require.Equal(t, current, read())
	}
	// Once execution consumes the nonce, a matching snapshot can refresh it.
	require.NoError(t, s.UpdateNonceAccount(ctx, current, "advanced-hash", 1, "call"))
	advanced := read()
	require.Equal(t, "advanced-hash", advanced.Hash)
	require.EqualValues(t, 1, advanced.HashSlot)
	require.False(t, advanced.Mix.Valid)
	require.False(t, advanced.CallId.Valid)
	require.Equal(t, "call", advanced.UpdatedBy.String)
}

func TestNonceHashSlotSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nonces.sqlite")
	s, err := OpenSQLite3Store(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.WriteNonceAccount(ctx, "nonce", "old-hash", 90))
	nonce, err := s.ReserveNonceAccountForCall(ctx, "executed-call")
	require.NoError(t, err)
	require.EqualValues(t, 90, nonce.HashSlot)
	require.NoError(t, s.UpdateNonceAccount(ctx, nonce, "current-hash", 100, "executed-call"))
	require.NoError(t, s.Close())
	s, err = OpenSQLite3Store(path)
	require.NoError(t, err)
	live, err := s.ReserveNonceAccountForCall(ctx, "live-call")
	require.NoError(t, err)
	require.EqualValues(t, 100, live.HashSlot)
	require.Equal(t, "current-hash", live.Hash)
	require.ErrorIs(t, s.UpdateNonceAccount(ctx, live, "old-hash", 99, "live-call"), ErrNonceAccountChanged)
	current, err := s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.Equal(t, live, current)
}

func TestNonceSignedCallsKeepLatePrepareWithFirstMain(t *testing.T) {
	ctx := common.EnableTestEnvironment(context.Background())
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	now := time.Now().UTC()
	public := fmt.Sprintf("%016x%016x", 0, StartUserId.Int64())
	write := func(id, kind, superior string, offset time.Duration) {
		t.Helper()
		require.NoError(t, s.TestWriteCall(ctx, &SystemCall{
			RequestId: id, Superior: superior, Type: kind, Public: public, MessageHash: id,
			State: common.RequestStatePending, Signature: sql.NullString{Valid: true},
			CreatedAt: now.Add(offset), UpdatedAt: now,
		}))
	}
	write("first-main", CallTypeMain, "first-main", 0)
	for i := range 100 {
		write(fmt.Sprintf("deposit-%d", i), CallTypeDeposit, "", time.Duration(i+1)*time.Second)
	}
	write("prepare", CallTypePrepare, "first-main", 2*time.Minute)
	write("later-main", CallTypeMain, "later-main", 3*time.Minute)
	write("post-process", CallTypePostProcess, "", 4*time.Minute)
	sequences, err := s.ListSignedCalls(ctx)
	require.NoError(t, err)
	main := sequences[CallTypeMain+":"+StartUserId.String()]
	require.Len(t, main, 2, "Prepare must be included even when its Main is more than 100 rows earlier")
	require.Equal(t, "prepare", main[0].RequestId)
	require.Equal(t, "first-main", main[1].RequestId, "only the first Main for a user may run")
	for i := range 100 {
		require.Len(t, sequences[fmt.Sprintf("deposit:deposit-%d", i)], 1)
	}
	require.Len(t, sequences[CallTypePostProcess+":post-process"], 1)
	require.Len(t, sequences, 102)
}

func TestReserveNonceAccountForMixConcurrent(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for i := range 8 {
		require.NoError(t, s.WriteNonceAccount(ctx, fmt.Sprintf("nonce-%d", i), "hash", 0))
	}
	type result struct {
		nonce *NonceAccount
		err   error
	}
	results := make(chan result, 16)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			nonce, err := s.ReserveNonceAccountForMix(ctx, "user")
			results <- result{nonce, err}
		})
	}
	wg.Wait()
	close(results)
	addresses := make(map[string]bool)
	for r := range results {
		if r.err != nil {
			require.ErrorIs(t, r.err, ErrNonceAccountLimit)
			continue
		}
		require.Equal(t, "user", r.nonce.Mix.String)
		require.False(t, addresses[r.nonce.Address])
		addresses[r.nonce.Address] = true
	}
	require.Len(t, addresses, MaxNonceAccountsPerMix)
	for i := range 2 {
		_, err := s.ReserveNonceAccountForCall(ctx, fmt.Sprintf("prepare-%d", i))
		require.NoError(t, err)
	}
	_, err = s.ReserveNonceAccountForMix(ctx, "another-user")
	require.ErrorIs(t, err, ErrNoSpareNonceAccount)
	_, err = s.ReserveNonceAccountForCall(ctx, "last-prepare")
	require.NoError(t, err, "internal calls can use the protected final nonce")
}

func TestNonceRetryRenewsReservation(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.WriteNonceAccount(ctx, "nonce", "hash", 0))
	_, err = s.ReserveNonceAccountForCall(ctx, "call")
	require.NoError(t, err)
	_, err = s.db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=?", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	expired, err := s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.True(t, expired.Expired())
	renewed, err := s.ReserveNonceAccountForCall(ctx, "call")
	require.NoError(t, err)
	require.False(t, renewed.Expired())
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, expired))
	current, err := s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.Equal(t, renewed, current)
}

func TestListLockedNonceAccountsIncludesEntirePool(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	for i := range 101 {
		require.NoError(t, s.WriteNonceAccount(ctx, fmt.Sprintf("nonce-%d", i), "hash", 0))
		_, err := s.ReserveNonceAccountForCall(ctx, fmt.Sprintf("call-%d", i))
		require.NoError(t, err)
	}
	locked, err := s.ListLockedNonceAccounts(ctx)
	require.NoError(t, err)
	require.Len(t, locked, 101)
}

func TestNonceProposalSurvivesExpiryAndRestart(t *testing.T) {
	ctx := common.EnableTestEnvironment(context.Background())
	path := filepath.Join(t.TempDir(), "nonces.sqlite")
	s, err := OpenSQLite3Store(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.WriteNonceAccount(ctx, "nonce", "hash", 0))
	nonce, err := s.ReserveNonceAccountForCall(ctx, "call")
	require.NoError(t, err)
	proposal, err := s.WriteNonceProposal(ctx, nonce, 13, "abcd")
	require.NoError(t, err)
	require.Equal(t, "abcd", proposal.Extra)
	_, err = s.db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=?", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = OpenSQLite3Store(path)
	require.NoError(t, err)
	nonce, err = s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.True(t, nonce.Expired())
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, nonce))
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "call"))
	current, err := s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.Equal(t, nonce, current, "an unrecorded submission is still in flight")
	nonce, err = s.ReserveNonceAccountForCall(ctx, "call")
	require.NoError(t, err)
	proposal, err = s.ReadNonceProposal(ctx, nonce)
	require.NoError(t, err)
	require.EqualValues(t, 13, proposal.Action)
	require.Equal(t, "abcd", proposal.Extra)
	proposal, err = s.WriteNonceProposal(ctx, nonce, 13, "different-retry-bytes")
	require.NoError(t, err)
	require.Equal(t, "abcd", proposal.Extra, "retries must publish the original transaction")

	req := &Request{Id: "proposal", Role: 1, Action: 13, ExtraHEX: "abcd", State: common.RequestStateInitial,
		CreatedAt: time.Now().UTC(), Output: &mtg.Action{}}
	req.Output.OutputId = req.Id
	require.NoError(t, s.WriteRequestIfNotExist(ctx, req))
	require.NoError(t, s.FailRequest(ctx, req, "compaction-asset", nil))
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, nonce))
	current, err = s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.Equal(t, "call", current.CallId.String, "compaction may still replay the proposal")
	require.NoError(t, s.ResetRequest(ctx, req.Id, 1))
	require.NoError(t, s.FailRequest(ctx, req, "", nil))
	require.NoError(t, s.ReleaseLockedNonceAccount(ctx, nonce))
	current, err = s.ReadNonceAccount(ctx, "nonce")
	require.NoError(t, err)
	require.False(t, current.CallId.Valid, "terminal rejection can release an unused reservation")
	_, err = s.WriteNonceProposal(ctx, nonce, 13, "late-submission")
	require.ErrorIs(t, err, ErrNonceAccountChanged)
}

func TestReserveNonceAccountExhaustionAndRelease(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.WriteNonceAccount(ctx, "user-nonce", "user-hash", 0))
	require.NoError(t, s.LockNonceAccountWithMix(ctx, "user-nonce", "user"))
	require.NoError(t, s.WriteNonceAccount(ctx, "free-nonce", "free-hash", 0))

	nonce, err := s.ReserveNonceAccountForCall(ctx, "call-a")
	require.NoError(t, err)
	require.Equal(t, "free-nonce", nonce.Address)
	require.Equal(t, "free-hash", nonce.Hash)
	for range 2 {
		missing, err := s.ReserveNonceAccountForCall(ctx, "call-b")
		require.ErrorIs(t, err, ErrNoSpareNonceAccount)
		require.Nil(t, missing)
		again, err := s.ReserveNonceAccountForCall(ctx, "call-a")
		require.NoError(t, err)
		require.Equal(t, nonce.Address, again.Address)
		require.Equal(t, nonce.Hash, again.Hash)
		require.Equal(t, nonce.CallId, again.CallId)
	}
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "wrong-call"))
	again, err := s.ReadNonceAccount(ctx, nonce.Address)
	require.NoError(t, err)
	require.Equal(t, "call-a", again.CallId.String)

	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "call-a"))
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "call-a"))
	again, err = s.ReserveNonceAccountForCall(ctx, "call-b")
	require.NoError(t, err)
	require.Equal(t, nonce.Address, again.Address)
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "call-a"))
	again, err = s.ReadNonceAccount(ctx, nonce.Address)
	require.NoError(t, err)
	require.Equal(t, "call-b", again.CallId.String)

	// This release helper must not release the user's main-call nonce.
	userNonce, err := s.ReadNonceAccount(ctx, "user-nonce")
	require.NoError(t, err)
	require.NoError(t, s.OccupyNonceAccountByCall(ctx, userNonce, "user-call"))
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, "user-nonce", "user-call"))
	locked, err := s.ReadNonceAccount(ctx, "user-nonce")
	require.NoError(t, err)
	require.Equal(t, "user", locked.Mix.String)
	require.Equal(t, "user-call", locked.CallId.String)

	_, err = s.ReserveNonceAccountForCall(ctx, "")
	require.Error(t, err)
}

func TestNonceProposalRetryAfterRejection(t *testing.T) {
	for _, release := range []bool{false, true} {
		t.Run(fmt.Sprintf("release=%t", release), func(t *testing.T) {
			ctx := common.EnableTestEnvironment(context.Background())
			path := filepath.Join(t.TempDir(), "nonces.sqlite")
			s, err := OpenSQLite3Store(path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			for _, address := range []string{"first", "second"} {
				require.NoError(t, s.WriteNonceAccount(ctx, address, "hash-"+address, 0))
			}
			nonce, err := s.ReserveNonceAccountForCall(ctx, "call")
			require.NoError(t, err)
			_, err = s.WriteNonceProposal(ctx, nonce, 14, "abcd")
			require.NoError(t, err)
			reject := func(id, extra, compaction string, sequence uint64) *Request {
				req := &Request{Id: id, MixinHash: crypto.Sha256Hash([]byte(id)), Role: 3, Action: 14, ExtraHEX: extra,
					State: common.RequestStateInitial, Sequence: sequence, CreatedAt: time.Now().UTC(), Output: &mtg.Action{}}
				req.Output.OutputId = id
				require.NoError(t, s.WriteRequestIfNotExist(ctx, req))
				require.NoError(t, s.FailRequest(ctx, req, compaction, nil))
				return req
			}
			req := reject("rejection-1", "abcd", "compaction-asset", 10)
			p, err := s.WriteNonceProposal(ctx, nonce, 14, "changed")
			require.NoError(t, err)
			require.Equal(t, "abcd", p.Extra, "compaction must replay the original proposal")
			require.Empty(t, p.RetryId)
			require.NoError(t, s.ResetRequest(ctx, req.Id, 11))
			require.NoError(t, s.FailRequest(ctx, req, "", nil))
			p, err = s.ReadNonceProposal(ctx, nonce)
			require.NoError(t, err)
			require.Equal(t, req.Id, p.RejectedBy)
			if release {
				original := nonce.Address
				require.NoError(t, s.ReleaseLockedNonceAccount(ctx, nonce))
				nonce, err = s.ReserveNonceAccountForCall(ctx, "call")
				require.NoError(t, err)
				require.NotEqual(t, original, nonce.Address)
			}
			// Identical payloads still need a new submission ID. The old terminal
			// request must not classify this new attempt as already rejected.
			p, err = s.WriteNonceProposal(ctx, nonce, 14, "abcd")
			require.NoError(t, err)
			require.Equal(t, req.Id, p.RetryId)
			require.Empty(t, p.RejectedBy)
			require.NoError(t, s.Close())
			s, err = OpenSQLite3Store(path)
			require.NoError(t, err)
			p, err = s.ReadNonceProposal(ctx, nonce)
			require.NoError(t, err)
			require.Equal(t, req.Id, p.RetryId)
			require.Empty(t, p.RejectedBy)
			require.NoError(t, s.ReleaseLockedNonceAccount(ctx, nonce))
			require.NoError(t, s.ReleaseNonceAccountForCall(ctx, nonce.Address, "call"))
			current, err := s.ReadNonceAccount(ctx, nonce.Address)
			require.NoError(t, err)
			require.Equal(t, "call", current.CallId.String)
			req = reject("rejection-2", "abcd", "", 12)
			p, err = s.WriteNonceProposal(ctx, nonce, 14, "corrected")
			require.NoError(t, err)
			require.Equal(t, req.Id, p.RetryId)
			require.Equal(t, "corrected", p.Extra)
			p, err = s.WriteNonceProposal(ctx, nonce, 14, "changed-again")
			require.NoError(t, err)
			require.Equal(t, "corrected", p.Extra)
			require.Equal(t, req.Id, p.RetryId, "in-flight retries keep the same submission ID")
			// A child can already exist when the enclosing request needs compaction.
			// Its accepted transaction must never be rebuilt, even if a later
			// duplicate request is rejected.
			require.NoError(t, s.TestWriteCall(ctx, &SystemCall{RequestId: "call", NonceAccount: nonce.Address, Type: CallTypePrepare,
				State: common.RequestStatePending, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}))
			reject("duplicate", "corrected", "", 13)
			p, err = s.WriteNonceProposal(ctx, nonce, 14, "must-not-replace")
			require.NoError(t, err)
			require.Equal(t, "corrected", p.Extra)
		})
	}
}
