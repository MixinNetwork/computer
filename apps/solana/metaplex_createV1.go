package solana

import (
	"bytes"

	"github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/types"
	ag_binary "github.com/gagliardetto/binary"
	bin "github.com/gagliardetto/binary"
	"github.com/gagliardetto/solana-go"
)

const (
	Instruction_CreateV1        = 4
	discriminator         uint8 = 42
	createV1Discriminator uint8 = 0
	tokenStandard         uint8 = 2
)

type MetadataArgs struct {
	Name     string
	Symbol   string
	Uri      string
	Decimals uint8
}

type MetaAccounts struct {
	Mint            solana.PublicKey
	MintAuthority   solana.PublicKey
	Payer           solana.PublicKey
	UpdateAuthority solana.PublicKey
}

func NewMetaplexCreateV1Instruction(acc MetaAccounts, args MetadataArgs) types.Instruction {
	pda, _, err := solana.FindTokenMetadataAddress(acc.Mint)
	if err != nil {
		panic(err)
	}
	accounts := []types.AccountMeta{
		{
			PubKey:     common.PublicKeyFromString(pda.String()),
			IsSigner:   false,
			IsWritable: true,
		},
		{
			PubKey:     common.PublicKeyFromString(solana.TokenMetadataProgramID.String()),
			IsSigner:   false,
			IsWritable: false,
		},
		{
			PubKey:     common.PublicKeyFromString(acc.Mint.String()),
			IsSigner:   false,
			IsWritable: true,
		},
		{
			PubKey:     common.PublicKeyFromString(acc.MintAuthority.String()),
			IsSigner:   true,
			IsWritable: false,
		},
		{
			PubKey:     common.PublicKeyFromString(acc.Payer.String()),
			IsSigner:   true,
			IsWritable: true,
		},
		{
			PubKey:     common.PublicKeyFromString(acc.UpdateAuthority.String()),
			IsSigner:   false,
			IsWritable: false,
		},
		{
			PubKey:     common.PublicKeyFromString(solana.SystemProgramID.String()),
			IsSigner:   false,
			IsWritable: false,
		},
		{
			PubKey:     common.PublicKeyFromString(solana.SysVarRentPubkey.String()),
			IsSigner:   false,
			IsWritable: false,
		},
		{
			PubKey:     common.PublicKeyFromString(solana.TokenProgramID.String()),
			IsSigner:   false,
			IsWritable: false,
		},
	}

	buf := new(bytes.Buffer)
	encoder := bin.NewBinEncoder(buf)
	err = marshalWithEncoder(encoder, acc, args)
	if err != nil {
		panic(err)
	}
	return types.Instruction{
		ProgramID: common.PublicKey(solana.TokenMetadataProgramID),
		Accounts:  accounts,
		Data:      buf.Bytes(),
	}
}

func marshalString(encoder *bin.Encoder, val string) error {
	b := []byte(val)
	lb := uint32(len(b))
	err := encoder.WriteUint32(lb, bin.LE)
	if err != nil {
		return err
	}
	return encoder.WriteBytes(b, false)
}

func marshalWithEncoder(encoder *ag_binary.Encoder, acc MetaAccounts, args MetadataArgs) (err error) {
	err = encoder.WriteBytes([]byte{discriminator, createV1Discriminator}, false)
	if err != nil {
		return err
	}

	err = marshalString(encoder, args.Name)
	if err != nil {
		return err
	}
	err = marshalString(encoder, args.Symbol)
	if err != nil {
		return err
	}
	err = marshalString(encoder, args.Uri)
	if err != nil {
		return err
	}
	// sellerFeeBasisPoints and prefix of creators
	err = encoder.WriteBytes([]byte{0, 0, 1, 1, 0, 0, 0}, false)
	if err != nil {
		return err
	}
	err = encoder.Encode(acc.UpdateAuthority)
	if err != nil {
		return err
	}
	// verified: false, share: 100
	err = encoder.WriteBytes([]byte{0, 100}, false)
	if err != nil {
		return err
	}
	// primarySaleHappened
	// isMutable
	// tokenStandard
	// collection
	// uses
	// collectionDetails
	// ruleSet
	// decimals
	// printSupply
	return encoder.WriteBytes([]byte{0, 1, tokenStandard, 0, 0, 0, 0, 1, args.Decimals, 0}, false)
}
