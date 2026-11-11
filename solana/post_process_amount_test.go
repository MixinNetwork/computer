package solana

import (
	"context"
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
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestPostProcessActualAmountsMatchSkippedExpectations(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "amounts.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	payer := solana.NewWallet()
	user := solana.NewWallet().PublicKey()
	entry := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	nonce := solana.NewWallet().PublicKey()
	node := &Node{store: st, conf: &Configuration{SolanaKey: payer.PrivateKey.String()}}
	// Cache a non-NFT result; these tests need no live account state.
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+mint.String(), hex.EncodeToString([]byte(`{"value":{"data":["","base64"]}}`))))
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		for _, kind := range []solanaApp.InitialAssetMovementKind{solanaApp.InitialAssetMovementTransfer, solanaApp.InitialAssetMovementBurn} {
			for _, test := range []struct {
				name     string
				expected string
				actual   uint64
				decimals uint8
				wantErr  bool
			}{
				{name: "zero expected", expected: "0", actual: 1_000_000, decimals: 6, wantErr: true},
				{name: "negative expected", expected: "-1", actual: 1_000_000, decimals: 6, wantErr: true},
				{name: "positive expected below dust", expected: "0.000000001", actual: 1_000_000_000, decimals: 9, wantErr: true},
				{name: "correct positive amount", expected: "1", actual: 1_000_000, decimals: 6},
				{name: "excessive positive amount", expected: "1", actual: 2_000_000, decimals: 6, wantErr: true},
				{name: "actual dust remains exempt", expected: "0", actual: 1, decimals: 9},
			} {
				t.Run(program.String()+"/"+string(kind)+"/"+test.name, func(t *testing.T) {
					userATA := solanaApp.FindAssociatedTokenAddress(user, mint, program)
					entryATA := solanaApp.FindAssociatedTokenAddress(entry, mint, program)
					var ix solana.Instruction
					if kind == solanaApp.InitialAssetMovementTransfer {
						ix = token.NewTransferCheckedInstruction(test.actual, test.decimals, userATA, mint, entryATA, user, nil).Build()
					} else {
						ix = token.NewBurnCheckedInstruction(test.actual, test.decimals, userATA, mint, user, nil).Build()
					}
					data, err := ix.Data()
					require.NoError(t, err)
					tx, err := solana.NewTransaction([]solana.Instruction{
						system.NewAdvanceNonceAccountInstruction(nonce, solana.SysVarRecentBlockHashesPubkey, payer.PublicKey()).Build(),
						solana.NewInstruction(program, ix.Accounts(), data),
					}, solana.Hash{}, solana.TransactionPayer(payer.PublicKey()))
					require.NoError(t, err)
					require.NoError(t, node.VerifySubSystemCallEnvelope(tx, user, true))
					require.NoError(t, node.verifySubSystemCall(ctx, tx, entry, user, false))
					transfers, burns, err := buildInitialAssetMovementMaps(tx, "", payer.PublicKey().String())
					require.NoError(t, err)
					actual := transfers
					if kind == solanaApp.InitialAssetMovementBurn {
						actual = burns
					}
					expected := map[string]*ReferencedTxAsset{
						mint.String(): {Address: mint.String(), Decimal: int(test.decimals), Amount: decimal.RequireFromString(test.expected)},
					}
					err = node.comparePostProcessMovements(ctx, kind, expected, actual, nil)
					if test.wantErr {
						require.ErrorContains(t, err, "invalid "+string(kind)+" user balance change")
					} else {
						require.NoError(t, err)
					}
				})
			}
		}
	}
}

func TestPostProcessAmountExemptions(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "exemptions.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	node := &Node{store: st}
	mint := solana.NewWallet().PublicKey().String()
	nft := solana.NewWallet().PublicKey().String()
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+mint, hex.EncodeToString([]byte(`{"value":{"data":["","base64"]}}`))))
	// Standard SPL mint: supply 1, decimals 0, initialized.
	nftData := make([]byte, 82)
	nftData[36], nftData[45] = 1, 1
	nftAccount, err := json.Marshal(map[string]any{"value": map[string]any{
		"data": []string{base64.StdEncoding.EncodeToString(nftData), "base64"},
	}})
	require.NoError(t, err)
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+nft, hex.EncodeToString(nftAccount)))
	for _, kind := range []solanaApp.InitialAssetMovementKind{solanaApp.InitialAssetMovementTransfer, solanaApp.InitialAssetMovementBurn} {
		t.Run(string(kind), func(t *testing.T) {
			for _, amount := range []string{"0", "-1", "0.000000001"} {
				expected := map[string]*ReferencedTxAsset{
					mint: {Decimal: 9, Amount: decimal.RequireFromString(amount)},
				}
				require.NoError(t, node.comparePostProcessMovements(ctx, kind, expected, nil, nil))
			}
			actual := map[string]*solanaApp.InitialAssetMovement{
				mint: {Kind: kind, TokenAddress: mint, Decimal: 6, Value: big.NewInt(1_000_000)},
			}
			require.ErrorContains(t, node.comparePostProcessMovements(ctx, kind, nil, actual, nil), "unexpected "+string(kind))
			expectedNFT := map[string]*ReferencedTxAsset{nft: {Amount: decimal.Zero}}
			actualNFT := map[string]*solanaApp.InitialAssetMovement{
				nft: {Kind: kind, TokenAddress: nft, Decimal: 0, Value: big.NewInt(1)},
			}
			require.NoError(t, node.comparePostProcessMovements(ctx, kind, expectedNFT, actualNFT, nil))
		})
	}
}

func TestPostProcessMovementDecimals(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite3Store(filepath.Join(t.TempDir(), "decimals.sqlite"))
	require.NoError(t, err)
	defer st.Close()
	node := &Node{store: st}
	mint := solana.NewWallet().PublicKey().String()
	require.NoError(t, st.WriteCache(ctx, "getAccount:"+mint, hex.EncodeToString([]byte(`{"value":{"data":["","base64"]}}`))))
	for _, kind := range []solanaApp.InitialAssetMovementKind{solanaApp.InitialAssetMovementTransfer, solanaApp.InitialAssetMovementBurn} {
		for _, test := range []struct {
			name     string
			expected string
			decimals uint8
			value    int64
			wantErr  bool
		}{
			{name: "wrong decimals same raw amount", expected: "1", decimals: 9, value: 1_000_000, wantErr: true},
			{name: "wrong decimals looks like dust", expected: "0", decimals: 18, value: 1_000_000, wantErr: true},
			{name: "wrong decimals zero amount", expected: "0", decimals: 9, value: 0, wantErr: true},
			{name: "correct decimals", expected: "1", decimals: 6, value: 1_000_000},
			{name: "correct zero amount", expected: "0", decimals: 6, value: 0},
		} {
			t.Run(string(kind)+"/"+test.name, func(t *testing.T) {
				expected := map[string]*ReferencedTxAsset{mint: {Decimal: 6, Amount: decimal.RequireFromString(test.expected)}}
				actual := map[string]*solanaApp.InitialAssetMovement{
					mint: {Kind: kind, TokenAddress: mint, Decimal: test.decimals, Value: big.NewInt(test.value)},
				}
				err := node.comparePostProcessMovements(ctx, kind, expected, actual, nil)
				if test.wantErr {
					require.ErrorContains(t, err, "decimals")
				} else {
					require.NoError(t, err)
				}
			})
		}
	}
}

func TestInitialMovementDecimalsBeforeAggregation(t *testing.T) {
	payer := solana.NewWallet().PublicKey()
	user := solana.NewWallet().PublicKey()
	entry := solana.NewWallet().PublicKey()
	mint := solana.NewWallet().PublicKey()
	type instruction struct {
		burn      bool
		decimals  uint8
		amount    uint64
		fromPayer bool
	}
	for _, program := range []solana.PublicKey{solana.TokenProgramID, solana.Token2022ProgramID} {
		for _, test := range []struct {
			name         string
			instructions []instruction
			includeUser  bool
			excludePayer bool
			wantErr      bool
			wantAmount   int64
		}{
			{name: "consistent transfers", instructions: []instruction{{decimals: 6, amount: 1_000_000}, {decimals: 6, amount: 1_000_000}}, wantAmount: 2_000_000},
			{name: "consistent burns", instructions: []instruction{{burn: true, decimals: 6, amount: 1_000_000}, {burn: true, decimals: 6, amount: 1_000_000}}, wantAmount: 2_000_000},
			{name: "mixed transfer decimals", instructions: []instruction{{decimals: 6, amount: 500_000}, {decimals: 9, amount: 500_000}}, wantErr: true},
			{name: "mixed burn decimals", instructions: []instruction{{burn: true, decimals: 6, amount: 500_000}, {burn: true, decimals: 9, amount: 500_000}}, wantErr: true},
			{name: "wrong first instruction", instructions: []instruction{{decimals: 9, amount: 500_000}, {decimals: 6, amount: 500_000}}, wantErr: true},
			{name: "wrong zero amount instruction", instructions: []instruction{{decimals: 6, amount: 1_000_000}, {decimals: 9, amount: 0}}, wantErr: true},
			{name: "mixed transfer and burn decimals", instructions: []instruction{{decimals: 6, amount: 500_000}, {burn: true, decimals: 9, amount: 500_000}}, wantErr: true},
			{name: "exclude payer before aggregation", instructions: []instruction{{decimals: 9, amount: 1_000_000, fromPayer: true}, {decimals: 6, amount: 1_000_000}}, excludePayer: true, wantAmount: 1_000_000},
			{name: "include user before aggregation", instructions: []instruction{{decimals: 9, amount: 1_000_000, fromPayer: true}, {decimals: 6, amount: 1_000_000}}, includeUser: true, wantAmount: 1_000_000},
		} {
			t.Run(program.String()+"/"+test.name, func(t *testing.T) {
				var instructions []solana.Instruction
				for _, spec := range test.instructions {
					sender := user
					if spec.fromPayer {
						sender = payer
					}
					source := solanaApp.FindAssociatedTokenAddress(sender, mint, program)
					destination := solanaApp.FindAssociatedTokenAddress(entry, mint, program)
					var ix solana.Instruction
					if spec.burn {
						ix = token.NewBurnCheckedInstruction(spec.amount, spec.decimals, source, mint, sender, nil).Build()
					} else {
						ix = token.NewTransferCheckedInstruction(spec.amount, spec.decimals, source, mint, destination, sender, nil).Build()
					}
					data, err := ix.Data()
					require.NoError(t, err)
					instructions = append(instructions, solana.NewInstruction(program, ix.Accounts(), data))
				}
				tx, err := solana.NewTransaction(instructions, solana.Hash{}, solana.TransactionPayer(payer))
				require.NoError(t, err)
				var include, exclude string
				if test.includeUser {
					include = user.String()
				}
				if test.excludePayer {
					exclude = payer.String()
				}
				transfers, burns, err := buildInitialAssetMovementMaps(tx, include, exclude)
				if test.wantErr {
					require.ErrorContains(t, err, "inconsistent movement decimals")
					require.Nil(t, transfers)
					require.Nil(t, burns)
					return
				}
				require.NoError(t, err)
				total := new(big.Int)
				for _, ledger := range []map[string]*solanaApp.InitialAssetMovement{transfers, burns} {
					if movement := ledger[mint.String()]; movement != nil {
						require.EqualValues(t, 6, movement.Decimal)
						total.Add(total, movement.Value)
					}
				}
				require.Equal(t, test.wantAmount, total.Int64())
			})
		}
	}
}
