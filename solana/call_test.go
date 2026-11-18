package solana

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/multi-party-sig/pkg/math/curve"
	"github.com/MixinNetwork/multi-party-sig/protocols/frost"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestMergeDepositBurnTransfers(t *testing.T) {
	mint := solana.PublicKey{1}
	first := &solanaApp.TokenTransfer{Mint: mint, Amount: 10}
	native := &solanaApp.TokenTransfer{Mint: solana.PublicKey{2}, Amount: 5, SolanaAsset: true}
	other := &solanaApp.TokenTransfer{Mint: solana.PublicKey{3}, Amount: 15}
	second := &solanaApp.TokenTransfer{Mint: mint, Amount: 20}

	merged, err := mergeDepositBurnTransfers([]*solanaApp.TokenTransfer{first, native, other, second, native})
	require.NoError(t, err)
	require.Equal(t, []*solanaApp.TokenTransfer{{Mint: mint, Amount: 30}, native, other, native}, merged)
	require.Equal(t, uint64(10), first.Amount)
	_, err = mergeDepositBurnTransfers([]*solanaApp.TokenTransfer{{Mint: mint, Amount: math.MaxUint64}, second})
	require.ErrorContains(t, err, "overflows")
}

func TestBurnSettlementConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		amounts     []uint64
		post        string
		settled     bool
		postProcess bool
	}{
		{name: "single burn", amounts: []uint64{30}, post: "70", settled: true},
		{name: "multiple burns of one mint", amounts: []uint64{10, 20}, post: "70", settled: true},
		{name: "post-process burn", amounts: []uint64{30}, post: "70", settled: true, postProcess: true},
		{name: "different decrease", amounts: []uint64{30}, post: "80"},
		{name: "unchanged balance", amounts: []uint64{30}, post: "100"},
		{name: "balance increase", amounts: []uint64{30}, post: "130"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := common.EnableTestEnvironment(t.Context())
			node, db := testBuildNode(ctx, require.New(t), t.TempDir(), 0)
			t.Cleanup(func() {
				require.NoError(t, node.store.Close())
				require.NoError(t, db.Close())
			})

			requestSequence := node.conf.MTG.Genesis.Epoch
			newRequest := func(action byte, extra []byte) *store.Request {
				out := testBuildObserverRequest(node, uuid.Must(uuid.NewV4()).String(), action, extra)
				requestSequence += 10
				out.Sequence = requestSequence
				out.TestAttachActionToGroup(node.group)
				req, err := node.parseObserverRequest(out)
				require.NoError(t, err)
				require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
				return req
			}

			const decimals = solanaApp.AssetDecimal
			mint, user, account := solana.PublicKey{1}, solana.PublicKey{2}, solana.PublicKey{3}
			asset := &solanaApp.DeployedAsset{AssetId: mtg.StorageAssetId, ChainId: mtg.StorageAssetId, Address: mint.String(), Decimals: decimals}
			deploy := newRequest(OperationTypeDeployExternalAssets, nil)
			require.NoError(t, node.store.WriteDeployedAssetsWithRequest(ctx, deploy, []*solanaApp.DeployedAsset{asset}))
			register := newRequest(OperationTypeAddUser, nil)
			mix := bot.NewUUIDMixAddress([]string{uuid.Must(uuid.NewV4()).String()}, 1)
			require.NoError(t, node.store.WriteUserWithRequest(ctx, register, "1", mix.String(), user.String(), ""))

			// Use an offline transaction fixture with an inert signature and cached RPC metadata.
			builder := solana.NewTransactionBuilder().SetFeePayer(user)
			for _, amount := range tc.amounts {
				builder.AddInstruction(token.NewBurnCheckedInstruction(amount, decimals, account, mint, user, nil).Build())
			}
			tx, err := builder.Build()
			require.NoError(t, err)
			signature := solana.Signature{1}
			tx.Signatures = []solana.Signature{signature}
			msg, err := tx.Message.MarshalBinary()
			require.NoError(t, err)
			tokenBalance := func(amount string) rpc.TokenBalance {
				return rpc.TokenBalance{Owner: &user, Mint: mint, UiTokenAmount: &rpc.UiTokenAmount{Amount: amount, Decimals: decimals}}
			}
			rpcResult, err := json.Marshal(map[string]any{
				"transaction": []string{tx.MustToBase64(), "base64"},
				"meta": &rpc.TransactionMeta{
					PreBalances:       make([]uint64, len(tx.Message.AccountKeys)),
					PostBalances:      make([]uint64, len(tx.Message.AccountKeys)),
					PreTokenBalances:  []rpc.TokenBalance{tokenBalance("100")},
					PostTokenBalances: []rpc.TokenBalance{tokenBalance(tc.post)},
				},
			})
			require.NoError(t, err)
			require.NoError(t, node.store.WriteCache(ctx, "getTransaction:"+signature.String(), hex.EncodeToString(rpcResult)))

			deposit := newRequest(OperationTypeDeposit, append(user.Bytes(), signature[:]...))
			call := &store.SystemCall{
				RequestId: deposit.Id, Superior: deposit.Id, RequestHash: deposit.MixinHash.String(),
				Type: store.CallTypeDeposit, State: common.RequestStatePending,
				Public: "00000000000000000000000000000001", MessageHash: crypto.Sha256Hash(msg).String(), Raw: tx.MustToBase64(),
				CreatedAt: deposit.CreatedAt,
			}
			if tc.postProcess {
				main := newRequest(OperationTypeSystemCall, nil)
				mainCall := &store.SystemCall{
					RequestId: main.Id, Superior: main.Id, RequestHash: main.MixinHash.String(),
					Type: store.CallTypeMain, State: common.RequestStateDone, Public: call.Public,
					MessageHash: main.MixinHash.String(), CreatedAt: main.CreatedAt,
				}
				require.NoError(t, node.store.WriteInitialSystemCallWithRequest(ctx, main, mainCall, nil))
				call.Superior = main.Id
				call.Type = store.CallTypePostProcess
			}
			require.NoError(t, node.store.WriteInitialSystemCallWithRequest(ctx, deposit, call, nil))
			_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{db}, node.conf.AppId, asset.AssetId, "", "", deposit.Sequence-1, decimal.New(20, -decimals))
			require.NoError(t, err)
			require.False(t, node.checkSufficientBalanceForBurnSystemCall(ctx, call))
			_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{db}, node.conf.AppId, asset.AssetId, "", "", deposit.Sequence, decimal.NewFromInt(1))
			require.NoError(t, err)
			require.True(t, node.checkSufficientBalanceForBurnSystemCall(ctx, call))

			extra := encodeConfirmCallRecords([]confirmCallRecord{{Status: FlagConfirmCallSuccess, CallId: call.RequestId, Signature: signature}})
			confirm := newRequest(OperationTypeConfirmCall, extra)
			op := &common.Operation{Id: confirm.Id, Type: OperationTypeConfirmCall, Extra: extra}
			require.NoError(t, node.store.WritePendingBurnSystemCallIfNotExists(ctx, call, op, []string{asset.AssetId}))
			payments, compaction := node.processConfirmCall(ctx, confirm)
			require.Empty(t, compaction)
			stored, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
			require.NoError(t, err)
			require.NoError(t, node.confirmBurnRelatedSystemCallToGroup(ctx, op, stored))
			pending, _, err := node.store.ListPendingBurnSystemCalls(ctx)
			require.NoError(t, err)
			if tc.settled {
				require.Empty(t, pending)
				require.Len(t, payments, 1)
				require.True(t, decimal.RequireFromString(payments[0].Amount).Equal(decimal.New(30, -decimals)))
				require.EqualValues(t, common.RequestStateDone, stored.State)
				require.Equal(t, []string{payments[0].TraceId}, stored.GetRefundIds())
				replayed, compaction := node.ProcessOutput(ctx, confirm.Output)
				require.Empty(t, compaction)
				require.Len(t, replayed, 1)
				require.Equal(t, payments[0].TraceId, replayed[0].TraceId)
			} else {
				require.Len(t, pending, 1)
				require.Equal(t, common.UniqueId(confirm.Id, "RETRY"), pending[0].RequestId)
				require.Empty(t, payments)
				require.EqualValues(t, common.RequestStatePending, stored.State)
				require.Empty(t, stored.GetRefundIds())
				failed, err := node.store.ReadRequest(ctx, confirm.Id)
				require.NoError(t, err)
				require.EqualValues(t, common.RequestStateFailed, failed.State)
			}
			payments, compaction = node.processConfirmCall(ctx, newRequest(OperationTypeConfirmCall, extra))
			require.Empty(t, payments)
			require.Empty(t, compaction)
		})
	}
}

func TestConfirmUnconfirmedDeposits(t *testing.T) {
	ctx := common.EnableTestEnvironment(t.Context())
	node, db := testBuildNode(ctx, require.New(t), t.TempDir(), 0)
	t.Cleanup(func() {
		require.NoError(t, node.store.Close())
		require.NoError(t, db.Close())
	})

	requestSequence := node.conf.MTG.Genesis.Epoch
	newRequest := func(action byte, extra []byte) *store.Request {
		out := testBuildObserverRequest(node, uuid.Must(uuid.NewV4()).String(), action, extra)
		requestSequence += 10
		out.Sequence = requestSequence
		out.TestAttachActionToGroup(node.group)
		req, err := node.parseObserverRequest(out)
		require.NoError(t, err)
		require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
		return req
	}

	user := solana.PublicKey{2}
	register := newRequest(OperationTypeAddUser, nil)
	mix := bot.NewUUIDMixAddress([]string{uuid.Must(uuid.NewV4()).String()}, 1)
	require.NoError(t, node.store.WriteUserWithRequest(ctx, register, "1", mix.String(), user.String(), ""))

	// Exercise settlement with cached confirmation metadata and already verified
	// deposit records. No Solana transactions are signed or submitted.
	tx, err := solana.NewTransaction([]solana.Instruction{
		system.NewTransferInstruction(1, user, node.SolanaDepositEntry()).Build(),
	}, solana.Hash{}, solana.TransactionPayer(user))
	require.NoError(t, err)
	signature := solana.Signature{1}
	tx.Signatures = []solana.Signature{signature}
	msg, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	rpcResult, err := json.Marshal(map[string]any{
		"transaction": []string{tx.MustToBase64(), "base64"},
		"meta": &rpc.TransactionMeta{
			PreBalances:  make([]uint64, len(tx.Message.AccountKeys)),
			PostBalances: make([]uint64, len(tx.Message.AccountKeys)),
		},
	})
	require.NoError(t, err)
	require.NoError(t, node.store.WriteCache(ctx, "getTransaction:"+signature.String(), hex.EncodeToString(rpcResult)))

	deposit := newRequest(OperationTypeDeposit, append(user.Bytes(), signature[:]...))
	call := &store.SystemCall{
		RequestId: deposit.Id, Superior: deposit.Id, RequestHash: deposit.MixinHash.String(),
		Type: store.CallTypeDeposit, State: common.RequestStatePending,
		Public: "00000000000000000000000000000001", MessageHash: crypto.Sha256Hash(msg).String(), Raw: tx.MustToBase64(),
		CreatedAt: deposit.CreatedAt,
	}
	require.NoError(t, node.store.WriteInitialSystemCallWithRequest(ctx, deposit, call, nil))

	firstAsset, secondAsset := mtg.StorageAssetId, solanaApp.SolanaChainBase
	for _, item := range []struct {
		asset  string
		amount int64
		index  int64
	}{{firstAsset, 4, 9}, {secondAsset, 2, 8}, {firstAsset, 3, 4}} {
		outputID := uuid.Must(uuid.NewV4()).String()
		out := &mtg.Action{UnifiedOutput: mtg.UnifiedOutput{
			OutputId: outputID, TransactionHash: crypto.Sha256Hash([]byte(outputID)).String(),
			AssetId: item.asset, Amount: decimal.NewFromInt(item.amount),
			DepositHash:  sql.NullString{String: signature.String(), Valid: true},
			DepositIndex: sql.NullInt64{Int64: item.index, Valid: true},
			Sequence:     deposit.Sequence + uint64(item.index), SequencerCreatedAt: time.Now().UTC(),
		}}
		require.NoError(t, node.store.FailDepositRequestIfNotExist(ctx, out, "", true))
	}

	// Use just one eligible input for the first asset so both deposits must be
	// funded together. Fund the second asset only after the first attempt.
	_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{db}, node.conf.AppId, firstAsset, "", "", deposit.Sequence-1, decimal.NewFromInt(7))
	require.NoError(t, err)

	extra := encodeConfirmCallRecords([]confirmCallRecord{{Status: FlagConfirmCallSuccess, CallId: call.RequestId, Signature: signature}})
	confirm := newRequest(OperationTypeConfirmCall, extra)
	payments, compaction := node.processConfirmCall(ctx, confirm)
	require.Empty(t, payments)
	require.Equal(t, secondAsset, compaction)
	stored, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
	require.NoError(t, err)
	require.EqualValues(t, common.RequestStatePending, stored.State)
	pending, err := node.store.ListUnconfirmedDepositsByHash(ctx, signature.String())
	require.NoError(t, err)
	require.Len(t, pending, 3)

	_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{db}, node.conf.AppId, secondAsset, "", "", confirm.Sequence+1, decimal.NewFromInt(2))
	require.NoError(t, err)
	confirm.Sequence += 2
	confirm.Output.Sequence = confirm.Sequence
	confirm.Output.TestAttachActionToGroup(node.group)
	require.NoError(t, node.store.ResetRequest(ctx, confirm.Id, confirm.Sequence))
	payments, compaction = node.processConfirmCall(ctx, confirm)
	require.Empty(t, compaction)
	require.Len(t, payments, 2)
	require.Equal(t, firstAsset, payments[0].AssetId)
	require.Equal(t, "7", payments[0].Amount)
	require.Equal(t, secondAsset, payments[1].AssetId)
	require.Equal(t, "2", payments[1].Amount)
	firstTrace := common.UniqueId(common.UniqueId(signature.String(), "4"), node.SolanaDepositEntry().String())
	require.Equal(t, common.UniqueId(node.group.GenesisId(), firstTrace), payments[0].TraceId)

	stored, err = node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
	require.NoError(t, err)
	require.EqualValues(t, common.RequestStateDone, stored.State)
	require.Equal(t, []string{payments[0].TraceId, payments[1].TraceId}, stored.GetRefundIds())
	pending, err = node.store.ListUnconfirmedDepositsByHash(ctx, signature.String())
	require.NoError(t, err)
	require.Empty(t, pending)
	replayed, compaction := node.ProcessOutput(ctx, confirm.Output)
	require.Empty(t, compaction)
	require.Len(t, replayed, 2)
	require.Equal(t, payments[0].TraceId, replayed[0].TraceId)
	require.Equal(t, payments[1].TraceId, replayed[1].TraceId)
	payments, compaction = node.processConfirmCall(ctx, newRequest(OperationTypeConfirmCall, extra))
	require.Empty(t, payments)
	require.Empty(t, compaction)
}

func TestUserSystemCallFeePayer(t *testing.T) {
	ctx := context.Background()
	payer := solana.NewWallet()
	user, nonce, recipient := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	node := &Node{conf: &Configuration{SolanaKey: payer.PrivateKey.String()}}
	for _, test := range []struct {
		name     string
		feePayer solana.PublicKey
		wantErr  string
	}{
		{name: "node pays fees", feePayer: payer.PublicKey()},
		{name: "user pays fees despite node signing", feePayer: user, wantErr: "invalid system call fee payer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := solana.NewTransaction([]solana.Instruction{
				system.NewAdvanceNonceAccountInstruction(nonce, solana.SysVarRecentBlockHashesPubkey, payer.PublicKey()).Build(),
				system.NewTransferInstruction(1, user, recipient).Build(),
			}, solana.Hash{}, solana.TransactionPayer(test.feePayer))
			require.NoError(t, err)
			// Both transactions satisfy the previous signer-only check.
			require.True(t, tx.IsSigner(payer.PublicKey()))
			err = node.checkUserSystemCall(ctx, tx)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
	require.ErrorContains(t, node.checkUserSystemCall(ctx, new(solana.Transaction)), "invalid system call fee payer")
}

func TestSOLCleanupPolicy(t *testing.T) {
	require.True(t, isSOLCleanupDust(solanaApp.SolanaChainBase, decimal.NewFromInt(99_999)))
	require.False(t, isSOLCleanupDust(solanaApp.SolanaChainBase, decimal.NewFromInt(100_000)))
	require.False(t, isSOLCleanupDust("other-asset", decimal.NewFromInt(1)))
	for _, test := range []struct {
		name    string
		balance uint64
		amount  uint64
		unsafe  bool
		wantErr bool
	}{
		{name: "leave exactly rent", balance: 950_240, amount: 300_000},
		{name: "leave below rent", balance: 950_239, amount: 300_000, unsafe: true},
		{name: "close account", balance: 300_000, amount: 300_000},
		{name: "overdraw", balance: 300_000, amount: 300_001, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			unsafe, err := isSOLCleanupRentUnsafe(test.balance, test.amount)
			require.Equal(t, test.unsafe, unsafe)
			require.Equal(t, test.wantErr, err != nil)
		})
	}
}

func TestCompareSOLCleanupMovements(t *testing.T) {
	for _, test := range []struct {
		name     string
		balance  uint64
		expected uint64
		actual   uint64
		present  bool
		wantErr  string
	}{
		{name: "omit below dust", balance: 1_000_000, expected: 99_999},
		{name: "require exact dust", balance: 1_000_000, expected: 100_000, wantErr: "missing transfer"},
		{name: "accept exact dust", balance: 1_000_000, expected: 100_000, actual: 100_000, present: true},
		{name: "require below former rent threshold", balance: 1_000_000, expected: 300_000, wantErr: "missing transfer"},
		{name: "accept below former rent threshold", balance: 1_000_000, expected: 300_000, actual: 300_000, present: true},
		{name: "omit rent unsafe transfer", balance: 1_000_000, expected: 800_000},
		{name: "reject rent unsafe transfer", balance: 1_000_000, expected: 800_000, actual: 800_000, present: true, wantErr: "insufficient rent"},
		{name: "accept zero remainder", balance: 1_000_000, expected: 1_000_000, actual: 1_000_000, present: true},
		{name: "require zero remainder transfer", balance: 1_000_000, expected: 1_000_000, wantErr: "missing transfer"},
		{name: "accept rent boundary", balance: 950_240, expected: 300_000, actual: 300_000, present: true},
		{name: "reject wrong amount below dust", balance: 1_000_000, expected: 50_000, actual: 50_001, present: true, wantErr: "invalid transfer"},
		{name: "reject rent unsafe below dust", balance: 650_240, expected: 50_000, actual: 50_000, present: true, wantErr: "insufficient rent"},
		{name: "reject zero expectation bypass", balance: 1_000_000, expected: 0, actual: 100_000, present: true, wantErr: "unexpected SOL cleanup"},
		{name: "reject overdrawn expectation", balance: 1_000_000, expected: 1_000_001, wantErr: "exceeds balance"},
	} {
		t.Run(test.name, func(t *testing.T) {
			expected := map[string]*ReferencedTxAsset{
				solanaApp.SolanaEmptyAddress: {
					Solana: true, AssetId: solanaApp.SolanaChainBase, Address: solanaApp.SolanaEmptyAddress,
					Decimal: solanaApp.SolanaDecimal, Amount: decimal.NewFromUint64(test.expected).Shift(-solanaApp.SolanaDecimal),
				},
			}
			actual := make(map[string]*solanaApp.InitialAssetMovement)
			if test.present {
				actual[solanaApp.SolanaEmptyAddress] = &solanaApp.InitialAssetMovement{
					Kind: solanaApp.InitialAssetMovementTransfer, TokenAddress: solanaApp.SolanaEmptyAddress,
					Value: new(big.Int).SetUint64(test.actual), Decimal: solanaApp.SolanaDecimal,
				}
			}
			err := new(Node).comparePostProcessMovements(context.Background(), solanaApp.InitialAssetMovementTransfer, expected, actual, &test.balance)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSOLCleanupOmissionStillRequiresToken(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "cleanup.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	mint := solana.NewWallet().PublicKey().String()
	// A non-NFT mint lookup with no external RPC dependency.
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+mint, hex.EncodeToString([]byte(`{"value":{"data":["","base64"]}}`))))
	node := &Node{store: st}
	expected := map[string]*ReferencedTxAsset{
		solanaApp.SolanaEmptyAddress: {AssetId: solanaApp.SolanaChainBase, Decimal: 9, Amount: decimal.RequireFromString("0.0008")},
		mint:                         {Decimal: 6, Amount: decimal.NewFromInt(1)},
	}
	balance := uint64(1_000_000)
	require.ErrorContains(t, node.comparePostProcessMovements(ctx, solanaApp.InitialAssetMovementTransfer, expected, nil, &balance), "missing transfer")
	actual := map[string]*solanaApp.InitialAssetMovement{
		mint: {Kind: solanaApp.InitialAssetMovementTransfer, TokenAddress: mint, Decimal: 6, Value: big.NewInt(1_000_000)},
	}
	require.NoError(t, node.comparePostProcessMovements(ctx, solanaApp.InitialAssetMovementTransfer, expected, actual, &balance))
}

func TestCleanupSOLBalanceSnapshot(t *testing.T) {
	ctx := context.Background()
	payer := solana.NewWallet().PublicKey()
	source := solana.NewWallet().PublicKey()
	tx, err := solana.NewTransaction([]solana.Instruction{
		system.NewTransferInstruction(1, source, payer).Build(),
	}, solana.Hash{}, solana.TransactionPayer(payer))
	require.NoError(t, err)
	accounts, err := tx.AccountMetaList()
	require.NoError(t, err)
	meta := &rpc.TransactionMeta{PostBalances: make([]uint64, len(accounts))}
	for index, account := range accounts {
		if account.PublicKey == source {
			meta.PostBalances[index] = 1_000_000
		}
	}
	node := new(Node)
	balance, err := node.cleanupSOLBalance(ctx, nil, tx, meta, source)
	require.NoError(t, err)
	require.NotNil(t, balance)
	require.EqualValues(t, 1_000_000, *balance)
	absent, err := node.cleanupSOLBalance(ctx, nil, tx, meta, solana.NewWallet().PublicKey())
	require.NoError(t, err)
	require.Nil(t, absent)
	zero, err := node.cleanupSOLBalance(ctx, nil, tx, &rpc.TransactionMeta{PostBalances: make([]uint64, len(accounts))}, source)
	require.NoError(t, err)
	require.NotNil(t, zero)
	require.Zero(t, *zero)
	_, err = node.cleanupSOLBalance(ctx, nil, tx, &rpc.TransactionMeta{}, source)
	require.ErrorContains(t, err, "missing cleanup source balance")
	_, err = node.cleanupSOLBalance(ctx, nil, nil, nil, source)
	require.ErrorContains(t, err, "missing confirmed cleanup transaction")

	// Failed cleanup/refund builders load the pinned confirmation from cache,
	// rather than querying the source's current account balance.
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "cleanup.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	node.store = st
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	result, err := json.Marshal(map[string]any{
		"meta": meta, "transaction": []string{base64.StdEncoding.EncodeToString(raw), "base64"},
	})
	require.NoError(t, err)
	require.NoError(t, st.WriteCache(ctx, "getTransaction:confirmed-attempt", hex.EncodeToString(result)))
	call := &store.SystemCall{Hash: sql.NullString{Valid: true, String: "confirmed-attempt"}}
	balance, err = node.cleanupSOLBalance(ctx, call, nil, nil, source)
	require.NoError(t, err)
	require.NotNil(t, balance)
	require.EqualValues(t, 1_000_000, *balance)
}

func TestSOLConfirmationFixture(t *testing.T) {
	ctx := context.Background()
	require := require.New(t)
	payer := solana.NewWallet()
	source := solana.NewWallet().PublicKey()
	var nodes []*Node
	for i := 0; i < 2; i++ {
		st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "confirmation.sqlite"))
		require.NoError(err)
		defer st.Close()
		nodes = append(nodes, &Node{store: st, conf: &Configuration{SolanaKey: payer.PrivateKey.String()}})
	}
	for index, signature := range []solana.Signature{
		solana.MustSignatureFromBase58("42fwVqHYmfLqoqQ3XgELu72FL6t2Q2HCqY7XzkVCdVsWHGoT6DHBk7qzoUkpfjqs42ygSSnFWzarQZdpUX9tLK6r"),
		solana.MustSignatureFromBase58("sKxPceKjZb4PiywqwMP3Wyf9YoPqdgZZ5MLNxkpRv7qgTXVxK8UQRGkjMT47qJht5muuUPpqynkrM5C6BcYSELg"),
	} {
		failed := index == 1
		call := &store.SystemCall{Hash: sql.NullString{Valid: true, String: signature.String()}}
		testCacheSOLConfirmation(ctx, require, nodes, signature, source, 1_000_000_000, failed)
		for _, node := range nodes {
			// No RPC client is configured: all nodes must use the same fixture.
			result, err := node.RPCGetTransaction(ctx, signature.String())
			require.NoError(err)
			require.Equal(failed, result.Meta.Err != nil)
			tx, err := result.Transaction.GetTransaction()
			require.NoError(err)
			require.Equal(signature, tx.Signatures[0])
			balance, err := node.cleanupSOLBalance(ctx, call, nil, nil, source)
			require.NoError(err)
			require.NotNil(balance)
			require.EqualValues(1_000_000_000, *balance)
			changes, err := buildUserBalanceChangesFromMeta(tx, result.Meta, source)
			require.NoError(err)
			require.True(changes[solanaApp.SolanaEmptyAddress].Amount.IsZero())
		}
	}
}

func TestConfirmMainRequiresPostProcess(t *testing.T) {
	for _, tc := range []struct {
		name        string
		income      uint64
		skip        bool
		lookup      bool
		requirePost bool
	}{
		{name: "no cleanup"},
		{name: "dust exemption", income: 99_999},
		{name: "rent exemption", income: 800_000},
		{name: "user opt-out", income: 300_000, skip: true},
		{name: "cleanup required", income: 300_000, requirePost: true},
		{name: "cleanup required with historical lookup", income: 300_000, lookup: true, requirePost: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := common.EnableTestEnvironment(t.Context())
			node, db := testBuildNode(ctx, require.New(t), t.TempDir(), 0)
			node.solana = nil
			t.Cleanup(func() {
				require.NoError(t, node.store.Close())
				require.NoError(t, db.Close())
			})
			requestSequence := node.conf.MTG.Genesis.Epoch
			newRequest := func(action byte, extra []byte) *store.Request {
				out := testBuildObserverRequest(node, uuid.Must(uuid.NewV4()).String(), action, extra)
				requestSequence += 10
				out.Sequence = requestSequence
				out.TestAttachActionToGroup(node.group)
				req, err := node.parseObserverRequest(out)
				require.NoError(t, err)
				require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
				return req
			}

			master, share, ok := strings.Cut(testFROSTKeys1[node.id], ";")
			require.True(t, ok)
			key := frost.EmptyConfig(curve.Edwards25519{})
			require.NoError(t, key.UnmarshalBinary(common.DecodeHexOrPanic(share)))
			// The legacy test share predates child key derivation.
			key.ChainKey = make([]byte, 32)
			encoded, err := key.MarshalBinary()
			require.NoError(t, err)
			require.NoError(t, node.store.TestWriteKey(ctx, common.UniqueId("prepare", master), master, encoded, false))
			user := &store.User{UserId: store.StartUserId.String(), Public: master}
			main := newRequest(OperationTypeSystemCall, nil)
			call := &store.SystemCall{
				RequestId: main.Id, Superior: main.Id, RequestHash: main.MixinHash.String(),
				Type: store.CallTypeMain, State: common.RequestStatePending, SkipPostProcess: tc.skip,
				Public: hex.EncodeToString(user.FingerprintWithPath()), CreatedAt: main.CreatedAt,
			}
			source := node.getUserSolanaPublicKeyFromCall(ctx, call)
			mix := bot.NewUUIDMixAddress([]string{uuid.Must(uuid.NewV4()).String()}, 1)
			register := newRequest(OperationTypeAddUser, nil)
			require.NoError(t, node.store.WriteUserWithRequest(ctx, register, user.UserId, mix.String(), source.String(), master))

			payer, entry := node.SolanaPayer(), solana.MPK(node.conf.SolanaDepositEntry)
			options := []solana.TransactionOption{solana.TransactionPayer(payer)}
			if tc.lookup {
				options = append(options, solana.TransactionAddressTables(map[solana.PublicKey]solana.PublicKeySlice{
					{3}: {source},
				}))
			}
			tx, err := solana.NewTransaction([]solana.Instruction{
				system.NewTransferInstruction(tc.income, payer, source).Build(),
			}, solana.Hash{}, options...)
			require.NoError(t, err)
			_, err = tx.PartialSign(solanaApp.BuildSignersGetter(solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)))
			require.NoError(t, err)
			signature := tx.Signatures[0]
			msg, err := tx.Message.MarshalBinary()
			require.NoError(t, err)
			call.MessageHash, call.Raw = crypto.Sha256Hash(msg).String(), tx.MustToBase64()
			require.NoError(t, node.store.WriteInitialSystemCallWithRequest(ctx, main, call, nil))
			count := len(tx.Message.AccountKeys) + tx.Message.NumLookups()
			meta := &rpc.TransactionMeta{PreBalances: make([]uint64, count), PostBalances: make([]uint64, count)}
			sourceIndex := 1
			if tc.lookup {
				// No live lookup table is supplied to the validator.
				require.Equal(t, 1, tx.Message.NumWritableLookups())
				sourceIndex = len(tx.Message.AccountKeys)
				meta.LoadedAddresses.Writable = solana.PublicKeySlice{source}
			} else {
				require.Equal(t, source, tx.Message.AccountKeys[sourceIndex])
			}
			meta.PreBalances[sourceIndex], meta.PostBalances[sourceIndex] = 1_000_000-tc.income, 1_000_000
			result, err := json.Marshal(map[string]any{"transaction": []string{tx.MustToBase64(), "base64"}, "meta": meta})
			require.NoError(t, err)
			require.NoError(t, node.store.WriteCache(ctx, "getTransaction:"+signature.String(), hex.EncodeToString(result)))

			confirm := func(extra []byte, state byte) *store.Request {
				req := newRequest(OperationTypeConfirmCall, extra)
				payments, compaction := node.processConfirmCall(ctx, req)
				require.Empty(t, payments)
				require.Empty(t, compaction)
				stored, err := node.store.ReadRequest(ctx, req.Id)
				require.NoError(t, err)
				require.EqualValues(t, state, stored.State)
				return req
			}
			extra := encodeConfirmCallRecords([]confirmCallRecord{{Status: FlagConfirmCallSuccess, CallId: call.RequestId, Signature: signature}})
			if !tc.requirePost {
				confirm(extra, common.RequestStateDone)
				stored, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
				require.NoError(t, err)
				require.EqualValues(t, common.RequestStateDone, stored.State)
				return
			}

			rejected := confirm(extra, common.RequestStateFailed)
			stored, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
			require.NoError(t, err)
			require.EqualValues(t, common.RequestStatePending, stored.State)
			require.False(t, stored.Hash.Valid)
			if tc.lookup {
				confirmed, _, err := node.handleSignedCall(ctx, stored)
				require.NoError(t, err)
				require.Equal(t, signature, confirmed.Signatures[0])
			}

			nonce := solana.PublicKey{8}
			post, err := solana.NewTransaction([]solana.Instruction{
				system.NewAdvanceNonceAccountInstruction(nonce, solana.SysVarRecentBlockHashesPubkey, payer).Build(),
				system.NewTransferInstruction(tc.income, source, entry).Build(),
			}, solana.Hash{}, solana.TransactionPayer(payer))
			require.NoError(t, err)
			post.Signatures = make([]solana.Signature, post.Message.Header.NumRequiredSignatures)
			raw, err := post.MarshalBinary()
			require.NoError(t, err)
			cid := common.UniqueId(common.UniqueId(call.RequestId, "confirm-success"), "post-process")
			corrected := attachSystemCall(extra, cid, raw)
			accepted := confirm(corrected, common.RequestStateDone)
			require.NotEqual(t, rejected.Id, accepted.Id)
			stored, err = node.store.ReadSystemCallByRequestId(ctx, call.RequestId, 0)
			require.NoError(t, err)
			require.EqualValues(t, common.RequestStateDone, stored.State)
			require.Equal(t, signature.String(), stored.Hash.String)

			confirm(corrected, common.RequestStateFailed)
			cleanup, err := node.store.ReadSystemCallByRequestId(ctx, cid, 0)
			require.NoError(t, err)
			require.EqualValues(t, common.RequestStatePending, cleanup.State)
			require.Equal(t, call.RequestId, cleanup.Superior)
			session, err := node.store.ReadSession(ctx, cid)
			require.NoError(t, err)
			require.Equal(t, accepted.MixinHash.String(), session.MixinHash)
		})
	}
}

// Exercise receiver filtering, ledger selection, instruction extraction and both
// comparison passes together. Account fixtures keep all cases independent of RPC.
func TestDepositCallMovements(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "deposit.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	node := &Node{store: st}
	payer := solana.NewWallet().PublicKey()
	user, other, entry := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	mint, deployed, unexpected, nft := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	for _, address := range []solana.PublicKey{mint, deployed, unexpected} {
		require.NoError(t, st.WriteCache(ctx, "getAccount:"+address.String(), hex.EncodeToString([]byte(`{"value":{"data":["","base64"]}}`))))
	}
	// Standard initialized NFT mint: supply 1 and decimals 0.
	nftData := make([]byte, 82)
	nftData[36], nftData[45] = 1, 1
	account, err := json.Marshal(map[string]any{"value": map[string]any{"data": []string{base64.StdEncoding.EncodeToString(nftData), "base64"}}})
	require.NoError(t, err)
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+nft.String(), hex.EncodeToString(account)))
	// Persist deployment through the store API, as production classification does.
	req := &store.Request{Id: "deposit-movement-fixture", Role: RequestRoleObserver, State: common.RequestStateInitial, CreatedAt: time.Now().UTC(), Output: &mtg.Action{}}
	req.Output.OutputId = req.Id
	require.NoError(t, st.WriteRequestIfNotExist(ctx, req))
	require.NoError(t, st.WriteDeployedAssetsWithRequest(ctx, req, []*solanaApp.DeployedAsset{{AssetId: "external-asset", ChainId: "external-chain", Address: deployed.String(), Decimals: 6}}))
	changes := func(receiver solana.PublicKey, address solana.PublicKey, amount int64) map[string]*big.Int {
		return map[string]*big.Int{receiver.String() + ":" + address.String(): big.NewInt(amount)}
	}
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		t.Run(program.String(), func(t *testing.T) {
			movement := func(burn bool, address, owner solana.PublicKey, amount uint64) solana.Instruction {
				ata := solanaApp.FindAssociatedTokenAddress(owner, address, program)
				decimals := uint8(6)
				if address == nft {
					decimals = 0
				}
				var ix solana.Instruction
				if burn {
					ix = token.NewBurnCheckedInstruction(amount, decimals, ata, address, owner, nil).Build()
				} else {
					ix = token.NewTransferCheckedInstruction(amount, decimals, ata, address, solanaApp.FindAssociatedTokenAddress(entry, address, program), owner, nil).Build()
				}
				data, err := ix.Data()
				require.NoError(t, err)
				return solana.NewInstruction(program, ix.Accounts(), data)
			}
			multiReceiver := changes(user, mint, 1_000_000)
			multiReceiver[other.String()+":"+mint.String()] = big.NewInt(7_000_000)
			// No account fixture for this address: filtering must happen before lookup.
			multiReceiver[other.String()+":"+solana.NewWallet().PublicKey().String()] = big.NewInt(1_000_000)
			mixed := changes(user, mint, 1_000_000)
			mixed[user.String()+":"+deployed.String()] = big.NewInt(2_000_000)
			for _, test := range []struct {
				name         string
				expected     map[string]*big.Int
				instructions []solana.Instruction
				wantErr      string
			}{
				{name: "multiple receivers", expected: multiReceiver, instructions: []solana.Instruction{movement(false, mint, user, 1_000_000)}},
				{name: "ignore another sender", expected: multiReceiver, instructions: []solana.Instruction{movement(false, mint, user, 1_000_000), movement(false, mint, other, 7_000_000)}},
				{name: "aggregate split transfers", expected: changes(user, mint, 1_000_000), instructions: []solana.Instruction{movement(false, mint, user, 400_000), movement(false, mint, user, 600_000)}},
				{name: "external asset burn", expected: changes(user, deployed, 1_000_000), instructions: []solana.Instruction{movement(true, deployed, user, 1_000_000)}},
				{name: "transfer and burn different assets", expected: mixed, instructions: []solana.Instruction{movement(false, mint, user, 1_000_000), movement(true, deployed, user, 2_000_000)}},
				{name: "missing transfer", expected: changes(user, mint, 1_000_000), wantErr: "non-existed transfer deposit"},
				{name: "missing burn", expected: changes(user, deployed, 1_000_000), wantErr: "non-existed burn deposit"},
				{name: "wrong transfer amount", expected: changes(user, mint, 1_000_000), instructions: []solana.Instruction{movement(false, mint, user, 999_999)}, wantErr: "invalid transfer deposit"},
				{name: "wrong burn amount", expected: changes(user, deployed, 1_000_000), instructions: []solana.Instruction{movement(true, deployed, user, 999_999)}, wantErr: "invalid burn deposit"},
				{name: "unexpected transfer", instructions: []solana.Instruction{movement(false, unexpected, user, 1_000_000)}, wantErr: "unexpected transfer deposit"},
				{name: "unexpected burn", instructions: []solana.Instruction{movement(true, unexpected, user, 1_000_000)}, wantErr: "unexpected burn deposit"},
				{name: "transfer cannot replace external burn", expected: changes(user, deployed, 1_000_000), instructions: []solana.Instruction{movement(false, deployed, user, 1_000_000)}, wantErr: "unexpected transfer deposit"},
				{name: "burn cannot replace native transfer", expected: changes(user, mint, 1_000_000), instructions: []solana.Instruction{movement(true, mint, user, 1_000_000)}, wantErr: "non-existed transfer deposit"},
				{name: "split burn and transfer cannot satisfy burn", expected: changes(user, deployed, 1_000_000), instructions: []solana.Instruction{movement(true, deployed, user, 600_000), movement(false, deployed, user, 400_000)}, wantErr: "unexpected transfer deposit"},
				{name: "eligible SOL requires transfer", expected: changes(user, solana.PublicKey{}, int64(solanaCleanupDustLamports)), wantErr: "non-existed transfer deposit"},
				{name: "eligible SOL can be transferred", expected: changes(user, solana.PublicKey{}, int64(solanaCleanupDustLamports)), instructions: []solana.Instruction{system.NewTransferInstruction(solanaCleanupDustLamports, user, entry).Build()}},
				{name: "partial SOL amount rejected", expected: changes(user, solana.PublicKey{}, int64(solanaCleanupDustLamports)), instructions: []solana.Instruction{system.NewTransferInstruction(solanaCleanupDustLamports-1, user, entry).Build()}, wantErr: "invalid transfer deposit"},
				{name: "unexpected SOL dust rejected", instructions: []solana.Instruction{system.NewTransferInstruction(9, user, entry).Build()}, wantErr: "unexpected transfer deposit"},
				{name: "omit NFT", expected: changes(user, nft, 1)},
				{name: "NFT transfer exempt", expected: changes(user, nft, 1), instructions: []solana.Instruction{movement(false, nft, user, 1)}},
				{name: "NFT burn exempt", expected: changes(user, nft, 1), instructions: []solana.Instruction{movement(true, nft, user, 1)}},
			} {
				t.Run(test.name, func(t *testing.T) {
					instructions := append([]solana.Instruction{solana.NewInstruction(solana.MemoProgramID, nil, []byte("deposit"))}, test.instructions...)
					tx, err := solana.NewTransaction(instructions, solana.Hash{}, solana.TransactionPayer(payer))
					require.NoError(t, err)
					tx.Signatures = make([]solana.Signature, tx.Message.Header.NumRequiredSignatures)
					expectedTransfers, expectedBurns := node.collectExpectedDepositMovements(ctx, test.expected, user.String())
					actualTransfers, actualBurns, err := buildInitialAssetMovementMaps(tx, user.String(), "")
					require.NoError(t, err)
					err = node.compareDepositMovements(ctx, "deposit-fixture", tx, solanaApp.InitialAssetMovementTransfer, expectedTransfers, actualTransfers)
					if err == nil {
						err = node.compareDepositMovements(ctx, "deposit-fixture", tx, solanaApp.InitialAssetMovementBurn, expectedBurns, actualBurns)
					}
					if test.wantErr != "" {
						require.ErrorContains(t, err, test.wantErr)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}

func TestPrepareCallMovements(t *testing.T) {
	payer := solana.NewWallet()
	mtg, user, other, mint, nonce := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	node := &Node{conf: &Configuration{SolanaKey: payer.PrivateKey.String()}}
	solAsset := &ReferencedTxAsset{Solana: true, AssetId: solanaApp.SolanaChainBase, ChainId: solanaApp.SolanaChainBase, Address: solanaApp.SolanaEmptyAddress, Decimal: 9, Amount: decimal.RequireFromString("0.001")}
	feeAsset := *solAsset
	feeAsset.Fee = true
	tokenAsset := &ReferencedTxAsset{Solana: true, AssetId: "native-token", ChainId: solanaApp.SolanaChainBase, Address: mint.String(), Decimal: 6, Amount: decimal.NewFromInt(1)}
	mintAsset := *tokenAsset
	mintAsset.Solana = false
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		t.Run(program.String(), func(t *testing.T) {
			wrap := func(ix solana.Instruction) solana.Instruction {
				data, err := ix.Data()
				require.NoError(t, err)
				return solana.NewInstruction(program, ix.Accounts(), data)
			}
			transfer := func(amount uint64, owner, receiver solana.PublicKey) solana.Instruction {
				return wrap(token.NewTransferCheckedInstruction(amount, 6, solanaApp.FindAssociatedTokenAddress(owner, mint, program), mint, solanaApp.FindAssociatedTokenAddress(receiver, mint, program), owner, nil).Build())
			}
			mintTo := func(amount uint64, receiver solana.PublicKey) solana.Instruction {
				return wrap(token.NewMintToInstruction(amount, mint, solanaApp.FindAssociatedTokenAddress(receiver, mint, program), mtg, nil).Build())
			}
			for _, test := range []struct {
				name          string
				assets        []*ReferencedTxAsset
				instructions  []solana.Instruction
				allowPayerFee bool
				wantErr       string
			}{
				{name: "SOL transfer", assets: []*ReferencedTxAsset{solAsset}, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, mtg, user).Build()}},
				{name: "token transfer", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{transfer(1_000_000, mtg, user)}},
				{name: "external mint", assets: []*ReferencedTxAsset{&mintAsset}, instructions: []solana.Instruction{mintTo(1_000_000, user)}},
				{name: "mixed SOL and token", assets: []*ReferencedTxAsset{solAsset, tokenAsset}, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, mtg, user).Build(), transfer(1_000_000, mtg, user)}},
				{name: "aggregate split transfers", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{transfer(400_000, mtg, user), transfer(600_000, mtg, user)}},
				{name: "aggregate references", assets: []*ReferencedTxAsset{tokenAsset, tokenAsset}, instructions: []solana.Instruction{transfer(2_000_000, mtg, user)}},
				{name: "wrong SOL recipient", assets: []*ReferencedTxAsset{solAsset}, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, mtg, other).Build()}, wantErr: "invalid prepare SOL recipient"},
				{name: "wrong SOL source", assets: []*ReferencedTxAsset{solAsset}, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, other, user).Build()}, wantErr: "unexpected prepare asset movement"},
				{name: "wrong token recipient", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{transfer(1_000_000, mtg, other)}, wantErr: "invalid prepare token recipient"},
				{name: "wrong token owner", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{transfer(1_000_000, other, user)}, wantErr: "unexpected prepare asset movement"},
				{name: "wrong mint recipient", assets: []*ReferencedTxAsset{&mintAsset}, instructions: []solana.Instruction{mintTo(1_000_000, other)}, wantErr: "invalid prepare mint recipient"},
				{name: "wrong amount", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{transfer(999_999, mtg, user)}, wantErr: "invalid prepare asset amount"},
				{name: "missing movement", assets: []*ReferencedTxAsset{tokenAsset}, wantErr: "missing prepare asset movement"},
				{name: "unexpected movement", instructions: []solana.Instruction{transfer(1_000_000, mtg, user)}, wantErr: "unexpected prepare asset movement"},
				{name: "transfer cannot replace mint", assets: []*ReferencedTxAsset{&mintAsset}, instructions: []solana.Instruction{transfer(1_000_000, mtg, user)}, wantErr: "unexpected prepare asset movement"},
				{name: "mint cannot replace transfer", assets: []*ReferencedTxAsset{tokenAsset}, instructions: []solana.Instruction{mintTo(1_000_000, user)}, wantErr: "unexpected prepare asset movement"},
				{name: "disallowed program", instructions: []solana.Instruction{solana.NewInstruction(other, nil, nil)}, wantErr: "program:"},
				{name: "known payer fee", assets: []*ReferencedTxAsset{&feeAsset}, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, payer.PublicKey(), user).Build()}},
				{name: "unknown payer fee allowed", allowPayerFee: true, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, payer.PublicKey(), user).Build()}},
				{name: "unknown payer fee rejected", instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, payer.PublicKey(), user).Build()}, wantErr: "unexpected prepare asset movement"},
				{name: "fee exception still checks recipient", allowPayerFee: true, instructions: []solana.Instruction{system.NewTransferInstruction(1_000_000, payer.PublicKey(), other).Build()}, wantErr: "invalid prepare SOL recipient"},
			} {
				t.Run(test.name, func(t *testing.T) {
					instructions := append([]solana.Instruction{system.NewAdvanceNonceAccountInstruction(nonce, solana.SysVarRecentBlockHashesPubkey, payer.PublicKey()).Build()}, test.instructions...)
					tx, err := solana.NewTransaction(instructions, solana.Hash{}, solana.TransactionPayer(payer.PublicKey()))
					require.NoError(t, err)
					err = node.comparePrepareCallWithSolanaTx(tx, test.assets, mtg, user, test.allowPayerFee)
					if test.wantErr != "" {
						require.ErrorContains(t, err, test.wantErr)
					} else {
						require.NoError(t, err)
					}
				})
			}
		})
	}
}

// Movement comparison does not check mint authorities; the envelope must reject
// an unrelated signing authority before a Prepare can be accepted.
func TestPrepareCallAuthority(t *testing.T) {
	payer := solana.NewWallet()
	mtg, user, other, mint, nonce := solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey(), solana.NewWallet().PublicKey()
	node := &Node{conf: &Configuration{SolanaKey: payer.PrivateKey.String()}}
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		for _, authority := range []solana.PublicKey{mtg, other} {
			t.Run(program.String()+"/"+authority.String(), func(t *testing.T) {
				ix := token.NewMintToInstruction(1_000_000, mint, solanaApp.FindAssociatedTokenAddress(user, mint, program), authority, nil).Build()
				data, err := ix.Data()
				require.NoError(t, err)
				tx, err := solana.NewTransaction([]solana.Instruction{system.NewAdvanceNonceAccountInstruction(nonce, solana.SysVarRecentBlockHashesPubkey, payer.PublicKey()).Build(), solana.NewInstruction(program, ix.Accounts(), data)}, solana.Hash{}, solana.TransactionPayer(payer.PublicKey()))
				require.NoError(t, err)
				err = node.VerifySubSystemCallEnvelope(tx, mtg, false)
				if authority == other {
					require.ErrorContains(t, err, "invalid subsystem signers")
					return
				}
				require.NoError(t, err)
				require.NoError(t, node.VerifySubSystemCall(context.Background(), tx, other, user))
				require.NoError(t, node.comparePrepareCallWithSolanaTx(tx, []*ReferencedTxAsset{{Address: mint.String(), Decimal: 6, Amount: decimal.NewFromInt(1)}}, mtg, user, false))
			})
		}
	}
}

func TestSystemCallAssetPrecision(t *testing.T) {
	for _, test := range []struct {
		name, chain string
		precision   int
		amounts     []string
		wantErr     bool
	}{
		{name: "exact token unit", chain: solanaApp.SolanaChainBase, precision: 6, amounts: []string{"0.000001"}},
		{name: "truncates to zero", chain: solanaApp.SolanaChainBase, precision: 6, amounts: []string{"0.00000001"}, wantErr: true},
		{name: "positive amount loses remainder", chain: solanaApp.SolanaChainBase, precision: 6, amounts: []string{"1.00000001"}, wantErr: true},
		{name: "aggregate before checking", chain: solanaApp.SolanaChainBase, precision: 6, amounts: []string{"0.00000050", "0.00000050"}},
		{name: "aggregate still loses remainder", chain: solanaApp.SolanaChainBase, precision: 6, amounts: []string{"0.00000050", "0.00000049"}, wantErr: true},
		{name: "SOL nine decimals", chain: solanaApp.SolanaChainBase, precision: 9, amounts: []string{"0.00000001"}},
		{name: "whole unit mint rejects fraction", chain: solanaApp.SolanaChainBase, precision: 0, amounts: []string{"1.99"}, wantErr: true},
		{name: "deployed uses eight decimals", chain: common.SafeEthereumChainId, precision: 18, amounts: []string{"0.00000001"}},
		{name: "deployed ignores original lower precision", chain: common.SafeLitecoinChainId, precision: 2, amounts: []string{"1.12345678"}},
		{name: "deployed rejects over eight decimals", chain: common.SafeEthereumChainId, precision: 18, amounts: []string{"1.000000001"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var outputs []*store.UserOutput
			for _, amount := range test.amounts {
				outputs = append(outputs, &store.UserOutput{AssetId: "asset", ChainId: test.chain, Amount: amount, Asset: bot.AssetNetwork{ChainID: test.chain, Precision: test.precision}})
			}
			err := validateSystemCallAssetAmounts(outputs)
			if test.wantErr {
				require.ErrorContains(t, err, "unrepresentable system call asset amount")
			} else {
				require.NoError(t, err)
			}
			for i, output := range outputs {
				require.Equal(t, test.amounts[i], output.Amount)
			}
		})
	}
}

func TestSystemCallPrecisionRefundsAllDeposits(t *testing.T) {
	for _, test := range []struct {
		name    string
		amounts [2]string
		refund  string
	}{
		{name: "zero units", amounts: [2]string{"0.00000001", "0.00000002"}, refund: "0.00000003"},
		{name: "nonzero amount loses remainder", amounts: [2]string{"0.5", "0.50000001"}, refund: "1.00000001"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := common.EnableTestEnvironment(context.Background())
			node, md := testBuildNode(ctx, require.New(t), t.TempDir(), 0)
			defer node.store.Close()
			defer md.Close()
			mix := bot.NewUUIDMixAddress([]string{"a7376114-5db3-4822-bd3c-26416b57da1b"}, 1)
			userID := store.StartUserId.String()
			invalidID, validID := "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"
			deposit := func(assetID, amount string, precision int) *store.UserOutput {
				id := uuid.Must(uuid.NewV4()).String()
				hash := crypto.Sha256Hash([]byte(id))
				req := &store.Request{Id: id, MixinHash: hash, AssetId: assetID, Amount: decimal.RequireFromString(amount), Role: RequestRoleUser, Action: OperationTypeUserDeposit, State: common.RequestStateInitial, CreatedAt: time.Now().UTC(), Output: &mtg.Action{}}
				req.Output.OutputId = id
				require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
				output := &store.UserOutput{OutputId: id, UserId: userID, TransactionHash: hash.String(), AssetId: assetID, ChainId: solanaApp.SolanaChainBase, Amount: amount, State: common.RequestStateInitial, CreatedAt: req.CreatedAt, UpdatedAt: req.CreatedAt, Asset: bot.AssetNetwork{ChainID: solanaApp.SolanaChainBase, Precision: precision}}
				require.NoError(t, node.store.WriteUserDepositWithRequest(ctx, req, output))
				return output
			}
			outputs := []*store.UserOutput{deposit(invalidID, test.amounts[0], 6), deposit(invalidID, test.amounts[1], 6), deposit(validID, "1", 9)}
			unreferenced := deposit(invalidID, "0.00000004", 6)
			readOutput := func(output *store.UserOutput) *store.UserOutput {
				os, err := node.store.ListUserOutputsByHashAndState(ctx, userID, output.TransactionHash, 0)
				require.NoError(t, err)
				require.Len(t, os, 1)
				return os[0]
			}
			// Deposits preserve their full amounts, including below-unit fractions.
			for _, output := range outputs {
				require.Equal(t, output.Amount, readOutput(output).Amount)
			}
			require.Error(t, validateSystemCallAssetAmounts(outputs))
			id := uuid.Must(uuid.NewV4()).String()
			out := testBuildUserRequest(node, mix.String(), id, "", "0.0001", mtg.StorageAssetId, OperationTypeSystemCall, nil, nil, nil)
			out.TestAttachActionToGroup(node.group)
			req, err := node.parseRequest(out)
			require.NoError(t, err)
			require.NoError(t, node.store.WriteRequestIfNotExist(ctx, req))
			// The first asset is funded, but the second is not: no partial refund or
			// output consumption may escape while compaction is still required.
			_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{md}, node.conf.AppId, invalidID, "", "", 1, decimal.RequireFromString(test.refund))
			require.NoError(t, err)
			txs, compaction := node.refundSystemCallDeposits(ctx, req, outputs, mix)
			require.Empty(t, txs)
			require.Equal(t, validID, compaction)
			for _, output := range outputs {
				require.EqualValues(t, common.RequestStateInitial, readOutput(output).State)
			}
			ar, _, err := node.store.ReadActionResult(ctx, out.OutputId, req.Id)
			require.NoError(t, err)
			require.Equal(t, validID, ar.Compaction)
			// Restore after compaction and refund both the invalid asset and valid SOL.
			_, err = testWriteOutputForNodes(ctx, []*mtg.SQLite3Store{md}, node.conf.AppId, validID, "", "", 2, decimal.NewFromInt(1))
			require.NoError(t, err)
			require.NoError(t, node.store.ResetRequest(ctx, req.Id, req.Sequence))
			out.TestAttachActionToGroup(node.group)
			txs, compaction = node.refundSystemCallDeposits(ctx, req, outputs, mix)
			require.Empty(t, compaction)
			require.Len(t, txs, 2)
			require.Equal(t, invalidID, txs[0].AssetId)
			require.Equal(t, test.refund, txs[0].Amount)
			require.Equal(t, validID, txs[1].AssetId)
			require.Equal(t, "1", txs[1].Amount)
			for _, tx := range txs {
				require.Equal(t, []string{mtg.UniqueId(mix.Members()[0], mix.Members()[0])}, tx.Receivers)
				require.Equal(t, int(mix.Threshold), tx.Threshold)
			}
			for _, output := range outputs {
				saved := readOutput(output)
				require.Equal(t, output.Amount, saved.Amount)
				require.EqualValues(t, common.RequestStateFailed, saved.State)
			}
			require.EqualValues(t, common.RequestStateInitial, readOutput(unreferenced).State)
			savedReq, err := node.store.ReadRequest(ctx, req.Id)
			require.NoError(t, err)
			require.EqualValues(t, common.RequestStateFailed, savedReq.State)
			// Duplicate delivery returns cached transactions instead of refunding again.
			replayed, compaction := node.processAction(ctx, out)
			require.Empty(t, compaction)
			require.Len(t, replayed, 2)
			for i := range txs {
				require.True(t, txs[i].Equal(replayed[i]))
			}
		})
	}
}
