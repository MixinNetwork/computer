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
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
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
			changes := node.buildUserBalanceChangesFromMeta(ctx, tx, result.Meta, source)
			require.True(changes[solanaApp.SolanaEmptyAddress].Amount.IsZero())
		}
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
				{name: "omit SOL dust", expected: changes(user, solana.PublicKey{}, 9)},
				{name: "SOL threshold requires transfer", expected: changes(user, solana.PublicKey{}, 10), wantErr: "non-existed transfer deposit"},
				{name: "accept SOL threshold", expected: changes(user, solana.PublicKey{}, 10), instructions: []solana.Instruction{system.NewTransferInstruction(10, user, entry).Build()}},
				{name: "unexpected SOL dust exempt", instructions: []solana.Instruction{system.NewTransferInstruction(9, user, entry).Build()}},
				{name: "unexpected SOL threshold rejected", instructions: []solana.Instruction{system.NewTransferInstruction(10, user, entry).Build()}, wantErr: "unexpected transfer deposit"},
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
