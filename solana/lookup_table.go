package solana

import (
	"context"
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

	err = node.ExtendLookupTable(ctx, []string{node.SolanaPayer().String()})
	if err != nil {
		panic(err)
	}

	return node.store.WriteProperty(ctx, key, "processed")
}
