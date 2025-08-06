package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	"github.com/MixinNetwork/safe/mtg"
)

const (
	BASE_HOST        = "https://api-v3.raydium.io"
	POOL_SEARCH_MINT = "/pools/info/mint"
	POOL_KEY_BY_ID   = "/pools/key/ids"
)

type RaydiumPoolKey struct {
	ProgramId string `json:"programId"`
	Id        string `json:"id"`
	Vault     struct {
		A string `json:"A"`
		B string `json:"B"`
	}
	Config struct {
		Id string `json:"id"`
	} `json:"config"`
	ObservationId   string `json:"observationId"`
	ExBitmapAccount string `json:"exBitmapAccount"`
}

type RaydiumPoolsByMintResponse struct {
	Data []RaydiumPool `json:"data"`
}

type RaydiumPool struct {
	Id string `json:"id"`
}

func RequestRaydium(ctx context.Context, path string) ([]byte, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", BASE_HOST+path, bytes.NewReader(nil))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

func RaydiumPoolsByMint(ctx context.Context, mint string) ([]string, error) {
	url := POOL_SEARCH_MINT + fmt.Sprintf(
		"?mint1=%s&mint2=&poolType=concentrated&poolSortField=liquidity&sortType=desc&pageSize=500&page=1",
		mint,
	)
	body, err := RequestRaydium(ctx, url)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data  *RaydiumPoolsByMintResponse `json:"data"`
		Error bot.Error                   `json:"error"`
	}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return nil, err
	}
	if response.Error.Code > 0 {
		return nil, response.Error
	}

	var ids []string
	for _, p := range response.Data.Data {
		ids = append(ids, p.Id)
	}
	return ids, nil
}

func RaydiumPoolKeysById(ctx context.Context, ids []string) ([]*RaydiumPoolKey, error) {
	url := POOL_KEY_BY_ID + fmt.Sprintf("?ids=%s", strings.Join(ids, ","))
	body, err := RequestRaydium(ctx, url)
	if err != nil {
		return nil, err
	}
	var response struct {
		Data  []*RaydiumPoolKey `json:"data"`
		Error bot.Error         `json:"error"`
	}
	err = json.Unmarshal(body, &response)
	if err != nil {
		return nil, err
	}
	if response.Error.Code > 0 {
		return nil, response.Error
	}
	return response.Data, nil
}

func GetRaydiumPoolKeysUntilSufficient(ctx context.Context, mint string) ([]string, error) {
	for {
		pools, err := RaydiumPoolsByMint(ctx, mint)
		if mtg.CheckRetryableError(err) {
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			return nil, err
		}
		keys, err := RaydiumPoolKeysById(ctx, pools)
		if mtg.CheckRetryableError(err) {
			time.Sleep(time.Second)
			continue
		}
		if err != nil {
			return nil, err
		}
		accounts := make(map[string]string)
		for _, k := range keys {
			accounts[k.ProgramId] = "1"
			accounts[k.Id] = "1"
			accounts[k.Vault.A] = "1"
			accounts[k.Vault.B] = "1"
			accounts[k.Config.Id] = "1"
			accounts[k.ObservationId] = "1"
			accounts[k.ExBitmapAccount] = "1"
		}
		return slices.Collect(maps.Keys(accounts)), nil
	}
}
