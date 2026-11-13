package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

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
		require.NoError(t, s.WriteNonceAccount(ctx, fmt.Sprintf("nonce-%d", i), fmt.Sprintf("hash-%d", i)))
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

func TestReserveNonceAccountExhaustionAndRelease(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite3Store(filepath.Join(t.TempDir(), "nonces.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.WriteNonceAccount(ctx, "user-nonce", "user-hash"))
	require.NoError(t, s.LockNonceAccountWithMix(ctx, "user-nonce", "user"))
	require.NoError(t, s.WriteNonceAccount(ctx, "free-nonce", "free-hash"))

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
		require.Equal(t, nonce, again)
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
	require.NoError(t, s.OccupyNonceAccountByCall(ctx, "user-nonce", "user-call"))
	require.NoError(t, s.ReleaseNonceAccountForCall(ctx, "user-nonce", "user-call"))
	locked, err := s.ReadNonceAccount(ctx, "user-nonce")
	require.NoError(t, err)
	require.Equal(t, "user", locked.Mix.String)
	require.Equal(t, "user-call", locked.CallId.String)

	_, err = s.ReserveNonceAccountForCall(ctx, "")
	require.Error(t, err)
}
