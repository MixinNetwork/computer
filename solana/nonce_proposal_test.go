package solana

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/fox-one/mixin-sdk-go/v3"
	"github.com/stretchr/testify/require"
)

func nonceProposalTestNode(t *testing.T) (context.Context, *Node) {
	t.Helper()
	ctx := common.EnableTestEnvironment(context.Background())
	node, md := testBuildNode(ctx, require.New(t), t.TempDir(), 0)
	t.Cleanup(func() { require.NoError(t, node.store.Close()) })
	t.Cleanup(func() { require.NoError(t, md.Close()) })
	node.network = newTestNetwork(node.GetPartySlice())
	return ctx, node
}

func nonceProposalTestTransport(t *testing.T, node *Node, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", r.Header.Get("X-Request-ID"))
		handler(w, r)
	}))
	client := mixin.GetRestyClient()
	previous := client.BaseURL
	client.SetBaseURL(server.URL)
	t.Cleanup(func() {
		client.SetBaseURL(previous)
		server.Close()
	})
	node.mixin = mixin.NewFromAccessToken("nonce-test")
}

func TestNonceProposalRejectedRetryUsesNewSubmissionID(t *testing.T) {
	ctx, node := nonceProposalTestNode(t)
	id := common.UniqueId("rejected-proposal", "operation")
	cid := common.UniqueId(id, "child")
	require.NoError(t, node.store.WriteNonceAccount(ctx, "first", "hash", 0))
	nonce, err := node.store.ReserveNonceAccountForCall(ctx, cid)
	require.NoError(t, err)
	op := &common.Operation{Id: id, Type: OperationTypeDeposit, Extra: []byte{1, 2}}
	_, err = node.store.WriteNonceProposal(ctx, nonce, op.Type, hex.EncodeToString(op.Extra))
	require.NoError(t, err)
	req := &store.Request{Id: "rejected-request", Role: RequestRoleObserver, Action: op.Type, ExtraHEX: hex.EncodeToString(op.Extra),
		State: common.RequestStateInitial, Sequence: 1, CreatedAt: time.Now().UTC(), Output: &mtg.Action{}}
	req.Output.OutputId = req.Id
	require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
	require.NoError(t, node.store.FailRequest(ctx, req, "", nil))
	sent, retryId, err := node.resendNonceProposal(ctx, id, nonce)
	require.NoError(t, err)
	require.False(t, sent, "a terminal rejection must reach the builder")
	require.Equal(t, req.Id, retryId, "nonce-free rebuilds also need a fresh submission ID")
	traces := make(chan string, 2)
	nonceProposalTestTransport(t, node, func(w http.ResponseWriter, r *http.Request) {
		traces <- r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"data": mixin.SafeTransactionRequest{
			State: mixin.SafeUtxoStateSpent, TransactionHash: crypto.Sha256Hash([]byte("sent")).String(),
		}})
	})
	// Use the actual transport path against the local server, including its
	// submission-ID deduplication lookup, rather than the in-memory MTG queue.
	require.NoError(t, node.sendObserverTransactionWithNonce(context.Background(), op, nonce))
	p, err := node.store.ReadNonceProposal(ctx, nonce)
	require.NoError(t, err)
	require.Equal(t, req.Id, p.RetryId)
	baseTrace := node.groupTransactionTraceId(fmt.Sprintf("SESSION:%s:OBSERVER:%s", id, node.id))
	retryTrace := node.groupTransactionTraceId(fmt.Sprintf("SESSION:%s:OBSERVER:%s", common.UniqueId(id, req.Id), node.id))
	require.NotEqual(t, baseTrace, retryTrace)
	require.Equal(t, "/safe/transactions/"+retryTrace, <-traces)
	sent, _, err = node.resendNonceProposal(context.Background(), id, nonce)
	require.NoError(t, err)
	require.True(t, sent)
	require.Equal(t, "/safe/transactions/"+retryTrace, <-traces)
}
