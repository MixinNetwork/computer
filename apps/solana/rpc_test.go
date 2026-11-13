package solana

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/require"
)

// TestRPCGetAssetLive checks the SDK's missing-account behavior against a real
// RPC endpoint. Like the other live Solana tests, SOLANARPC overrides mainnet.
func TestRPCGetAssetLive(t *testing.T) {
	endpoint := os.Getenv("SOLANARPC")
	if endpoint == "" {
		endpoint = "https://api.mainnet-beta.solana.com"
	}
	client := NewClient(endpoint)
	t.Cleanup(func() { _ = client.rpcClient.Close() })
	// This key has never been submitted to the chain, so its account is absent.
	missing := solana.NewWallet().PublicKey()

	for _, test := range []struct {
		name       string
		commitment rpc.CommitmentType
		getAsset   func(context.Context, string) (*Asset, error)
	}{
		{"confirmed", rpc.CommitmentConfirmed, client.RPCGetConfirmedAsset},
		{"processed", rpc.CommitmentProcessed, client.RPCGetAsset},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			account, err := client.rpcClient.GetAccountInfoWithOpts(ctx, missing, &rpc.GetAccountInfoOpts{
				Encoding: solana.EncodingJSONParsed, Commitment: test.commitment,
			})
			require.ErrorIs(t, err, rpc.ErrNotFound)
			require.Nil(t, account)
			t.Log("live SDK lookup returned rpc.ErrNotFound for a fresh public key")

			asset, err := test.getAsset(ctx, missing.String())
			require.NoError(t, err)
			require.Nil(t, asset)

			const existingMint = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB" // USDT on mainnet.
			asset, err = test.getAsset(ctx, existingMint)
			require.NoError(t, err)
			require.NotNil(t, asset)
			require.Equal(t, existingMint, asset.Address)
			require.Equal(t, solana.TokenProgramID.String(), asset.ProgramId)
			require.EqualValues(t, 6, asset.Decimals)
			require.True(t, asset.IsInitialized)
		})
	}
}
