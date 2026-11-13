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
	"sync/atomic"
	"testing"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/multi-party-sig/pkg/party"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

func TestDepositScannerMissingMint(t *testing.T) {
	testDepositScannerMint(t, solana.NewWallet().PublicKey(), 6, true, false)
}

func TestDepositScannerWSOL(t *testing.T) {
	testDepositScannerMint(t, solana.MPK(solanaApp.WrappedSolanaAddress), solanaApp.SolanaDecimal, false, false)
}

func TestDepositScannerNonceExhaustion(t *testing.T) {
	testDepositScannerMint(t, solana.MPK(solanaApp.WrappedSolanaAddress), solanaApp.SolanaDecimal, false, true)
}

func testDepositScannerMint(t *testing.T, mint solana.PublicKey, decimals uint8, missing, waitForNonce bool) {
	t.Helper()
	ctx := common.EnableTestEnvironment(context.Background())
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "scanner.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	payer, sender := solana.NewWallet(), solana.NewWallet()
	user, group := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	require.NoError(t, st.TestWriteKey(ctx, "group-key", hex.EncodeToString(group.Bytes()), nil, true))
	req := &store.Request{
		Id: "scanner-user", Role: RequestRoleUser, Action: OperationTypeAddUser,
		State: common.RequestStateInitial, CreatedAt: time.Now().UTC(), Output: &mtg.Action{},
	}
	req.Output.OutputId = req.Id
	require.NoError(t, st.WriteRequestIfNotExist(ctx, req))
	require.NoError(t, st.WriteUserWithRequest(ctx, req, store.StartUserId.String(), "scanner-mix", user.String(), hex.EncodeToString(group.Bytes())))

	source := solanaApp.FindAssociatedTokenAddress(sender.PublicKey(), mint, solana.TokenProgramID)
	destination := solanaApp.FindAssociatedTokenAddress(user, mint, solana.TokenProgramID)
	tx, err := solana.NewTransaction([]solana.Instruction{
		token.NewTransferCheckedInstruction(1_000_000, decimals, source, mint, destination, sender.PublicKey(), nil).Build(),
	}, solana.Hash{}, solana.TransactionPayer(sender.PublicKey()))
	require.NoError(t, err)
	_, err = tx.Sign(solanaApp.BuildSignersGetter(sender.PrivateKey))
	require.NoError(t, err)
	balance := func(index uint16, owner solana.PublicKey, amount string) rpc.TokenBalance {
		return rpc.TokenBalance{AccountIndex: index, Owner: &owner, Mint: mint,
			UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: decimals}}
	}
	accounts := tx.Message.Instructions[0].Accounts
	meta := &rpc.TransactionMeta{
		PreTokenBalances: []rpc.TokenBalance{
			balance(accounts[0], sender.PublicKey(), "1000000"), balance(accounts[2], user, "0"),
		},
		PostTokenBalances: []rpc.TokenBalance{
			balance(accounts[0], sender.PublicKey(), "0"), balance(accounts[2], user, "1000000"),
		},
	}
	var mintData bytes.Buffer
	require.NoError(t, bin.NewBinEncoder(&mintData).Encode(token.Mint{Decimals: decimals, IsInitialized: true}))
	var emptyBlockReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var result any
		switch request.Method {
		case "getBlock":
			var height int64
			if len(request.Params) < 1 || json.Unmarshal(request.Params[0], &height) != nil {
				t.Error("missing block height")
				http.Error(w, "missing block height", http.StatusBadRequest)
				return
			}
			transactions := []any{map[string]any{
				"transaction": []string{tx.MustToBase64(), "base64"}, "meta": meta, "version": "legacy",
			}}
			if height == 101 {
				emptyBlockReads.Add(1)
				transactions = nil
			}
			result = map[string]any{"transactions": transactions}
		case "getAccountInfo":
			var value any
			if !missing {
				var address string
				var opts struct {
					Encoding string `json:"encoding"`
				}
				if len(request.Params) != 2 || json.Unmarshal(request.Params[0], &address) != nil || json.Unmarshal(request.Params[1], &opts) != nil || address != mint.String() {
					t.Error("unexpected account lookup")
					http.Error(w, "unexpected account lookup", http.StatusBadRequest)
					return
				}
				var data any = []string{base64.StdEncoding.EncodeToString(mintData.Bytes()), "base64"}
				if opts.Encoding == "jsonParsed" {
					data = map[string]any{"program": "spl-token", "space": solanaApp.MintSize,
						"parsed": map[string]any{"type": "mint", "info": map[string]any{
							"supply": "0", "decimals": decimals, "isInitialized": true,
							"mintAuthority": nil, "freezeAuthority": nil,
						}}}
				}
				value = map[string]any{"owner": solana.TokenProgramID.String(), "data": data, "lamports": 1_461_600, "executable": false, "rentEpoch": 0}
			}
			result = map[string]any{"context": map[string]any{"slot": 100}, "value": value}
		default:
			t.Errorf("unexpected RPC method: %s", request.Method)
			http.Error(w, "unexpected RPC method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	node := &Node{store: st, solana: solanaApp.NewClient(server.URL), conf: &Configuration{
		SolanaKey: payer.PrivateKey.String(), SolanaDepositEntry: solana.NewWallet().PublicKey().String(),
	}}
	if !missing {
		const member = "10000000-0000-4000-8000-000000000001"
		node.id = party.ID(member)
		network := newTestNetwork(party.IDSlice{node.id})
		node.network = network
		node.conf.AppId = member
		node.conf.MTG = &mtg.Configuration{}
		node.conf.MTG.Genesis.Members = []string{member}
		node.conf.MTG.Genesis.Threshold = 1
		node.conf.MTG.App.SpendPrivateKey = crypto.NewKeyFromSeed(make([]byte, 64)).String()
		if waitForNonce {
			require.NoError(t, node.writeBlockCheckpoint(ctx, 99))
			for range 2 {
				err := node.solanaReadBlocks(ctx, 99, 101, 2_039_280)
				require.ErrorIs(t, err, store.ErrNoSpareNonceAccount)
				checkpoint, err := st.ReadProperty(ctx, store.SolanaScanHeightKey)
				require.NoError(t, err)
				require.Equal(t, "99", checkpoint)
				cached, err := st.ReadCache(ctx, "block:100")
				require.NoError(t, err)
				require.Empty(t, cached)
				require.Empty(t, network.mtgChannel(node.id))
			}
			require.EqualValues(t, 1, emptyBlockReads.Load(), "completed blocks should stay cached while waiting")
		}
		nonce := solana.NewWallet().PublicKey()
		require.NoError(t, st.WriteNonceAccount(ctx, nonce.String(), solana.Hash{}.String()))

		require.NoError(t, node.solanaReadBlocks(ctx, 99, 101, 2_039_280))
		checkpoint, err := st.ReadProperty(ctx, store.SolanaScanHeightKey)
		require.NoError(t, err)
		require.Equal(t, "101", checkpoint)
		require.EqualValues(t, 1, emptyBlockReads.Load())
		cached, err := st.ReadCache(ctx, "block:100")
		require.NoError(t, err)
		require.Equal(t, "processed", cached)
		var out mtg.Action
		select {
		case data := <-network.mtgChannel(node.id):
			require.NoError(t, json.Unmarshal(data, &out))
		default:
			t.Fatal("scanner did not propose a deposit call")
		}
		_, extra := mtg.DecodeMixinExtraHEX(out.Extra)
		require.Greater(t, len(extra), 64+1+32+64+16)
		op := decodeOperation(extra[64:]) // Observer signature precedes the operation.
		require.EqualValues(t, OperationTypeDeposit, op.Type)
		require.Equal(t, user.Bytes(), op.Extra[:32])
		require.Equal(t, tx.Signatures[0][:], op.Extra[32:96])
		sweep, err := solana.TransactionFromBytes(op.Extra[112:]) // User, signature, call UUID, transaction.
		require.NoError(t, err)
		var transfers []*token.TransferChecked
		for _, ix := range sweep.Message.Instructions {
			program, err := sweep.Message.Program(ix.ProgramIDIndex)
			require.NoError(t, err)
			if program != solana.TokenProgramID {
				continue
			}
			accounts, err := ix.ResolveInstructionAccounts(&sweep.Message)
			require.NoError(t, err)
			transfer, ok := solanaApp.DecodeTokenTransferChecked(accounts, ix.Data)
			require.True(t, ok)
			transfers = append(transfers, transfer)
		}
		require.Len(t, transfers, 1)
		transfer := transfers[0]
		require.EqualValues(t, 1_000_000, *transfer.Amount)
		require.Equal(t, decimals, *transfer.Decimals)
		require.Equal(t, mint, transfer.GetMintAccount().PublicKey)
		require.Equal(t, destination, transfer.GetSourceAccount().PublicKey)
		require.Equal(t, solanaApp.FindAssociatedTokenAddress(node.SolanaDepositEntry(), mint, solana.TokenProgramID), transfer.GetDestinationAccount().PublicKey)
		require.Equal(t, user, transfer.GetOwnerAccount().PublicKey)
		reserved, err := st.ReadNonceAccount(ctx, nonce.String())
		require.NoError(t, err)
		require.True(t, reserved.CallId.Valid)
		// Re-reading a processed block must not create a second deposit call.
		require.NoError(t, node.solanaReadBlock(ctx, 100, 2_039_280))
		require.Empty(t, network.mtgChannel(node.id))

		// An unfinished block can be retried after one of its deposits has
		// completed and released its nonce. Do not allocate or propose it again.
		require.NoError(t, st.TestWriteCall(ctx, &store.SystemCall{
			RequestId: reserved.CallId.String, Type: store.CallTypeDeposit,
			NonceAccount: nonce.String(), Raw: sweep.MustToBase64(),
			State: common.RequestStateDone, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}))
		require.NoError(t, st.UpdateNonceAccount(ctx, nonce.String(), solana.Hash{1}.String(), reserved.CallId.String))
		require.NoError(t, node.solanaProcessTransaction(ctx, tx, meta, 2_039_280))
		spare, err := st.CountSpareNonceAccounts(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, spare)
		require.Empty(t, network.mtgChannel(node.id))
		return
	}
	// Missing mint data must fail the block on every attempt, without caching
	// the absence or marking the deposit's block as successfully processed.
	for range 2 {
		err := node.solanaReadBlock(ctx, 100, 2_039_280)
		require.ErrorIs(t, err, rpc.ErrNotFound)
		require.ErrorContains(t, err, mint.String())
		require.ErrorContains(t, err, tx.Signatures[0].String())
		for _, key := range []string{"block:100", "getAsset:" + mint.String()} {
			cached, err := st.ReadCache(ctx, key)
			require.NoError(t, err)
			require.Empty(t, cached)
		}
	}
}
