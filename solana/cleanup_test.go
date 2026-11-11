package solana

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"path/filepath"
	"testing"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

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
	require.EqualValues(t, 1_000_000, balance)
	_, err = node.cleanupSOLBalance(ctx, nil, tx, meta, solana.NewWallet().PublicKey())
	require.ErrorContains(t, err, "source absent")
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
	require.EqualValues(t, 1_000_000, balance)
}
