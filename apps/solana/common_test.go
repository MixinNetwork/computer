package solana

import (
	"strings"
	"testing"

	"github.com/MixinNetwork/safe/apps/ethereum"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestAssetKeyAddresses(t *testing.T) {
	pda, _, err := solana.FindProgramAddress([][]byte{[]byte("mint")}, solana.TokenProgramID)
	require.NoError(t, err)
	require.False(t, pda.IsOnCurve())

	for _, address := range []string{
		WrappedSolanaAddress,
		pda.String(),
		"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", // USDT.
	} {
		t.Run(address, func(t *testing.T) {
			require.NoError(t, VerifyAssetKey(address))
			require.Equal(t, ethereum.BuildChainAssetId(SolanaChainBase, address), GenerateAssetId(address))
			require.NotEqual(t, SolanaChainBase, GenerateAssetId(address))
		})
	}
	require.NoError(t, VerifyAssetKey(SolanaEmptyAddress))
	require.Equal(t, SolanaChainBase, GenerateAssetId(SolanaEmptyAddress))

	for _, address := range []string{"", "0OIl", strings.Repeat("1", 31), strings.Repeat("1", 33), " " + WrappedSolanaAddress, WrappedSolanaAddress + " "} {
		require.Error(t, VerifyAssetKey(address), address)
	}
}
