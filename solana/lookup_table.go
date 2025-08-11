package solana

import (
	"context"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
)

const (
	ExtendTableSize = 30
)

func (node *Node) createALTForAccounts(ctx context.Context) error {
	key := "ADDRESS_LOOKUP_TABLE"
	val, err := node.store.ReadProperty(ctx, key)
	if err != nil {
		panic(err)
	}
	if val != "" {
		return nil
	}

	as := []string{node.SolanaPayer().String()}
	nonces, err := node.store.ListNonceAccounts(ctx)
	if err != nil {
		panic(err)
	}
	for _, a := range nonces {
		as = append(as, a.Address)
	}

	err = node.ExtendLookupTable(ctx, as)
	if err != nil {
		panic(err)
	}

	return node.store.WriteProperty(ctx, key, "processed")
}

func (node *Node) getAvailableALT(ctx context.Context) (*solanaApp.LookupTableStats, error) {
	tables, err := node.store.ListAddressLookupTable(ctx)
	if err != nil {
		return nil, err
	}
	for _, table := range tables {
		if table.Space == 0 {
			continue
		}
		return &table, nil
	}
	return nil, nil
}
