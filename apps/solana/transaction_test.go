package solana

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	bot "github.com/MixinNetwork/bot-api-go-client/v3"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/require"
)

func TestCreateMintsUTF8Metadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		var result any
		switch request.Method {
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]any{"slot": 1}, "value": map[string]any{
				"blockhash": solana.Hash{1}.String(), "lastValidBlockHeight": 100,
			}}
		case "getRecentPrioritizationFees":
			result = []any{}
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
	client := NewClient(server.URL)
	payer, mtg, mint := solana.NewWallet(), solana.NewWallet(), solana.NewWallet()
	for _, test := range []struct {
		name, symbol, wantName, wantSymbol string
	}{
		{"Token", "TOK", "Token", "TOK"},
		{strings.Repeat("n", 33), "12345678901", strings.Repeat("n", 32), "1234567890"},
		{strings.Repeat("n", 31) + "é", "123456789é", strings.Repeat("n", 31), "123456789"},
		{strings.Repeat("n", 30) + "éx", "12345678éx", strings.Repeat("n", 30) + "é", "12345678é"},
		{strings.Repeat("界", 11), "界界界界", strings.Repeat("界", 10), "界界界"},
		{strings.Repeat("🚀", 9), "🚀🚀🚀", strings.Repeat("🚀", 8), "🚀🚀"},
		{"", "", "", ""},
	} {
		t.Run(test.symbol, func(t *testing.T) {
			asset := &DeployedAsset{
				AssetId: common.SafeLitecoinChainId, ChainId: common.SafeLitecoinChainId,
				Address: mint.PublicKey().String(), PrivateKey: &mint.PrivateKey,
				Asset: &bot.AssetNetwork{Name: test.name, Symbol: test.symbol},
			}
			tx, err := client.CreateMints(context.Background(), payer.PublicKey(), mtg.PublicKey(), []*DeployedAsset{asset}, 1_461_600)
			require.NoError(t, err)
			metadataCount := 0
			for _, ix := range tx.Message.Instructions {
				program, err := tx.Message.Program(ix.ProgramIDIndex)
				require.NoError(t, err)
				if program != solana.TokenMetadataProgramID {
					continue
				}
				metadataCount++
				// Decode the actual byte strings sent in the create instruction.
				data := []byte(ix.Data)
				require.Equal(t, []byte{discriminator, createV1Discriminator}, data[:2])
				data = data[2:]
				for i, want := range []string{test.wantName, test.wantSymbol} {
					require.GreaterOrEqual(t, len(data), 4)
					length := int(binary.LittleEndian.Uint32(data[:4]))
					data = data[4:]
					require.GreaterOrEqual(t, len(data), length)
					value := string(data[:length])
					require.True(t, utf8.ValidString(value))
					require.Equal(t, want, value)
					require.LessOrEqual(t, length, []int{maxNameLength, maxSymbolLength}[i])
					data = data[length:]
				}
			}
			require.Equal(t, 1, metadataCount)
		})
	}
}
