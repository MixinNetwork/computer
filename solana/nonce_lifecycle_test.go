package solana

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/safe/common"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/require"
)

func nonceLifecycleNode(t *testing.T, getHash func(string) (*solana.Hash, uint64)) (context.Context, *Node, *sql.DB) {
	t.Helper()
	ctx := common.EnableTestEnvironment(context.Background())
	path := filepath.Join(t.TempDir(), "nonces.sqlite")
	st, err := store.OpenSQLite3Store(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	db, err := common.OpenSQLite3Store(path, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	payer := solana.NewWallet()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var address string
		var options struct {
			Commitment string `json:"commitment"`
		}
		if request.Method != "getAccountInfo" || len(request.Params) != 2 || json.Unmarshal(request.Params[0], &address) != nil || json.Unmarshal(request.Params[1], &options) != nil || options.Commitment != "finalized" {
			t.Error("expected finalized nonce lookup")
			http.Error(w, "unexpected RPC request", http.StatusBadRequest)
			return
		}
		var value any
		hash, slot := getHash(address)
		if hash != nil {
			var data bytes.Buffer
			err := bin.NewBinEncoder(&data).Encode(system.NonceAccount{
				Version: 1, State: 1, AuthorizedPubkey: payer.PublicKey(), Nonce: solana.PublicKey(*hash),
			})
			if err != nil {
				t.Error(err)
				return
			}
			value = map[string]any{"owner": solana.SystemProgramID.String(), "lamports": 1_461_600,
				"data": []string{base64.StdEncoding.EncodeToString(data.Bytes()), "base64"}}
		}
		result := map[string]any{"context": map[string]any{"slot": slot}, "value": value}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return ctx, &Node{store: st, solana: solanaApp.NewClient(server.URL), conf: &Configuration{SolanaKey: payer.PrivateKey.String()}}, db
}

func TestNonceCleanupDoesNotReleaseConcurrentClaim(t *testing.T) {
	hash := solana.Hash{1}
	entered, proceed := make(chan struct{}), make(chan struct{})
	ctx, node, db := nonceLifecycleNode(t, func(string) (*solana.Hash, uint64) {
		close(entered)
		<-proceed
		return &hash, 100
	})
	t.Cleanup(func() {
		select {
		case <-proceed:
		default:
			close(proceed)
		}
	})
	address := solana.NewWallet().PublicKey().String()
	require.NoError(t, node.store.WriteNonceAccount(ctx, address, hash.String(), 0))
	require.NoError(t, node.store.LockNonceAccountWithMix(ctx, address, "user"))
	_, err := db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=?", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { finished <- node.releaseNonceAccounts(ctx) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not reach RPC")
	}
	nonce, err := node.store.ReadNonceAccount(ctx, address)
	require.NoError(t, err)
	require.NoError(t, node.OccupyNonceAccountByCall(ctx, nonce, "new-main-call"))
	close(proceed)
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not finish")
	}
	claimed, err := node.store.ReadNonceAccount(ctx, address)
	require.NoError(t, err)
	require.True(t, claimed.Valid("new-main-call"))
	require.Equal(t, "new-main-call", claimed.CallId.String)
	require.ErrorIs(t, node.OccupyNonceAccountByCall(ctx, nonce, "other-call"), store.ErrNonceAccountChanged)
}

func TestNonceCleanupMakesProgressWhileFinalityLags(t *testing.T) {
	oldHash, advancedHash := solana.Hash{1}, solana.Hash{2}
	addresses := make([]string, 4)
	for i := range addresses {
		addresses[i] = solana.NewWallet().PublicKey().String()
	}
	ctx, node, db := nonceLifecycleNode(t, func(address string) (*solana.Hash, uint64) {
		switch address {
		case addresses[1]:
			return &advancedHash, 100
		case addresses[2]:
			return nil, 100
		default:
			return &oldHash, 100
		}
	})
	for i, address := range addresses {
		require.NoError(t, node.store.WriteNonceAccount(ctx, address, oldHash.String(), 0))
		nonce, err := node.store.ReadNonceAccount(ctx, address)
		require.NoError(t, err)
		id := fmt.Sprintf("call-%d", i)
		if i == 3 {
			require.NoError(t, node.store.LockNonceAccountWithMix(ctx, address, "expired-user"))
			_, err := db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=? WHERE address=?", time.Now().UTC().Add(-time.Hour), address)
			require.NoError(t, err)
			continue
		}
		require.NoError(t, node.store.OccupyNonceAccountByCall(ctx, nonce, id))
		state := int64(common.RequestStatePending)
		if i == 0 {
			state = common.RequestStateDone
		}
		require.NoError(t, node.store.TestWriteCall(ctx, &store.SystemCall{
			RequestId: id, NonceAccount: address, Type: store.CallTypeMain, State: state,
			MessageHash: id, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}))
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	err := node.releaseNonceAccounts(ctx)
	require.ErrorContains(t, err, "nonce account missing")
	require.NoError(t, ctx.Err(), "one lagging account must not block cleanup")
	for i, address := range addresses {
		nonce, err := node.store.ReadNonceAccount(ctx, address)
		require.NoError(t, err)
		switch i {
		case 0, 2:
			require.True(t, nonce.CallId.Valid)
		case 1:
			require.False(t, nonce.CallId.Valid)
			require.Equal(t, advancedHash.String(), nonce.Hash)
			require.Equal(t, "call-1", nonce.UpdatedBy.String)
		case 3:
			require.False(t, nonce.Mix.Valid)
		}
	}
}

func TestReserveUserNonceRefreshesHashAndReleasesMissingAccount(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			currentHash := solana.Hash{2}
			ctx, node, _ := nonceLifecycleNode(t, func(string) (*solana.Hash, uint64) {
				if missing {
					return nil, 100
				}
				return &currentHash, 100
			})
			for range 2 {
				require.NoError(t, node.store.WriteNonceAccount(ctx, solana.NewWallet().PublicKey().String(), solana.Hash{1}.String(), 0))
			}
			nonce, err := node.reserveNonceAccountForMix(ctx, "user")
			if missing {
				require.Error(t, err)
				require.Nil(t, nonce)
				count, err := node.store.CountSpareNonceAccounts(ctx)
				require.NoError(t, err)
				require.Equal(t, 2, count)
				return
			}
			require.NoError(t, err)
			require.Equal(t, currentHash.String(), nonce.Hash)
			require.EqualValues(t, 100, nonce.HashSlot)
			require.Equal(t, "user", nonce.Mix.String)
			stored, err := node.store.ReadNonceAccount(ctx, nonce.Address)
			require.NoError(t, err)
			require.Equal(t, nonce, stored)
		})
	}
}

func TestNonceCleanupRejectsOlderRPCSlot(t *testing.T) {
	oldHash, currentHash, nextHash := solana.Hash{1}, solana.Hash{2}, solana.Hash{3}
	type reply struct {
		hash solana.Hash
		slot uint64
	}
	replies := make(chan reply, 4)
	replies <- reply{currentHash, 100}
	replies <- reply{oldHash, 99}
	replies <- reply{currentHash, 100}
	replies <- reply{nextHash, 101}
	close(replies)
	ctx, node, _ := nonceLifecycleNode(t, func(string) (*solana.Hash, uint64) {
		r, ok := <-replies
		if !ok {
			t.Error("unexpected nonce lookup")
			return nil, 0
		}
		return &r.hash, r.slot
	})
	address := solana.NewWallet().PublicKey().String()
	require.NoError(t, node.store.WriteNonceAccount(ctx, address, oldHash.String(), 90))
	reserve := func(id string) *store.NonceAccount {
		t.Helper()
		nonce, err := node.store.ReserveNonceAccountForCall(ctx, id)
		require.NoError(t, err)
		require.NoError(t, node.store.TestWriteCall(ctx, &store.SystemCall{
			RequestId: id, NonceAccount: address, Type: store.CallTypeDeposit,
			State: common.RequestStatePending, MessageHash: id,
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}))
		return nonce
	}
	reserve("executed-a")
	require.NoError(t, node.releaseNonceAccounts(ctx)) // A advanced the nonce at slot 100.
	live := reserve("unexecuted-b")
	require.Equal(t, currentHash.String(), live.Hash)
	require.EqualValues(t, 100, live.HashSlot)
	require.ErrorIs(t, node.releaseNonceAccounts(ctx), store.ErrNonceAccountChanged)
	stored, err := node.store.ReadNonceAccount(ctx, address)
	require.NoError(t, err)
	require.Equal(t, live, stored, "slot 99 must not free B or roll back its hash")
	_, err = node.store.ReserveNonceAccountForCall(ctx, "next-call")
	require.ErrorIs(t, err, store.ErrNoSpareNonceAccount)
	require.NoError(t, node.releaseNonceAccounts(ctx)) // Slot 100 still shows B's unconsumed hash.
	stored, err = node.store.ReadNonceAccount(ctx, address)
	require.NoError(t, err)
	require.Equal(t, live, stored)
	require.NoError(t, node.releaseNonceAccounts(ctx)) // Slot 101 proves B consumed it.
	next := reserve("next-call")
	require.Equal(t, nextHash.String(), next.Hash)
	require.EqualValues(t, 101, next.HashSlot)
	require.Empty(t, replies)
}

func TestNonceRefreshRejectsOlderRPCSlot(t *testing.T) {
	for _, action := range []string{"boot", "user reservation", "expired user"} {
		t.Run(action, func(t *testing.T) {
			oldHash, currentHash := solana.Hash{1}, solana.Hash{2}
			ctx, node, db := nonceLifecycleNode(t, func(string) (*solana.Hash, uint64) {
				return &oldHash, 99
			})
			address := solana.NewWallet().PublicKey().String()
			require.NoError(t, node.store.WriteNonceAccount(ctx, address, currentHash.String(), 100))
			require.NoError(t, node.store.WriteNonceAccount(ctx, solana.NewWallet().PublicKey().String(), currentHash.String(), 100))
			switch action {
			case "boot":
				require.NoError(t, node.checkNonceAccounts(ctx))
			case "user reservation":
				nonce, err := node.reserveNonceAccountForMix(ctx, "user")
				require.ErrorIs(t, err, store.ErrNonceAccountChanged)
				require.Nil(t, nonce)
			case "expired user":
				require.NoError(t, node.store.LockNonceAccountWithMix(ctx, address, "user"))
				_, err := db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=? WHERE address=?", time.Now().UTC().Add(-time.Hour), address)
				require.NoError(t, err)
				require.ErrorIs(t, node.releaseNonceAccounts(ctx), store.ErrNonceAccountChanged)
			}
			nonces, err := node.store.ListNonceAccounts(ctx)
			require.NoError(t, err)
			for _, nonce := range nonces {
				require.Equal(t, currentHash.String(), nonce.Hash)
				require.EqualValues(t, 100, nonce.HashSlot)
				require.False(t, nonce.CallId.Valid)
				require.Equal(t, action == "expired user" && nonce.Address == address, nonce.Mix.Valid)
			}
		})
	}
}

func TestNoncePoolProgressWithPendingCleanupBacklog(t *testing.T) {
	hash, advanced := solana.Hash{2}, solana.Hash{3}
	var consumed atomic.Bool
	ctx, node, db := nonceLifecycleNode(t, func(string) (*solana.Hash, uint64) {
		if consumed.Load() {
			return &advanced, 101
		}
		return &hash, 100
	})
	now := time.Now().UTC()
	for range 101 {
		require.NoError(t, node.store.WriteNonceAccount(ctx, solana.NewWallet().PublicKey().String(), hash.String(), 100))
	}
	accounts, err := node.store.ListNonceAccounts(ctx)
	require.NoError(t, err)
	// Executed Main calls have recycled their nonces but still need cleanup
	// reservations before their MTG confirmations can be submitted.
	for i := range 100 {
		id := common.UniqueId("executed-main", fmt.Sprint(i))
		require.NoError(t, node.store.TestWriteCall(ctx, &store.SystemCall{
			RequestId: id, Superior: id, Type: store.CallTypeMain,
			NonceAccount: accounts[i].Address, MessageHash: id,
			Public: fmt.Sprintf("%016x%016x", 0, store.StartUserId.Int64()+int64(i)),
			State:  common.RequestStatePending, Signature: sql.NullString{Valid: true},
			WithdrawalTraces: sql.NullString{Valid: true},
			CreatedAt:        now.Add(-time.Hour).Add(time.Duration(i) * time.Second), UpdatedAt: now,
		}))
	}
	// Accepted Initial calls hold 100 leases, respecting the per-user limit.
	for i := range 20 {
		uid := store.StartUserId.Int64() + 100 + int64(i)
		_, err := db.ExecContext(ctx, "INSERT INTO users(user_id,request_id,mix_address,chain_address,public,created_at) VALUES(?,?,?,?,?,?)",
			fmt.Sprint(uid), fmt.Sprintf("user-request-%d", i), fmt.Sprintf("mix-%d", i), solana.NewWallet().PublicKey().String(), "", now)
		require.NoError(t, err)
	}
	for i := range 100 {
		nonce, err := node.store.ReserveNonceAccountForMix(ctx, fmt.Sprintf("mix-%d", i/5))
		require.NoError(t, err)
		id := common.UniqueId("initial-main", fmt.Sprint(i))
		require.NoError(t, node.store.TestWriteCall(ctx, &store.SystemCall{
			RequestId: id, Superior: id, Type: store.CallTypeMain,
			NonceAccount: nonce.Address, MessageHash: id,
			Public: fmt.Sprintf("%016x%016x", 0, store.StartUserId.Int64()+100+int64(i/5)),
			State:  common.RequestStateInitial, CreatedAt: now, UpdatedAt: now,
		}))
	}
	depositID := common.UniqueId("nonce-backlog", "deposit")
	nonce, err := node.store.ReserveNonceAccountForCall(ctx, depositID)
	require.NoError(t, err)
	require.NoError(t, node.store.TestWriteCall(ctx, &store.SystemCall{
		RequestId: depositID, Type: store.CallTypeDeposit, NonceAccount: nonce.Address, MessageHash: depositID,
		State: common.RequestStatePending, Signature: sql.NullString{Valid: true},
		WithdrawalTraces: sql.NullString{Valid: true}, CreatedAt: now, UpdatedAt: now,
	}))
	_, err = db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=?", now.Add(-time.Hour))
	require.NoError(t, err)
	require.NoError(t, node.releaseNonceAccounts(ctx))
	require.NoError(t, node.createNonceAccounts(ctx))
	count, err := node.store.CountSpareNonceAccounts(ctx)
	require.NoError(t, err)
	require.Zero(t, count, "expiry and pool growth cannot free capacity here")

	sequences, err := node.store.ListSignedCalls(ctx)
	require.NoError(t, err)
	deposit := sequences[store.CallTypeDeposit+":"+depositID]
	require.Len(t, deposit, 1, "the call holding the last nonce must remain runnable behind the cleanup backlog")
	var cleanupID string
	for _, calls := range sequences {
		if calls[0].Type != store.CallTypeMain {
			continue
		}
		cleanupID = common.UniqueId(common.UniqueId(calls[0].RequestId, "confirm-success"), "post-process")
		err := node.processSuccessedCall(ctx, calls[0], nil, nil, []confirmCallRecord{{
			Status: FlagConfirmCallSuccess, CallId: calls[0].RequestId, Signature: solana.Signature{1},
		}})
		require.ErrorIs(t, err, store.ErrNoSpareNonceAccount)
	}
	// Simulate the selected Deposit consuming its nonce on Solana. Cleanup
	// can then recycle it and unblock a waiting Main's PostProcess allocation.
	consumed.Store(true)
	nonce, err = node.store.ReadNonceAccountByCall(ctx, deposit[0].RequestId)
	require.NoError(t, err)
	require.NotNil(t, nonce)
	require.NoError(t, node.releaseNonceAccount(ctx, nonce))
	cleanup, err := node.store.ReserveNonceAccountForCall(ctx, cleanupID)
	require.NoError(t, err)
	require.Equal(t, nonce.Address, cleanup.Address)
	require.Equal(t, advanced.String(), cleanup.Hash)
	require.EqualValues(t, 101, cleanup.HashSlot)
}
