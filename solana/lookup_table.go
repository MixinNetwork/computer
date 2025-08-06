package solana

import (
	"context"
	"time"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/mixin/logger"
	sc "github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/program/address_lookup_table"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

const (
	ExtendTableSize = 30
)

func (node *Node) createALTForUsersAndAssets(ctx context.Context) error {
	key := "ADDRESS_LOOKUP_TABLE"
	val, err := node.store.ReadProperty(ctx, key)
	if err != nil {
		panic(err)
	}
	if val != "" {
		return nil
	}

	payer := solana.MustPrivateKeyFromBase58(node.conf.SolanaKey)
	pb := sc.PublicKeyFromString(payer.PublicKey().String())

	var as []string
	users, err := node.store.ListNewUsersAfter(ctx, time.Time{})
	if err != nil {
		panic(err)
	}
	for _, u := range users {
		as = append(as, u.ChainAddress)
	}
	das, err := node.store.ListDeployedAssets(ctx)
	if err != nil {
		panic(err)
	}
	for _, a := range das {
		as = append(as, a.Address)
	}
	nonces, err := node.store.ListNonceAccounts(ctx)
	if err != nil {
		panic(err)
	}
	for _, a := range nonces {
		as = append(as, a.Address)
	}

	var table string
	start := 0
	for {
		if start >= len(as) {
			break
		}
		logger.Printf("handle users address loopup table")
		end := min(start+ExtendTableSize, len(as))

		var accounts []sc.PublicKey
		for i, a := range as[start:end] {
			logger.Println(start+i, a)
			accounts = append(accounts, sc.PublicKeyFromString(a))
		}

		block, err := node.solana.Client().GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
		if err != nil {
			panic(err)
		}
		blockhash := block.Value.Blockhash

		ins := []solana.Instruction{}
		if table == "" {
			instruction, t := solanaApp.BuildCreateAddressLookupTableInstruction(block, pb)
			table = t
			ins = append(ins, instruction)
		}
		ins = append(ins, solanaApp.CustomInstruction{
			Instruction: address_lookup_table.ExtendLookupTable(address_lookup_table.ExtendLookupTableParams{
				LookupTable: sc.PublicKeyFromString(table),
				Authority:   pb,
				Payer:       &pb,
				Addresses:   accounts,
			}),
		})

		tx, err := solana.NewTransaction(
			ins,
			blockhash,
			solana.TransactionPayer(payer.PublicKey()),
		)
		if err != nil {
			panic(err)
		}
		_, err = tx.Sign(solanaApp.BuildSignersGetter(payer))
		if err != nil {
			panic(err)
		}
		_, err = node.SendTransactionUtilConfirm(ctx, tx, nil)
		if err != nil {
			return err
		}

		err = node.store.WriteAddressLookupTables(ctx, table, accounts)
		if err != nil {
			panic(err)
		}
		start = end
		time.Sleep(time.Second)
	}

	return node.store.WriteProperty(ctx, key, "processed")
}

func (node *Node) getAvailableALT(ctx context.Context, num int) (string, error) {
	tables, err := node.store.ListAddressLookupTable(ctx)
	if err != nil {
		return "", err
	}
	for _, table := range tables {
		if table.Space == 0 || table.Space-uint(num) < 1 {
			continue
		}
		return table.Table, nil
	}
	return "", nil
}
