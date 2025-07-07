package solana

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func (node *Node) checkCreatedAtaUntilSufficient(ctx context.Context, tx *solana.Transaction) error {
	as := solanaApp.ExtractCreatedAtasFromTransaction(ctx, tx)
	for _, ata := range as {
		for {
			acc, err := node.RPCGetAccount(ctx, ata)
			if err != nil {
				return err
			}
			if acc != nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

func (node *Node) checkMintsUntilSufficient(ctx context.Context, ts []*solanaApp.TokenTransfer) error {
	for _, t := range ts {
		for {
			acc, err := node.RPCGetAccount(ctx, t.Mint)
			if err != nil {
				return err
			}
			if acc != nil {
				break
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

func (node *Node) SendTransactionUtilConfirm(ctx context.Context, tx *solana.Transaction, call *store.SystemCall) (*rpc.GetTransactionResult, error) {
	id := ""
	if call != nil {
		id = call.RequestId
	}

	hash := tx.Signatures[0].String()
	retry := SolanaTxRetry
	for {
		rpcTx, err := node.RPCGetTransaction(ctx, hash)
		if err != nil {
			return nil, fmt.Errorf("solana.RPCGetTransaction(%s) => %v", hash, err)
		}
		if rpcTx != nil {
			return rpcTx, nil
		}

		sig, sendError := node.solana.SendTransaction(ctx, tx)
		logger.Printf("solana.SendTransaction(%s) => %s %v", id, sig, sendError)
		if sendError == nil {
			retry -= 1
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if strings.Contains(sendError.Error(), "This transaction has already been processed") {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		if strings.Contains(sendError.Error(), "Blockhash not found") {
			// retry when observer send tx without nonce account
			if call == nil {
				retry -= 1
				if retry > 0 {
					time.Sleep(5 * time.Second)
					continue
				}
				return nil, sendError
			}

			// outdated nonce account hash when sending tx at first time
			if retry == SolanaTxRetry {
				return nil, sendError
			}
		}

		retry -= 1
		if retry > 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		return nil, sendError
	}
}

func (node *Node) GetPayerBalance(ctx context.Context) (uint64, error) {
	return node.solana.RPCGetBalance(ctx, node.SolanaPayer())
}

func (node *Node) RPCGetTransaction(ctx context.Context, signature string) (*rpc.GetTransactionResult, error) {
	key := fmt.Sprintf("getTransaction:%s", signature)
	value, err := node.store.ReadCache(ctx, key, store.CacheTTL)
	if err != nil {
		panic(err)
	}
	if value != "" {
		var r rpc.GetTransactionResult
		err = json.Unmarshal(common.DecodeHexOrPanic(value), &r)
		if err != nil {
			panic(err)
		}
		return &r, nil
	}

	tx, err := node.solana.RPCGetTransaction(ctx, signature)
	if err != nil || tx == nil {
		return nil, err
	}
	b, err := json.Marshal(tx)
	if err != nil {
		panic(err)
	}
	err = node.store.WriteCache(ctx, key, hex.EncodeToString(b), store.CacheTTL)
	if err != nil {
		panic(err)
	}
	return tx, nil
}

func (node *Node) RPCGetAccount(ctx context.Context, account solana.PublicKey) (*rpc.GetAccountInfoResult, error) {
	key := fmt.Sprintf("getAccount:%s", account.String())
	value, err := node.store.ReadCache(ctx, key, store.CacheTTL)
	if err != nil {
		panic(err)
	}

	if value != "" {
		var r rpc.GetAccountInfoResult
		err = json.Unmarshal(common.DecodeHexOrPanic(value), &r)
		if err != nil {
			panic(err)
		}
		return &r, nil
	}

	acc, err := node.solana.RPCGetAccount(ctx, account)
	if err != nil {
		panic(err)
	}
	if acc == nil {
		return nil, nil
	}
	b, err := json.Marshal(acc)
	if err != nil {
		panic(err)
	}
	err = node.store.WriteCache(ctx, key, hex.EncodeToString(b), store.CacheTTL)
	if err != nil {
		panic(err)
	}
	return acc, nil
}

func (node *Node) RPCGetMultipleAccounts(ctx context.Context, as solana.PublicKeySlice) (*rpc.GetMultipleAccountsResult, error) {
	accounts, err := node.solana.RPCGetMultipleAccounts(ctx, as)
	if err != nil {
		return nil, err
	}
	for index, acc := range accounts.Value {
		if acc == nil {
			continue
		}
		account := &rpc.GetAccountInfoResult{
			RPCContext: accounts.RPCContext,
			Value:      acc,
		}
		key := fmt.Sprintf("getAccountInfo:%s", as[index].String())
		b, err := json.Marshal(account)
		if err != nil {
			panic(err)
		}
		err = node.store.WriteCache(ctx, key, hex.EncodeToString(b), store.CacheTTL)
		if err != nil {
			panic(err)
		}
	}
	return accounts, nil
}

func (node *Node) RPCGetAsset(ctx context.Context, account string) (*solanaApp.Asset, error) {
	key := fmt.Sprintf("getAsset:%s", account)
	value, err := node.store.ReadCache(ctx, key, store.CacheTTL)
	if err != nil {
		panic(err)
	}

	if value != "" {
		var a solanaApp.Asset
		err = json.Unmarshal(common.DecodeHexOrPanic(value), &a)
		if err != nil {
			panic(err)
		}
		return &a, nil
	}

	asset, err := node.solana.RPCGetAsset(ctx, account)
	if err != nil {
		panic(err)
	}
	if asset == nil {
		return nil, nil
	}
	b, err := json.Marshal(asset)
	if err != nil {
		panic(err)
	}
	err = node.store.WriteCache(ctx, key, hex.EncodeToString(b), store.CacheTTL)
	if err != nil {
		panic(err)
	}
	return asset, nil
}

func (node *Node) RPCGetMinimumBalanceForRentExemption(ctx context.Context, dataSize uint64) (uint64, error) {
	key := fmt.Sprintf("getMinimumBalanceForRentExemption:%d", dataSize)
	value, err := node.store.ReadCache(ctx, key, store.CacheTTL)
	if err != nil {
		panic(err)
	}

	if value != "" {
		num, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			panic(err)
		}
		return num, nil
	}

	rentExemptBalance, err := node.solana.RPCGetMinimumBalanceForRentExemption(ctx, dataSize)
	if err != nil {
		return 0, fmt.Errorf("soalan.GetMinimumBalanceForRentExemption(%d) => %v", dataSize, err)
	}
	err = node.store.WriteCache(ctx, key, fmt.Sprintf("%d", rentExemptBalance), store.CacheTTL)
	if err != nil {
		panic(err)
	}
	return rentExemptBalance, nil
}

func (node *Node) RPCGetConfirmedHeight(ctx context.Context) (uint64, error) {
	key := "getLatestBlockhash"
	value, err := node.store.ReadCache(ctx, key, time.Second*5)
	if err != nil {
		panic(err)
	}

	if value != "" {
		num, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			panic(err)
		}
		return num, nil
	}

	height, err := node.solana.RPCGetConfirmedHeight(ctx)
	if err != nil {
		return 0, fmt.Errorf("soalan.RPCGetConfirmedHeight() => %v", err)
	}
	err = node.store.WriteCache(ctx, key, fmt.Sprint(height), time.Second*5)
	if err != nil {
		panic(err)
	}
	return height, nil
}
