package solana

import (
	"bytes"
	"errors"
	"fmt"

	ag_spew "github.com/davecgh/go-spew/spew"
	ag_binary "github.com/gagliardetto/binary"
	ag_treeout "github.com/gagliardetto/treeout"

	ag_solanago "github.com/gagliardetto/solana-go"
	associatedtokenaccount "github.com/gagliardetto/solana-go/programs/associated-token-account"
	ag_text "github.com/gagliardetto/solana-go/text"
	ag_format "github.com/gagliardetto/solana-go/text/format"
)

const (
	// Creates an associated token account for the given wallet address and
	// token mint Returns an error if the account exists.
	Instruction_Create uint8 = iota

	// Creates an associated token account for the given wallet address and
	// token mint, if it doesn't already exist.  Returns an error if the
	// account exists, but with a different owner.
	Instruction_CreateIdempotent
)

type CreateIdempotent struct {
	// [0] = [WRITE, SIGNER] Payer
	// ··········· Funding account
	//
	// [1] = [WRITE] AssociatedTokenAccount
	// ··········· Associated token account address to be created
	//
	// [2] = [] Wallet
	// ··········· Wallet address for the new associated token account
	//
	// [3] = [] TokenMint
	// ··········· The token mint for the new associated token account
	//
	// [4] = [] SystemProgram
	// ··········· System program ID
	//
	// [5] = [] TokenProgram
	// ··········· SPL token program ID
	ag_solanago.AccountMetaSlice `bin:"-" borsh_skip:"true"`
}

func (inst *CreateIdempotent) SetAccounts(accounts []*ag_solanago.AccountMeta) error {
	if len(accounts) != 6 {
		return fmt.Errorf("expected 6 accounts, got %v", len(accounts))
	}
	inst.AccountMetaSlice = accounts
	return nil
}

func (inst CreateIdempotent) GetAccounts() (accounts []*ag_solanago.AccountMeta) {
	accounts = append(accounts, inst.AccountMetaSlice...)
	return
}

// NewCreateIdempotentInstructionBuilder creates a new `CreateIdempotent` instruction builder.
func NewCreateIdempotentInstructionBuilder() *CreateIdempotent {
	return &CreateIdempotent{
		AccountMetaSlice: make(ag_solanago.AccountMetaSlice, 6),
	}
}

// SetFundingAccount sets the "funding" account.
func (inst *CreateIdempotent) SetFundingAccount(funding ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[0] = ag_solanago.Meta(funding).WRITE().SIGNER()
	return inst
}

// GetFundingAccount gets the "funding" account.
func (inst *CreateIdempotent) GetFundingAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[0]
}

// SetAssociatedTokenAccount sets the "associated token" account.
func (inst *CreateIdempotent) SetAssociatedTokenAccount(associatedToken ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[1] = ag_solanago.Meta(associatedToken).WRITE()
	return inst
}

// GetAssociatedTokenAccount gets the "associated token" account.
func (inst *CreateIdempotent) GetAssociatedTokenAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[1]
}

// SetWalletAccount sets the "wallet" account.
func (inst *CreateIdempotent) SetWalletAccount(wallet ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[2] = ag_solanago.Meta(wallet)
	return inst
}

// GetWalletAccount gets the "wallet" account.
func (inst *CreateIdempotent) GetWalletAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[2]
}

// SetTokenMintAccount sets the "token mint" account.
func (inst *CreateIdempotent) SetTokenMintAccount(tokenMint ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[3] = ag_solanago.Meta(tokenMint)
	return inst
}

// GetTokenMintAccount gets the "token mint" account.
func (inst *CreateIdempotent) GetTokenMintAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[3]
}

// SetSystemProgramAccount sets the "system program" account.
func (inst *CreateIdempotent) SetSystemProgramAccount(systemProgram ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[4] = ag_solanago.Meta(systemProgram)
	return inst
}

// GetSystemProgramAccount gets the "system program" account.
func (inst *CreateIdempotent) GetSystemProgramAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[4]
}

// SetTokenProgramAccount sets the "token program" account.
func (inst *CreateIdempotent) SetTokenProgramAccount(tokenProgram ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[5] = ag_solanago.Meta(tokenProgram)
	return inst
}

// GetTokenProgramAccount gets the "token program" account.
func (inst *CreateIdempotent) GetTokenProgramAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[5]
}

// SetSysVarRentAccount sets the "sys var rent" account.
func (inst *CreateIdempotent) SetSysVarRentAccount(sysVarRent ag_solanago.PublicKey) *CreateIdempotent {
	inst.AccountMetaSlice[6] = ag_solanago.Meta(sysVarRent)
	return inst
}

// GetSysVarRentAccount gets the "sys var rent" account.
func (inst *CreateIdempotent) GetSysVarRentAccount() *ag_solanago.AccountMeta {
	return inst.AccountMetaSlice[6]
}

func (inst CreateIdempotent) Build() *AtaInstruction {
	return &AtaInstruction{BaseVariant: ag_binary.BaseVariant{
		Impl:   inst,
		TypeID: ag_binary.TypeIDFromUint8(Instruction_CreateIdempotent),
	}}
}

// ValidateAndBuild validates the instruction parameters and accounts;
// if there is a validation error, it returns the error.
// Otherwise, it builds and returns the instruction.
func (inst CreateIdempotent) ValidateAndBuild() (*AtaInstruction, error) {
	if err := inst.Validate(); err != nil {
		return nil, err
	}
	return inst.Build(), nil
}

func (inst *CreateIdempotent) Validate() error {
	if inst.AccountMetaSlice.Get(0).PublicKey.IsZero() {
		return errors.New("Payer not set")
	}
	if inst.AccountMetaSlice.Get(2).PublicKey.IsZero() {
		return errors.New("Wallet not set")
	}
	if inst.AccountMetaSlice.Get(3).PublicKey.IsZero() {
		return errors.New("Mint not set")
	}
	_, _, err := ag_solanago.FindAssociatedTokenAddress(
		inst.AccountMetaSlice.Get(2).PublicKey,
		inst.AccountMetaSlice.Get(3).PublicKey,
	)
	if err != nil {
		return fmt.Errorf("error while FindAssociatedTokenAddress: %w", err)
	}
	return nil
}

func (inst *CreateIdempotent) EncodeToTree(parent ag_treeout.Branches) {
	parent.Child(ag_format.Program(AtaProgramName, associatedtokenaccount.ProgramID)).
		ParentFunc(func(programBranch ag_treeout.Branches) {
			programBranch.Child(ag_format.Instruction("CreateIdempotent")).
				ParentFunc(func(instructionBranch ag_treeout.Branches) {
					instructionBranch.Child("Accounts[len=6]").ParentFunc(func(accountsBranch ag_treeout.Branches) {
						accountsBranch.Child(ag_format.Meta("                 payer", inst.AccountMetaSlice.Get(0)))
						accountsBranch.Child(ag_format.Meta("associatedTokenAddress", inst.AccountMetaSlice.Get(1)))
						accountsBranch.Child(ag_format.Meta("                wallet", inst.AccountMetaSlice.Get(2)))
						accountsBranch.Child(ag_format.Meta("             tokenMint", inst.AccountMetaSlice.Get(3)))
						accountsBranch.Child(ag_format.Meta("         systemProgram", inst.AccountMetaSlice.Get(4)))
						accountsBranch.Child(ag_format.Meta("          tokenProgram", inst.AccountMetaSlice.Get(5)))
					})
				})
		})
}

func (inst CreateIdempotent) MarshalWithEncoder(encoder *ag_binary.Encoder) (err error) {
	return nil
}

func (inst *CreateIdempotent) UnmarshalWithDecoder(_ *ag_binary.Decoder) (err error) {
	return nil
}

// NewCreateIdempotentInstruction declares a new CreateIdempotent instruction with the provided parameters and accounts.
func NewCreateIdempotentInstruction(
	// Accounts:
	funding ag_solanago.PublicKey,
	associatedTokenAccount ag_solanago.PublicKey,
	wallet ag_solanago.PublicKey,
	tokenMint ag_solanago.PublicKey,
	systemProgram ag_solanago.PublicKey,
	tokenProgram ag_solanago.PublicKey,
) *CreateIdempotent {
	return NewCreateIdempotentInstructionBuilder().
		SetFundingAccount(funding).
		SetAssociatedTokenAccount(associatedTokenAccount).
		SetWalletAccount(wallet).
		SetTokenMintAccount(tokenMint).
		SetSystemProgramAccount(systemProgram).
		SetTokenProgramAccount(tokenProgram)
}

var ProgramID ag_solanago.PublicKey = ag_solanago.SPLAssociatedTokenAccountProgramID

// func SetProgramID(pubkey ag_solanago.PublicKey) {
// 	ProgramID = pubkey
// 	ag_solanago.RegisterInstructionDecoder(ProgramID, registryDecodeAtaInstruction)
// }

const AtaProgramName = "AssociatedTokenAccount"

// func init() {
// 	ag_solanago.RegisterInstructionDecoder(ProgramID, registryDecodeAtaInstruction)
// }

// InstructionIDToName returns the name of the instruction given its ID.
func InstructionIDToName(id uint8) string {
	switch id {
	case Instruction_Create:
		return "Create"
	case Instruction_CreateIdempotent:
		return "CreateIdempotent"
	default:
		return ""
	}
}

type AtaInstruction struct {
	ag_binary.BaseVariant
}

func (inst *AtaInstruction) EncodeToTree(parent ag_treeout.Branches) {
	if enToTree, ok := inst.Impl.(ag_text.EncodableToTree); ok {
		enToTree.EncodeToTree(parent)
	} else {
		parent.Child(ag_spew.Sdump(inst))
	}
}

var AtaInstructionImplDef = ag_binary.NewVariantDefinition(
	ag_binary.Uint8TypeIDEncoding,
	[]ag_binary.VariantType{
		{
			Name: "Create", Type: (*associatedtokenaccount.Create)(nil),
		},
		{
			Name: "CreateIdempotent", Type: (*CreateIdempotent)(nil),
		},
	},
)

func (inst *AtaInstruction) ProgramID() ag_solanago.PublicKey {
	return ProgramID
}

func (inst *AtaInstruction) Accounts() (out []*ag_solanago.AccountMeta) {
	return inst.Impl.(ag_solanago.AccountsGettable).GetAccounts()
}

func (inst *AtaInstruction) Data() ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := ag_binary.NewBinEncoder(buf).Encode(inst); err != nil {
		return nil, fmt.Errorf("unable to encode instruction: %w", err)
	}
	return buf.Bytes(), nil
}

func (inst *AtaInstruction) TextEncode(encoder *ag_text.Encoder, option *ag_text.Option) error {
	return encoder.Encode(inst.Impl, option)
}

func (inst *AtaInstruction) UnmarshalWithDecoder(decoder *ag_binary.Decoder) error {
	return inst.BaseVariant.UnmarshalBinaryVariant(decoder, AtaInstructionImplDef)
}

func (inst AtaInstruction) MarshalWithEncoder(encoder *ag_binary.Encoder) error {
	err := encoder.WriteUint8(inst.TypeID.Uint8())
	if err != nil {
		return fmt.Errorf("unable to write variant type: %w", err)
	}
	return encoder.Encode(inst.Impl)
}

func DecodeAtaInstruction(accounts []*ag_solanago.AccountMeta, data []byte) (*AtaInstruction, error) {
	inst := new(AtaInstruction)
	if err := ag_binary.NewBinDecoder(data).Decode(inst); err != nil {
		return nil, fmt.Errorf("unable to decode instruction: %w", err)
	}
	if v, ok := inst.Impl.(ag_solanago.AccountsSettable); ok {
		err := v.SetAccounts(accounts)
		if err != nil {
			return nil, fmt.Errorf("unable to set accounts for instruction: %w", err)
		}
	}
	return inst, nil
}
