package solana

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/MixinNetwork/safe/common"
	sc "github.com/blocto/solana-go-sdk/common"
	"github.com/blocto/solana-go-sdk/program/address_lookup_table"
	"github.com/gagliardetto/solana-go"
	tokenAta "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/memo"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
	token2022 "github.com/gagliardetto/solana-go/programs/token-2022"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/shopspring/decimal"
)

const (
	solanaInnerIndexBase = int64(1_000_000_000)

	// Agave runtime limits and denomination constants:
	// https://solana.com/docs/core/constants-reference
	maxComputeUnitLimit = uint32(1_400_000)
	// V1 defaults this limit to zero when omitted, so sets it explicitly.
	maxLoadedAccountsDataSizeLimit = uint32(64 * 1024 * 1024)
	microLamportsPerLamport        = int64(1_000_000)
	defaultMicroLamportsPerCU      = uint64(1_000)

	// Computer fee-payer policy. This is a service limit, not an Agave
	// protocol constant. It caps one transaction's total priority fee at
	// 0.001 SOL.
	maxPriorityFeeLamports = uint64(1_000_000)

	// Solana's compute optimization guide uses a 10% margin over simulated CU:
	// https://solana.com/developers/cookbook/transactions/optimize-compute
	computeUnitMarginNumerator   = uint64(110)
	computeUnitMarginDenominator = uint64(100)
	// Loaded account data can change between simulation and execution. Apply
	// the same 10% operational margin, capped by Agave's 64 MiB limit.
	loadedAccountsDataSizeMarginNumerator   = uint64(110)
	loadedAccountsDataSizeMarginDenominator = uint64(100)
)

func (c *Client) CreateNonceAccount(ctx context.Context, key, nonce string, rent uint64) (*solana.Transaction, error) {
	payer, err := solana.PrivateKeyFromBase58(key)
	if err != nil {
		panic(err)
	}
	nonceKey, err := solana.PrivateKeyFromBase58(nonce)
	if err != nil {
		panic(err)
	}

	block, err := c.rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, fmt.Errorf("solana.GetLatestBlockhash() => %v", err)
	}
	blockhash := block.Value.Blockhash

	instructions := []solana.Instruction{
		system.NewCreateAccountInstruction(
			rent,
			NonceAccountSize,
			system.ProgramID,
			payer.PublicKey(),
			nonceKey.PublicKey(),
		).Build(),
		system.NewInitializeNonceAccountInstruction(
			payer.PublicKey(),
			nonceKey.PublicKey(),
			solana.SysVarRecentBlockHashesPubkey,
			solana.SysVarRentPubkey,
		).Build(),
	}
	tx, err := solana.NewTransaction(
		instructions,
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionV1Config(provisionalV1TransactionConfig()),
	)
	if err != nil {
		panic(err)
	}
	err = c.configureV1Transaction(ctx, tx)
	if err != nil {
		return nil, err
	}
	_, err = tx.Sign(BuildSignersGetter(nonceKey, payer))
	if err != nil {
		panic(err)
	}
	if err := ValidateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func (c *Client) InitializeAccount(ctx context.Context, key, user string) (*solana.Transaction, error) {
	payer := solana.MustPrivateKeyFromBase58(key)

	rentExemptBalance, err := c.RPCGetMinimumBalanceForRentExemption(ctx, NormalAccountSize)
	if err != nil {
		return nil, fmt.Errorf("soalan.GetMinimumBalanceForRentExemption(%d) => %v", NormalAccountSize, err)
	}
	block, err := c.rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, fmt.Errorf("solana.GetLatestBlockhash() => %v", err)
	}
	blockhash := block.Value.Blockhash

	instructions := []solana.Instruction{
		system.NewTransferInstruction(
			rentExemptBalance,
			payer.PublicKey(),
			solana.MPK(user),
		).Build(),
	}
	tx, err := solana.NewTransaction(
		instructions,
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionV1Config(provisionalV1TransactionConfig()),
	)
	if err != nil {
		panic(err)
	}
	err = c.configureV1Transaction(ctx, tx)
	if err != nil {
		return nil, err
	}
	_, err = tx.Sign(BuildSignersGetter(payer))
	if err != nil {
		panic(err)
	}
	if err := ValidateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func (c *Client) CreateMints(ctx context.Context, payer, mtg solana.PublicKey, assets []*DeployedAsset, rent uint64) (*solana.Transaction, error) {
	builder := solana.NewTransactionBuilder()
	builder.SetFeePayer(payer)

	for _, asset := range assets {
		if asset.ChainId == SolanaChainBase {
			return nil, fmt.Errorf("CreateMints(%s) => invalid asset chain", asset.AssetId)
		}
		mint := solana.MustPublicKeyFromBase58(asset.Address)

		builder.AddInstruction(
			system.NewCreateAccountInstruction(
				rent,
				MintSize,
				token.ProgramID,
				payer,
				mint,
			).Build(),
		)
		builder.AddInstruction(
			token.NewInitializeMint2InstructionBuilder().
				SetDecimals(uint8(AssetDecimal)).
				SetMintAuthority(payer).
				SetMintAccount(solana.MustPublicKeyFromBase58(asset.Address)).Build(),
		)

		name := asset.Asset.Name
		if len(name) > maxNameLength {
			name = name[:maxNameLength]
		}
		symbol := asset.Asset.Symbol
		if len(symbol) > maxSymbolLength {
			name = name[:maxSymbolLength]
		}
		builder.AddInstruction(
			CustomInstruction{
				Instruction: NewMetaplexCreateV1Instruction(
					MetaAccounts{
						Mint:            mint,
						MintAuthority:   payer,
						Payer:           payer,
						UpdateAuthority: mtg,
					},
					MetadataArgs{
						Name:     name,
						Symbol:   symbol,
						Uri:      asset.Uri,
						Decimals: AssetDecimal,
					},
				),
			},
		)

		builder.AddInstruction(
			token.NewSetAuthorityInstruction(token.AuthorityMintTokens, mtg, mint, payer, nil).Build(),
		)
	}

	block, err := c.rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, fmt.Errorf("solana.GetLatestBlockhash() => %v", err)
	}
	builder.SetRecentBlockHash(block.Value.Blockhash)
	err = c.configureV1TransactionBuilder(ctx, builder)
	if err != nil {
		return nil, err
	}

	tx, err := builder.Build()
	if err != nil {
		panic(err)
	}
	for _, asset := range assets {
		if asset.PrivateKey == nil {
			return nil, fmt.Errorf("CreateMints(%s) => asset private key is required", asset.AssetId)
		}
		_, err = tx.PartialSign(BuildSignersGetter(*asset.PrivateKey))
		if err != nil {
			if common.CheckTestEnvironment(ctx) {
				tx.Signatures[1] = solana.MustSignatureFromBase58("449h9tg5hCHigegVuH6Waoh8ACDYc5hrhZh2t9td2ToFgtBHrkzH7Z2vSE2nnmNdksUkj71k7eaQhdHrRgj19b5W")
				continue
			}
			panic(err)
		}
	}
	if err := ValidateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

func (c *Client) ExtendLookupTables(ctx context.Context, key, table string, as []sc.PublicKey) (*solana.Transaction, string, error) {
	payer := solana.MustPrivateKeyFromBase58(key)
	pb := sc.PublicKeyFromString(payer.PublicKey().String())

	block, err := c.rpcClient.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, "", fmt.Errorf("solana.GetLatestBlockhash() => %v", err)
	}
	blockhash := block.Value.Blockhash

	var ins []solana.Instruction
	if table == "" {
		instruction, t := BuildCreateAddressLookupTableInstruction(block, pb)
		table = t
		ins = append(ins, instruction)
	}
	ins = append(ins, CustomInstruction{
		Instruction: address_lookup_table.ExtendLookupTable(address_lookup_table.ExtendLookupTableParams{
			LookupTable: sc.PublicKeyFromString(table),
			Authority:   pb,
			Payer:       &pb,
			Addresses:   as,
		}),
	})

	tx, err := solana.NewTransaction(
		ins,
		blockhash,
		solana.TransactionPayer(payer.PublicKey()),
		solana.TransactionV1Config(provisionalV1TransactionConfig()),
	)
	if err != nil {
		panic(err)
	}
	err = c.configureV1Transaction(ctx, tx)
	if err != nil {
		return nil, "", err
	}
	_, err = tx.Sign(BuildSignersGetter(payer))
	if err != nil {
		panic(err)
	}
	if err := ValidateTransaction(tx); err != nil {
		return nil, "", err
	}
	return tx, table, nil
}

func (c *Client) TransferOrMintTokens(ctx context.Context, payer, mtg solana.PublicKey, nonce NonceAccount, transfers []*TokenTransfer, memoStr string) (*solana.Transaction, error) {
	builder, err := c.NewTransferOrMintTokensBuilder(ctx, payer, mtg, nonce, transfers, memoStr)
	if err != nil {
		return nil, err
	}
	err = c.configureV1TransactionBuilder(ctx, builder)
	if err != nil {
		return nil, err
	}

	tx, err := builder.Build()
	if err != nil {
		panic(err)
	}
	err = ValidateTransaction(tx)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// NewTransferOrMintTokensBuilder builds the durable-nonce and asset
// instructions without selecting a transaction version or estimating its v1
// resource configuration. Cleanup verification uses this to rebuild the
// signed business instructions without consulting current RPC state for a
// simulation or priority-fee quote.
func (c *Client) NewTransferOrMintTokensBuilder(ctx context.Context, payer, mtg solana.PublicKey, nonce NonceAccount, transfers []*TokenTransfer, memoStr string) (*solana.TransactionBuilder, error) {
	builder := c.buildInitialTxWithNonceAccount(ctx, payer, nonce)

	for _, transfer := range transfers {
		if transfer.SolanaAsset {
			b, err := c.AddTransferSolanaAssetInstruction(ctx, builder, transfer, payer, mtg)
			if err != nil {
				return nil, err
			}
			builder = b
			continue
		}

		mint := transfer.Mint
		ataAddress := FindAssociatedTokenAddress(transfer.Destination, mint, solana.TokenProgramID)
		builder.AddInstruction(
			tokenAta.NewCreateIdempotentInstructionWithTokenProgram(
				payer,
				transfer.Destination,
				mint,
				solana.TokenProgramID,
			).Build(),
		)

		builder.AddInstruction(
			token.NewMintToInstruction(
				transfer.Amount,
				mint,
				ataAddress,
				mtg,
				nil,
			).Build(),
		)
	}

	if memoStr != "" {
		builder.AddInstruction(
			memo.NewMemoInstruction(
				[]byte(memoStr),
				payer,
			).Build(),
		)
	}
	return builder, nil
}

func (c *Client) TransferOrBurnTokens(ctx context.Context, payer, user solana.PublicKey, nonce NonceAccount, transfers []*TokenTransfer) (*solana.Transaction, error) {
	builder, err := c.NewTransferOrBurnTokensBuilder(ctx, payer, user, nonce, transfers)
	if err != nil {
		return nil, err
	}
	err = c.configureV1TransactionBuilder(ctx, builder)
	if err != nil {
		return nil, err
	}

	tx, err := builder.Build()
	if err != nil {
		panic(err)
	}
	if err := ValidateTransaction(tx); err != nil {
		return nil, err
	}
	return tx, nil
}

// NewTransferOrBurnTokensBuilder is the burn-side counterpart to
// NewTransferOrMintTokensBuilder. It only builds the deterministic transaction
// instructions; callers decide how the final transaction is configured.
func (c *Client) NewTransferOrBurnTokensBuilder(ctx context.Context, payer, user solana.PublicKey, nonce NonceAccount, transfers []*TokenTransfer) (*solana.TransactionBuilder, error) {
	builder := c.buildInitialTxWithNonceAccount(ctx, payer, nonce)

	for _, transfer := range transfers {
		if transfer.SolanaAsset {
			b, err := c.AddTransferSolanaAssetInstruction(ctx, builder, transfer, payer, user)
			if err != nil {
				return nil, err
			}
			builder = b
			continue
		}

		ataAddress := FindAssociatedTokenAddress(user, transfer.Mint, solana.TokenProgramID)
		builder.AddInstruction(
			token.NewBurnCheckedInstruction(
				transfer.Amount,
				transfer.Decimals,
				ataAddress,
				transfer.Mint,
				user,
				nil,
			).Build(),
		)
	}
	return builder, nil
}

func (c *Client) AddTransferSolanaAssetInstruction(ctx context.Context, builder *solana.TransactionBuilder, transfer *TokenTransfer, payer, source solana.PublicKey) (*solana.TransactionBuilder, error) {
	if !transfer.SolanaAsset {
		panic(transfer.AssetId)
	}
	if transfer.AssetId == transfer.ChainId {
		src := source
		if transfer.Fee {
			src = payer
		}
		builder.AddInstruction(
			system.NewTransferInstruction(
				transfer.Amount,
				src,
				transfer.Destination,
			).Build(),
		)
		return builder, nil
	}

	mintAccount, err := c.RPCGetAccount(ctx, transfer.Mint)
	if err != nil {
		panic(err)
	}
	tokenProgram := mintAccount.Value.Owner

	src := FindAssociatedTokenAddress(source, transfer.Mint, tokenProgram)
	dst := FindAssociatedTokenAddress(transfer.Destination, transfer.Mint, tokenProgram)
	builder.AddInstruction(
		tokenAta.NewCreateIdempotentInstructionWithTokenProgram(
			payer,
			transfer.Destination,
			transfer.Mint,
			tokenProgram,
		).Build(),
	)

	switch {
	case tokenProgram.Equals(solana.TokenProgramID):
		builder.AddInstruction(
			token.NewTransferCheckedInstruction(
				transfer.Amount,
				transfer.Decimals,
				src,
				transfer.Mint,
				dst,
				source,
				nil,
			).Build(),
		)
	case tokenProgram.Equals(solana.Token2022ProgramID):
		builder.AddInstruction(
			token2022.NewTransferCheckedInstruction(
				transfer.Amount,
				transfer.Decimals,
				src,
				transfer.Mint,
				dst,
				source,
				nil,
			).Build(),
		)
	default:
		panic(fmt.Errorf("invalid token program id: %s", tokenProgram.String()))
	}
	return builder, nil
}

// provisionalV1TransactionConfig gives simulation enough resources to execute
// the whole transaction. V1 defaults omitted compute and loaded-account limits
// to zero, which would prevent a useful estimate. The zero priority fee does not
// affect compute usage. Outside the offline test environment, the simulated CU
// result replaces this config before the transaction is signed or sent.
func provisionalV1TransactionConfig() solana.TransactionConfig {
	return solana.TransactionConfig{}.
		WithComputeUnitLimit(maxComputeUnitLimit).
		WithLoadedAccountsDataSizeLimit(maxLoadedAccountsDataSizeLimit).
		WithPriorityFee(0)
}

func (c *Client) getV1TransactionConfig(ctx context.Context, tx *solana.Transaction) (solana.TransactionConfig, error) {
	// Computer tests use synthetic accounts and fixed durable-nonce hashes that
	// do not represent current on-chain state. Keep a valid v1 config without
	// making those offline fixtures depend on a live simulation. TestCreateV1
	// exercises the RPC-backed estimation path separately.
	if common.CheckTestEnvironment(ctx) {
		return provisionalV1TransactionConfig(), nil
	}

	// Keep the transaction's real blockhash. Replacing it during simulation
	// breaks durable-nonce transactions because the nonce advance must match it.
	simulation, err := c.rpcClient.SimulateTransactionWithOpts(ctx, tx, &rpc.SimulateTransactionOpts{
		SigVerify:  false,
		Commitment: rpc.CommitmentProcessed,
	})
	if err != nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => %w", err)
	}
	if simulation == nil || simulation.Value == nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => empty result")
	}
	if simulation.Value.Err != nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => %v, logs: %v", simulation.Value.Err, simulation.Value.Logs)
	}
	if simulation.Value.UnitsConsumed == nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => units consumed is missing")
	}
	if *simulation.Value.UnitsConsumed == 0 {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => units consumed is zero")
	}
	if simulation.Value.LoadedAccountsDataSize == nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => loaded accounts data size is missing")
	}
	if *simulation.Value.LoadedAccountsDataSize == 0 {
		return solana.TransactionConfig{}, fmt.Errorf("solana.SimulateTransaction() => loaded accounts data size is zero")
	}
	computeUnitLimit := getComputeUnitLimit(*simulation.Value.UnitsConsumed)
	loadedAccountsDataSizeLimit := getLoadedAccountsDataSizeLimit(*simulation.Value.LoadedAccountsDataSize)
	writableAccounts, err := tx.Message.Writable()
	if err != nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.Message.Writable() => %w", err)
	}
	recentFees, err := c.RPCGetRecentPrioritizationFees(ctx, writableAccounts)
	if err != nil {
		return solana.TransactionConfig{}, fmt.Errorf("solana.GetRecentPrioritizationFees() => %w", err)
	}
	microLamportsPerCU := getMedianPriorityFee(recentFees)
	priorityFee := getTotalPriorityFee(microLamportsPerCU, computeUnitLimit)

	return solana.TransactionConfig{}.
		WithComputeUnitLimit(computeUnitLimit).
		WithLoadedAccountsDataSizeLimit(loadedAccountsDataSizeLimit).
		WithPriorityFee(priorityFee), nil
}

func (c *Client) configureV1Transaction(ctx context.Context, tx *solana.Transaction) error {
	config, err := c.getV1TransactionConfig(ctx, tx)
	if err != nil {
		return err
	}
	tx.Message.TransactionConfig = config
	return nil
}

func (c *Client) configureV1TransactionBuilder(ctx context.Context, builder *solana.TransactionBuilder) error {
	// Computer's offline replay tests compare transactions with historical
	// legacy messages and synthetic nonce state. Preserve those message bytes;
	// TestCreateV1 covers v1 compilation and RPC-backed resource estimation.
	if common.CheckTestEnvironment(ctx) {
		return nil
	}

	builder.SetTransactionConfig(provisionalV1TransactionConfig())
	preview, err := builder.Build()
	if err != nil {
		return err
	}
	config, err := c.getV1TransactionConfig(ctx, preview)
	if err != nil {
		return err
	}
	builder.SetTransactionConfig(config)
	return nil
}

func getTotalPriorityFee(microLamportsPerCU uint64, computeUnitLimit uint32) uint64 {
	fee := decimal.NewFromUint64(microLamportsPerCU).
		Mul(decimal.NewFromUint64(uint64(computeUnitLimit))).
		Div(decimal.NewFromInt(microLamportsPerLamport)).
		RoundCeil(0)
	if fee.GreaterThan(decimal.NewFromUint64(maxPriorityFeeLamports)) {
		return maxPriorityFeeLamports
	}
	return fee.BigInt().Uint64()
}

func getComputeUnitLimit(unitsConsumed uint64) uint32 {
	if unitsConsumed == 0 {
		return 0
	}
	if unitsConsumed >= uint64(maxComputeUnitLimit) {
		return maxComputeUnitLimit
	}
	units := (unitsConsumed*computeUnitMarginNumerator + computeUnitMarginDenominator - 1) /
		computeUnitMarginDenominator
	if units > uint64(maxComputeUnitLimit) {
		return maxComputeUnitLimit
	}
	return uint32(units)
}

func getLoadedAccountsDataSizeLimit(loadedAccountsDataSize uint32) uint32 {
	if loadedAccountsDataSize == 0 {
		return 0
	}
	if loadedAccountsDataSize >= maxLoadedAccountsDataSizeLimit {
		return maxLoadedAccountsDataSizeLimit
	}
	size := (uint64(loadedAccountsDataSize)*loadedAccountsDataSizeMarginNumerator + loadedAccountsDataSizeMarginDenominator - 1) /
		loadedAccountsDataSizeMarginDenominator
	if size > uint64(maxLoadedAccountsDataSizeLimit) {
		return maxLoadedAccountsDataSizeLimit
	}
	return uint32(size)
}

func getMedianPriorityFee(recentFees []rpc.PriorizationFeeResult) uint64 {
	if len(recentFees) == 0 {
		return defaultMicroLamportsPerCU
	}
	fees := make([]uint64, len(recentFees))
	for i, fee := range recentFees {
		fees[i] = fee.PrioritizationFee
	}
	slices.Sort(fees)
	middle := len(fees) / 2
	if len(fees)%2 == 1 {
		return fees[middle]
	}
	lower, upper := fees[middle-1], fees[middle]
	return lower + (upper-lower)/2
}

func ExtractTransfersFromTransaction(ctx context.Context, tx *solana.Transaction, meta *rpc.TransactionMeta, exception *solana.PublicKey) ([]*Transfer, error) {
	if meta.Err != nil {
		panic(fmt.Sprint(meta.Err))
	}

	hash := tx.Signatures[0].String()
	msg := tx.Message

	var (
		transfers         = []*Transfer{}
		innerInstructions = map[uint16][]solana.CompiledInstruction{}
		tokenAccounts     = map[solana.PublicKey]token.Account{}
		owners            = []*solana.PublicKey{}
	)

	for _, inner := range meta.InnerInstructions {
		sis := make([]solana.CompiledInstruction, len(inner.Instructions))
		for idx, ii := range inner.Instructions {
			sis[idx] = solana.CompiledInstruction{
				ProgramIDIndex: ii.ProgramIDIndex,
				Accounts:       ii.Accounts,
				Data:           ii.Data,
			}
		}
		innerInstructions[inner.Index] = sis
	}

	bs := meta.PreTokenBalances
	bs = append(bs, meta.PostTokenBalances...)
	for _, balance := range bs {
		if account, err := msg.Account(balance.AccountIndex); err == nil {
			tokenAccounts[account] = token.Account{
				Owner: *balance.Owner,
				Mint:  balance.Mint,
			}
			if !slices.ContainsFunc(owners, func(owner *solana.PublicKey) bool {
				return owner.Equals(*balance.Owner)
			}) {
				owners = append(owners, balance.Owner)
			}
		}
	}

	for index, ix := range msg.Instructions {
		if transfer := extractTransfersFromInstruction(&msg, ix, tokenAccounts, owners, transfers); transfer != nil {
			if exception != nil && exception.String() == transfer.Receiver {
				continue
			}
			transfer.Signature = hash
			transfer.Index = int64(index)
			transfers = append(transfers, transfer)
		}

		for innerIndex, inner := range innerInstructions[uint16(index)] {
			if transfer := extractTransfersFromInstruction(&msg, inner, tokenAccounts, owners, transfers); transfer != nil {
				if exception != nil && exception.String() == transfer.Receiver {
					continue
				}
				transfer.Signature = hash
				transfer.Index = (int64(index)+1)*solanaInnerIndexBase + int64(innerIndex)
				transfers = append(transfers, transfer)
			}
		}
	}

	return transfers, nil
}

func ExtractTransferFromTransactionByIndex(ctx context.Context, tx *solana.Transaction, meta *rpc.TransactionMeta, index int64) *Transfer {
	if meta.Err != nil {
		panic(fmt.Sprint(meta.Err))
	}
	msg := tx.Message

	var (
		innerInstructions = map[uint16][]solana.CompiledInstruction{}
		tokenAccounts     = map[solana.PublicKey]token.Account{}
		owners            = []*solana.PublicKey{}
	)

	for _, inner := range meta.InnerInstructions {
		sis := make([]solana.CompiledInstruction, len(inner.Instructions))
		for idx, ii := range inner.Instructions {
			sis[idx] = solana.CompiledInstruction{
				ProgramIDIndex: ii.ProgramIDIndex,
				Accounts:       ii.Accounts,
				Data:           ii.Data,
			}
		}
		innerInstructions[inner.Index] = sis
	}

	bs := meta.PreTokenBalances
	bs = append(bs, meta.PostTokenBalances...)
	for _, balance := range bs {
		if account, err := msg.Account(balance.AccountIndex); err == nil {
			tokenAccounts[account] = token.Account{
				Owner: *balance.Owner,
				Mint:  balance.Mint,
			}
			if !slices.ContainsFunc(owners, func(owner *solana.PublicKey) bool {
				return owner.Equals(*balance.Owner)
			}) {
				owners = append(owners, balance.Owner)
			}
		}
	}

	ix, ok := instructionByTransferIndex(&msg, innerInstructions, index)
	if !ok {
		return nil
	}
	return extractTransfersFromInstruction(&msg, ix, tokenAccounts, owners, nil)
}

func instructionByTransferIndex(msg *solana.Message, innerInstructions map[uint16][]solana.CompiledInstruction, index int64) (solana.CompiledInstruction, bool) {
	if index < 0 {
		return solana.CompiledInstruction{}, false
	}

	if index < solanaInnerIndexBase {
		if index >= int64(len(msg.Instructions)) {
			return solana.CompiledInstruction{}, false
		}
		return msg.Instructions[index], true
	}

	outerIndex := index/solanaInnerIndexBase - 1
	if outerIndex < 0 || outerIndex >= int64(len(msg.Instructions)) {
		return solana.CompiledInstruction{}, false
	}

	innerIndex := index % solanaInnerIndexBase
	inners := innerInstructions[uint16(outerIndex)]
	if innerIndex < 0 || innerIndex >= int64(len(inners)) {
		return solana.CompiledInstruction{}, false
	}
	return inners[innerIndex], true
}

func ExtractMintsFromTransaction(tx *solana.Transaction) []string {
	var assets []string
	for index, ix := range tx.Message.Instructions {
		if index == 0 {
			continue
		}
		programKey, err := tx.Message.Program(ix.ProgramIDIndex)
		if err != nil {
			panic(err)
		}
		accounts, err := ix.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			panic(err)
		}

		switch programKey {
		case solana.TokenProgramID, solana.Token2022ProgramID:
			if mint, ok := DecodeMintToken(accounts, ix.Data); ok {
				address := mint.GetMintAccount().PublicKey
				assets = append(assets, address.String())
				continue
			}
		}
	}
	return assets
}

func ExtractMemoFromTransaction(ctx context.Context, tx *solana.Transaction, meta *rpc.TransactionMeta, payer solana.PublicKey) string {
	if meta.Err != nil {
		panic(fmt.Sprint(meta.Err))
	}

	msg := tx.Message
	for _, ins := range msg.Instructions {
		programKey, err := msg.Program(ins.ProgramIDIndex)
		if err != nil {
			panic(err)
		}
		if !programKey.Equals(solana.MemoProgramID) {
			continue
		}
		accounts, err := ins.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			panic(err)
		}
		if memo, err := DecodeMemo(accounts, ins.Data); err == nil {
			signer := memo.GetSigner()
			if signer != nil && signer.PublicKey.Equals(payer) {
				return strings.TrimPrefix(string(memo.Message), "$")
			}
		}
	}

	return ""
}

func GetSignatureIndexOfAccount(tx solana.Transaction, publicKey solana.PublicKey) (int, error) {
	index, err := tx.GetAccountIndex(publicKey)
	if err == nil {
		return int(index), nil
	}
	if strings.Contains(err.Error(), "account not found") {
		return -1, nil
	}
	return -1, err
}

func BuildCreateAddressLookupTableInstruction(block *rpc.GetLatestBlockhashResult, payer sc.PublicKey) (CustomInstruction, string) {
	slot := block.Context.Slot
	lookupTablePubkey, bumpSeed := address_lookup_table.DeriveLookupTableAddress(
		payer,
		slot,
	)
	return CustomInstruction{
		Instruction: address_lookup_table.CreateLookupTable(address_lookup_table.CreateLookupTableParams{
			LookupTable: lookupTablePubkey,
			Authority:   payer,
			Payer:       payer,
			RecentSlot:  slot,
			BumpSeed:    bumpSeed,
		}),
	}, lookupTablePubkey.ToBase58()
}
