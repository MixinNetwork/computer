package solana

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/stretchr/testify/require"
)

func TestPrepareNonceExhaustionPreservesMainNonce(t *testing.T) {
	ctx := common.EnableTestEnvironment(context.Background())
	root := t.TempDir()
	node, md := testBuildNode(ctx, require.New(t), root, 0)
	t.Cleanup(func() { require.NoError(t, node.store.Close()) })
	t.Cleanup(func() { require.NoError(t, md.Close()) })
	network := newTestNetwork(node.GetPartySlice())
	node.network = network

	hash := solana.Hash{1}
	var account bytes.Buffer
	require.NoError(t, bin.NewBinEncoder(&account).Encode(system.NonceAccount{
		Version: 1, State: 1, AuthorizedPubkey: node.SolanaPayer(), Nonce: solana.PublicKey(hash),
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "getAccountInfo" {
			t.Errorf("unexpected RPC method: %s", request.Method)
			http.Error(w, "unexpected RPC method", http.StatusBadRequest)
			return
		}
		result := map[string]any{"context": map[string]any{"slot": 1}, "value": map[string]any{
			"owner": solana.SystemProgramID.String(), "lamports": 1_461_600,
			"data": []string{base64.StdEncoding.EncodeToString(account.Bytes()), "base64"},
		}}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	node.solana = solanaApp.NewClient(server.URL)
	userRequest := &store.Request{
		Id: common.UniqueId("nonce exhaustion", "user"), Role: RequestRoleUser, Action: OperationTypeAddUser,
		State: common.RequestStateInitial, CreatedAt: time.Now().UTC(), Output: &mtg.Action{},
	}
	userRequest.Output.OutputId = userRequest.Id
	require.NoError(t, node.store.WriteRequestIfNotExist(ctx, userRequest))
	require.NoError(t, node.store.WriteUserWithRequest(ctx, userRequest, store.StartUserId.String(), "main-user", solana.NewWallet().PublicKey().String(), ""))

	id := common.UniqueId("prepare nonce exhaustion", "main")
	req := &store.Request{
		Id: id, MixinHash: crypto.Sha256Hash([]byte(id)), Role: RequestRoleUser, Action: OperationTypeSystemCall,
		State: common.RequestStateInitial, CreatedAt: time.Now().UTC(), ExtraHEX: hex.EncodeToString(make([]byte, 25)),
	}
	require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
	mainNonce := solana.NewWallet().PublicKey().String()
	require.NoError(t, node.store.WriteNonceAccount(ctx, mainNonce, hash.String(), 0))
	require.NoError(t, node.store.LockNonceAccountWithMix(ctx, mainNonce, "main-user"))
	call := &store.SystemCall{
		RequestId: id, RequestHash: req.MixinHash.String(), Type: store.CallTypeMain, NonceAccount: mainNonce,
		Public: hex.EncodeToString(append(make([]byte, 8), store.StartUserId.FillBytes(make([]byte, 8))...)),
		State:  common.RequestStateInitial, CreatedAt: req.CreatedAt, UpdatedAt: req.CreatedAt,
	}
	require.NoError(t, node.store.TestWriteCall(ctx, call))
	// A later accepted call has not been visited when the first call runs out
	// of Prepare nonces. Its user-only reservation still belongs to that call.
	laterNonce := solana.NewWallet().PublicKey().String()
	require.NoError(t, node.store.WriteNonceAccount(ctx, laterNonce, hash.String(), 0))
	require.NoError(t, node.store.LockNonceAccountWithMix(ctx, laterNonce, "main-user"))
	laterRequest := *req
	laterRequest.Id = common.UniqueId(id, "later-request")
	laterRequest.MixinHash = crypto.Sha256Hash([]byte(laterRequest.Id))
	laterRequest.CreatedAt = req.CreatedAt.Add(time.Second)
	require.NoError(t, node.store.WriteRequestIfNotExist(ctx, &laterRequest))
	later := *call
	later.RequestId = laterRequest.Id
	later.RequestHash = laterRequest.MixinHash.String()
	later.MessageHash = laterRequest.Id
	later.NonceAccount = laterNonce
	later.CreatedAt = laterRequest.CreatedAt
	require.NoError(t, node.store.TestWriteCall(ctx, &later))
	require.ErrorIs(t, node.handleUnconfirmedCalls(ctx), store.ErrNoSpareNonceAccount)

	// Age the reservation without sleeping through the 20-minute timeout.
	db, err := common.OpenSQLite3Store(filepath.Join(root, "mpc.sqlite3"), "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_at=?", time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	for range 2 {
		nonce, err := node.store.ReadNonceAccount(ctx, mainNonce)
		require.NoError(t, err)
		require.True(t, nonce.Expired())
		require.NoError(t, node.releaseNonceAccounts(ctx))
		nonce, err = node.store.ReadNonceAccount(ctx, mainNonce)
		require.NoError(t, err)
		require.True(t, nonce.Valid(id), "cleanup must preserve the waiting main call's reservation")
		require.Equal(t, id, nonce.CallId.String)
		queued, err := node.store.ReadNonceAccount(ctx, laterNonce)
		require.NoError(t, err)
		require.True(t, queued.Valid(later.RequestId), "cleanup must also preserve unvisited Main calls")
		require.False(t, queued.CallId.Valid)
		require.ErrorIs(t, node.handleUnconfirmedCalls(ctx), store.ErrNoSpareNonceAccount)
		require.Empty(t, network.mtgChannel(node.id))
	}

	// Once the pool has room, the same call can proceed with its original nonce.
	require.NoError(t, node.store.WriteNonceAccount(ctx, solana.NewWallet().PublicKey().String(), hash.String(), 0))
	require.NoError(t, node.handleUnconfirmedCalls(ctx))
	var out mtg.Action
	select {
	case data := <-network.mtgChannel(node.id):
		require.NoError(t, json.Unmarshal(data, &out))
	default:
		t.Fatal("main call did not resume after a nonce became available")
	}
	_, extra := mtg.DecodeMixinExtraHEX(out.Extra)
	op := decodeOperation(extra[64:])
	require.EqualValues(t, OperationTypeConfirmNonce, op.Type)
	require.EqualValues(t, ConfirmFlagNonceAvailable, op.Extra[0])
	require.Empty(t, network.mtgChannel(node.id), "later Main must wait before allocating a Prepare nonce")
	// The earlier Main still needs cleanup while Pending. Its later Main must
	// leave the last spare available for that cleanup to make progress.
	_, err = db.ExecContext(ctx, "UPDATE system_calls SET state=? WHERE id=?", common.RequestStatePending, id)
	require.NoError(t, err)
	require.NoError(t, node.handleUnconfirmedCalls(ctx))
	require.Empty(t, network.mtgChannel(node.id))
	cleanupID := common.UniqueId(id, "cleanup")
	cleanup, err := node.store.ReserveNonceAccountForCall(ctx, cleanupID)
	require.NoError(t, err, "older Main must be able to reserve the last spare")
	require.NoError(t, node.store.ReleaseNonceAccountForCall(ctx, cleanup.Address, cleanupID))
	// Once the earlier Main is terminal, the next one can prepare normally.
	_, err = db.ExecContext(ctx, "UPDATE system_calls SET state=? WHERE id=?", common.RequestStateDone, id)
	require.NoError(t, err)
	require.NoError(t, node.handleUnconfirmedCalls(ctx))
	select {
	case data := <-network.mtgChannel(node.id):
		require.NoError(t, json.Unmarshal(data, &out))
		_, extra := mtg.DecodeMixinExtraHEX(out.Extra)
		require.EqualValues(t, ConfirmFlagNonceAvailable, decodeOperation(extra[64:]).Extra[0])
	default:
		t.Fatal("later Main call did not resume with its original reservation")
	}
	reason, err := node.store.ReadFailReason(ctx, id)
	require.NoError(t, err)
	require.Empty(t, reason)

	// The protection ends when the call finishes, so its nonce can be released.
	_, err = db.ExecContext(ctx, "UPDATE system_calls SET state=?", common.RequestStateDone)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_by=? WHERE address=?", id, mainNonce)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "UPDATE nonce_accounts SET updated_by=? WHERE address=?", later.RequestId, laterNonce)
	require.NoError(t, err)
	require.NoError(t, node.releaseNonceAccounts(ctx))
	nonce, err := node.store.ReadNonceAccount(ctx, mainNonce)
	require.NoError(t, err)
	require.False(t, nonce.Mix.Valid)
	require.False(t, nonce.CallId.Valid)

	t.Run("reject another user's nonce", func(t *testing.T) {
		require.NoError(t, node.store.LockNonceAccountWithMix(ctx, mainNonce, "another-user"))
		other := *call
		other.RequestId = common.UniqueId(id, "wrong-owner")
		other.MessageHash = other.RequestId
		require.NoError(t, node.store.TestWriteCall(ctx, &other))
		require.NoError(t, node.handleUnconfirmedCalls(ctx))
		reason, err := node.store.ReadFailReason(ctx, other.RequestId)
		require.NoError(t, err)
		require.Equal(t, "expired or invalid nonce", reason)
		nonce, err := node.store.ReadNonceAccount(ctx, mainNonce)
		require.NoError(t, err)
		require.Equal(t, "another-user", nonce.Mix.String)
		require.False(t, nonce.CallId.Valid)
	})
}
