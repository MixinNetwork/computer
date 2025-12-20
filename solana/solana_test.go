package solana

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	testRpcEndpoint         = "https://api.mainnet-beta.solana.com"
	testNonceAccountAddress = "FLq1XqAbaFjib59q6mRDRFEzoQnTShWu1Vis7q57HKtd"
	testNonceAccountHash    = "8j6J9Z8GdbkY1VsJKuKk799nGfkNchMGZ9LY2bdvtYrZ"

	testPayerPrivKey            = "56HtVW5YQ9Xi8MTeQFAWdSuzV17mrDAr1AUCYzTdx36VLvsodA89eSuZd6axrufzo4tyoUNdgjDpm4fnLJLRcXmF"
	testUserNonceAccountPrivKey = "5mCExzNoFSY8UwVbGYPiVtmfeWtqoNeprRymq4wU7yZwWxVCrpXoX7F2KSEFrbVEPRSUjejAeNBbFYMhC3iiu4F5"
	testUserNonceAccountHash    = "FrqtK1eTYLJtR6mGNaBWF6qyfpjTqk1DJaAQdAm31Xc1"
)

func TestSolana(t *testing.T) {
	require := require.New(t)
	ctx, nodes, _ := testPrepare(require, true)
	testFROSTPrepareKeys(ctx, require, nodes, testFROSTKeys1, "fb17b60698d36d45bc624c8e210b4c845233c99a7ae312a27e883a8aa8444b9b")
	testFROSTPrepareKeys(ctx, require, nodes, testFROSTKeys2, "4375bcd5726aadfdd159135441bbe659c705b37025c5c12854e9906ca8500295")

	node := nodes[0]
	count, err := node.store.CountKeys(ctx)
	require.Nil(err)
	require.Equal(2, count)
	key, err := node.store.ReadLatestPublicKey(ctx)
	require.Nil(err)
	require.Equal("4375bcd5726aadfdd159135441bbe659c705b37025c5c12854e9906ca8500295", key)
	payer, err := solana.PrivateKeyFromBase58(testPayerPrivKey)
	require.Nil(err)
	require.Equal("ErFBVPGYmi8Vjuf1jAfmZLzyFHLnF9c1MNhfcEQGdgMb", payer.PublicKey().String())
	addr := solana.PublicKeyFromBytes(common.DecodeHexOrPanic(key))
	require.Equal("5YLSixqjK2m8ECirGaco8tHSn2Uc4aY7cLPoMSMptsgG", addr.String())

	nonceAccount := solana.MustPrivateKeyFromBase58(testUserNonceAccountPrivKey)
	require.Equal("DaJw3pa9rxr25AT1HnQnmPvwS4JbnwNvQbNLm8PJRhqV", nonceAccount.PublicKey().String())
	nonceHash := solana.MustHashFromBase58(testUserNonceAccountHash)

	amount, _ := decimal.NewFromString("0.0001")

	b := solana.NewTransactionBuilder()
	b.SetRecentBlockHash(nonceHash)
	b.SetFeePayer(payer.PublicKey())
	b.AddInstruction(system.NewAdvanceNonceAccountInstruction(
		nonceAccount.PublicKey(),
		solana.SysVarRecentBlockHashesPubkey,
		payer.PublicKey(),
	).Build())
	b.AddInstruction(system.NewTransferInstruction(
		decimal.New(1, solanaApp.SolanaDecimal).Mul(amount).BigInt().Uint64(),
		addr,
		addr,
	).Build())
	tx, err := b.Build()
	require.Nil(err)
	_, err = tx.PartialSign(solanaApp.BuildSignersGetter(payer))
	require.Nil(err)

	testFROSTSign(ctx, require, nodes, nonceAccount.PublicKey().String(), key, tx)
}

func TestGetNonceAccountHash(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	rpc := testRpcEndpoint
	if er := os.Getenv("SOLANARPC"); er != "" {
		rpc = er
	}
	rpcClient := solanaApp.NewClient(rpc)

	key := solana.MustPublicKeyFromBase58(testNonceAccountAddress)
	hash, err := rpcClient.GetNonceAccountHash(ctx, key)
	require.Nil(err)
	require.Equal(testNonceAccountHash, hash.String())
}

func TestGetTxMemo(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	rpc := testRpcEndpoint
	if er := os.Getenv("SOLANARPC"); er != "" {
		rpc = er
	}
	rpcClient := solanaApp.NewClient(rpc)

	rpcTx, err := rpcClient.RPCGetTransaction(ctx, "sKxPceKjZb4PiywqwMP3Wyf9YoPqdgZZ5MLNxkpRv7qgTXVxK8UQRGkjMT47qJht5muuUPpqynkrM5C6BcYSELg")
	require.Nil(err)
	tx, err := rpcTx.Transaction.GetTransaction()
	require.Nil(err)
	memo := solanaApp.ExtractMemoFromTransaction(ctx, tx, rpcTx.Meta, solana.MPK("5ECPyQVa9gZuig8guSmofttMfYjCMRxqa6nCciFTrsTB"))
	require.Equal("74d03590-1a28-3666-9225-f32b2c97ad51", memo)
}

func TestCreateV1(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	rpc := testRpcEndpoint
	if er := os.Getenv("SOLANARPC"); er != "" {
		rpc = er
	}
	rpcClient := solanaApp.NewClient(rpc)

	mint := solana.MustPrivateKeyFromBase58("bH2GaiFeQVbPKitvweDD9ae8i2peb6QohWZTxBRJKP37siCsWte8HAd9uvbP7dqsL25HUWSDuFKfnPjAyydTnJC")
	tx, err := rpcClient.CreateMints(
		ctx,
		solana.MPK("5ECPyQVa9gZuig8guSmofttMfYjCMRxqa6nCciFTrsTB"),
		solana.MPK("5v1eqBfJQkX4JYCi43v7eApXERTNakRBJX1d6Ax6KRzK"),
		[]*solanaApp.DeployedAsset{
			{
				Address: "AnF3RoYAxAETRPAddDWtMET5wL83uzEmbUjkHE93zsHS",
				Uri:     "https://kernel.mixin.dev/objects/9cfd190f6d87070dac3db4209f4f1db8925a59f668e2befd7b8f4c43927526e6",
				Asset: &bot.AssetNetwork{
					Name:   "amituofo2",
					Symbol: "AMI",
				},
				PrivateKey: &mint,
			},
		},
		1461600,
	)
	require.Nil(err)
	fmt.Println(tx)

	ins := tx.Message.Instructions[2]
	require.Equal(
		"2a0009000000616d6974756f666f3203000000414d496100000068747470733a2f2f6b65726e656c2e6d6978696e2e6465762f6f626a656374732f3963666431393066366438373037306461633364623432303966346631646238393235613539663636386532626566643762386634633433393237353236653600000101000000490344a1ba98fc23c897fcf1215189b5b23965911e580f6e6a37d071a29cd69c006400010200000000010800",
		hex.EncodeToString(ins.Data),
	)
}

func TestCreateIdempotent(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	rpc1 := testRpcEndpoint
	c := solanaApp.NewClient(rpc1)

	blockhash := solana.MustHashFromBase58("2fGgNDhkTwhBH1PqS6xpgmKSCSVdYUGuZBMCSQoAMRDt")
	owner := solana.MPK("73yoz7kK3zgh2ScD9aTJpXCrKHETi1xyEKfMTH95ugff")
	mint := solana.MPK("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB")
	ata := solanaApp.FindAssociatedTokenAddress(owner, mint, solana.TokenProgramID)
	tx, err := solana.NewTransaction(
		[]solana.Instruction{
			solanaApp.NewCreateIdempotentInstruction(
				owner,
				ata,
				owner,
				mint,
				system.ProgramID,
				solana.TokenProgramID,
			).Build(),
		},
		blockhash,
		solana.TransactionPayer(owner),
	)
	require.Nil(err)

	data, err := tx.Message.MarshalBinary()
	require.Nil(err)
	require.Equal(
		"0100040659e984bf1923c33a3d247aa4f9ce780423028db7b8d0e1ef452d7b3d46dd8f9ec3f3b4b766373251dbbdde0c9be5824640f5ede70bbeda2422c57da2d9078380ce010e60afedb22717bd63192f54145a3f965a33bb82d2c7029eb2ce1e208264000000000000000000000000000000000000000000000000000000000000000006ddf6e1d765a193d9cbe146ceeb79ac1cb485ed5f5b37913a8cf5857eff00a98c97258f4e2489f1bb3d1029148e0d830b5a1399daff1084048e7bd8dbe9f85918a97c54168e4c731e1cb11427eac8ad346847dbc1714cf592b2f748613a52fb0105060001000203040101",
		hex.EncodeToString(data),
	)

	blockhash = solana.MustHashFromBase58("AqLCRjEWfstqpdzyvLbn3NKZeW9oXRmuaKxebBvTQdK7")
	mint = solana.MPK("9wA7eF6kaBkPhCbwDu8gojpHSfdjTMWD8VhbmS846nN7")
	ata = solanaApp.FindAssociatedTokenAddress(owner, mint, solana.Token2022ProgramID)
	tx, err = solana.NewTransaction(
		[]solana.Instruction{
			solanaApp.NewCreateIdempotentInstruction(
				owner,
				ata,
				owner,
				mint,
				system.ProgramID,
				solana.Token2022ProgramID,
			).Build(),
		},
		blockhash,
		solana.TransactionPayer(owner),
	)
	require.Nil(err)

	data, err = tx.Message.MarshalBinary()
	require.Nil(err)
	require.Equal(
		"0100040659e984bf1923c33a3d247aa4f9ce780423028db7b8d0e1ef452d7b3d46dd8f9e667c6ecde852cd23d501dd593eb1b5e115b0285491058be7682fb475e797283e84bd2a383f1dfc9e5ed5982192551ab563b0057f26b74e581c04e953810119b4000000000000000000000000000000000000000000000000000000000000000006ddf6e1ee758fde18425dbce46ccddab61afc4d83b90d27febdf928d8a18bfc8c97258f4e2489f1bb3d1029148e0d830b5a1399daff1084048e7bd8dbe9f859921ac46d8d98f055dc016163184fa4112145e4aa2c53b892dcc433fa713d13020105060001000203040101",
		hex.EncodeToString(data),
	)

	builder := solana.NewTransactionBuilder()
	builder.SetFeePayer(owner)
	transfer := &solanaApp.TokenTransfer{
		SolanaAsset: true,
		AssetId:     "cb54aed4-1893-3977-b739-ec7b2e04f0c5",
		Destination: solana.MPK("9WKkVWoWuj8RQbSbe6tsU939aJYbjsDEEPb9J7p98bcQ"),
		Mint:        solana.MPK("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB"),
		Amount:      1,
		Decimals:    6,
	}
	builder, err = c.AddTransferSolanaAssetInstruction(ctx, builder, transfer, owner, owner)
	require.Nil(err)

	builder.SetRecentBlockHash(solana.MustHashFromBase58("6QfjA7T1GojSuoo2NVd38iw47qBtgVN4kDsTMyp4QxFs"))
	tx, err = builder.Build()
	require.Nil(err)
	data, err = tx.Message.MarshalBinary()
	require.Nil(err)
	require.Equal(
		"0100050859e984bf1923c33a3d247aa4f9ce780423028db7b8d0e1ef452d7b3d46dd8f9e19d947119f4554e9755871e1bb887205dfedb6262ef40c15abce81d3530cc787c3f3b4b766373251dbbdde0c9be5824640f5ede70bbeda2422c57da2d90783807e608a165388caafffe2a5d24baa9589c2ecfc10b674b9adff936b94e90e42c5ce010e60afedb22717bd63192f54145a3f965a33bb82d2c7029eb2ce1e208264000000000000000000000000000000000000000000000000000000000000000006ddf6e1d765a193d9cbe146ceeb79ac1cb485ed5f5b37913a8cf5857eff00a98c97258f4e2489f1bb3d1029148e0d830b5a1399daff1084048e7bd8dbe9f859505a94f280514a58cb479c399a6cb9006c3b7337eb6b5731015687bb0f14454202070600010304050601010604020401000a0c010000000000000006",
		hex.EncodeToString(data),
	)

	ata = solanaApp.FindAssociatedTokenAddress(transfer.Destination, transfer.Mint, solana.TokenProgramID)
	as := solanaApp.ExtractCreatedAtasFromTransaction(ctx, tx)
	require.Len(as, 1)
	require.True(as[0].Equals(ata))
}

func testFROSTSign(ctx context.Context, require *require.Assertions, nodes []*Node, nonce, public string, tx *solana.Transaction) {
	msg, err := tx.Message.MarshalBinary()
	require.Nil(err)
	require.Equal(
		"02000205cdc56c8d087a301b21144b2ab5e1286b50a5d941ee02f62488db0308b943d2d64375bcd5726aadfdd159135441bbe659c705b37025c5c12854e9906ca8500295bad4af79952644bd80881b3934b3e278ad2f4eeea3614e1c428350d905eac4ec06a7d517192c568ee08a845f73d29788cf035c3145b21ab344d8062ea94000000000000000000000000000000000000000000000000000000000000000000000dcc859c62859a93c7ca37d6f180d63ba1f1ccadc68373b6605c4358bd77983060204030203000404000000040201010c02000000a086010000000000",
		hex.EncodeToString(msg),
	)

	now := time.Now().UTC()
	id := uuid.Must(uuid.NewV4()).String()
	sid := common.UniqueId(id, now.String())
	call := &store.SystemCall{
		RequestId:        id,
		Superior:         id,
		RequestHash:      "4375bcd5726aadfdd159135441bbe659c705b37025c5c12854e9906ca8500295",
		Type:             store.CallTypeMain,
		NonceAccount:     nonce,
		Public:           public,
		MessageHash:      crypto.Sha256Hash(msg).String(),
		Raw:              tx.MustToBase64(),
		State:            common.RequestStatePending,
		WithdrawalTraces: sql.NullString{Valid: true, String: ""},
		Signature:        sql.NullString{Valid: false},
		RequestSignerAt:  sql.NullTime{Valid: false},
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	pub := common.Fingerprint(call.Public)
	pub = append(pub, []byte{0, 0, 0, 0, 0, 0, 0, 0}...)
	for _, node := range nodes {
		err := node.store.TestWriteCall(ctx, call)
		require.Nil(err)
		session := &store.Session{
			Id:         sid,
			RequestId:  call.RequestId,
			MixinHash:  crypto.Sha256Hash([]byte(id)).String(),
			MixinIndex: 0,
			Index:      0,
			Operation:  OperationTypeSignInput,
			Public:     hex.EncodeToString(pub),
			Extra:      call.MessageHex(),
			CreatedAt:  now,
		}
		err = node.store.TestWriteSignSession(ctx, call, []*store.Session{session})
		require.Nil(err)
	}

	for _, node := range nodes {
		testWaitOperation(ctx, node, sid)
	}

	node := nodes[0]
	for {
		s, err := node.store.ReadSystemCallByRequestId(ctx, call.RequestId, common.RequestStatePending)
		require.Nil(err)
		if s != nil && s.Signature.Valid {
			return
		}
		time.Sleep(5 * time.Second)
	}
}
