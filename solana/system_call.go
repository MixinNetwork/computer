package solana

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/computer/store"
	mc "github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/apps/ethereum"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
	tokenAta "github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gofrs/uuid/v5"
	"github.com/shopspring/decimal"
)

type ReferencedTxAsset struct {
	Solana  bool
	Amount  decimal.Decimal
	Decimal int
	Address string
	AssetId string
	ChainId string
	Fee     bool
}

func systemCallReferenceOutputStateValue(state int64) byte {
	// Failed system calls may be replayed after compaction; the first attempt has
	// already moved their references from pending to done.
	if state == common.RequestStateFailed {
		return common.RequestStateDone
	}
	return common.RequestStatePending
}

// should only return error when mtg could not find outputs from referenced transaction
// all assets needed in system call should be referenced
// extra amount of XIN is used for fees in system call like rent
func (node *Node) GetSystemCallReferenceOutputs(ctx context.Context, uid, requestHash string, state byte) ([]*store.UserOutput, *crypto.Hash, error) {
	var outputs []*store.UserOutput
	req, err := node.store.ReadRequestByHash(ctx, requestHash)
	if err != nil || req == nil {
		panic(fmt.Errorf("store.ReadRequestByHash(%s) => %v %v", requestHash, req, err))
	}
	ver, err := node.group.ReadKernelTransactionUntilSufficient(ctx, req.MixinHash.String())
	if err != nil || ver == nil {
		panic(fmt.Errorf("group.ReadKernelTransactionUntilSufficient(%s) => %v %v", req.MixinHash.String(), ver, err))
	}
	if common.CheckTestEnvironment(ctx) {
		ver.References = readOutputReferences(req.Id)
	}

	var storage *crypto.Hash
	for _, ref := range ver.References {
		// The subsequent entry for system call should reference the previous entry for system call
		// to ensure the order when create multiple system calls with invoice.
		// Thus the output from previous entry for system call should be skipped.
		request, err := node.store.ReadRequestByHash(ctx, ref.String())
		if err != nil {
			return nil, nil, err
		}
		if request != nil && request.Action == OperationTypeSystemCall {
			continue
		}

		os, hash, err := node.getSystemCallReferenceTx(ctx, uid, ref.String(), state)
		if err != nil {
			return nil, nil, err
		}
		if len(os) > 0 {
			outputs = append(outputs, os...)
		}
		if hash == nil {
			continue
		}
		if storage == nil {
			storage = hash
		} else if storage.String() != hash.String() {
			return nil, nil, fmt.Errorf("multiple storage references: %s != %s", storage.String(), hash.String())
		}
	}
	return outputs, storage, nil
}

func (node *Node) getSystemCallReferenceTx(ctx context.Context, uid, hash string, state byte) ([]*store.UserOutput, *crypto.Hash, error) {
	ver, err := node.group.ReadKernelTransactionUntilSufficient(ctx, hash)
	if err != nil || ver == nil {
		panic(fmt.Errorf("group.ReadKernelTransactionUntilSufficient(%s) => %v %v", hash, ver, err))
	}
	if common.CheckTestEnvironment(ctx) {
		value, err := node.store.ReadProperty(ctx, hash)
		if err != nil {
			panic(err)
		}
		if len(value) > 0 {
			switch hash {
			case "2e9e56113ac650ebe865762d99b4c80ba83d06b24db1083915af4bc07f0720cd",
				"a8eed784060b200ea7f417309b12a33ced8344c24f5cdbe0237b7fc06125f459",
				"01c43005fd06e0b8f06a0af04faf7530331603e352a11032afd0fd9dbd84e8ee":
				raw := common.DecodeHexOrPanic(value)
				ver, err = mc.UnmarshalVersionedTransaction(raw)
				if err != nil {
					panic(err)
				}
			default:
				extra, err := base64.RawURLEncoding.DecodeString(value)
				if err != nil {
					panic(err)
				}
				ver.Extra = extra
			}
		}
	}
	// skip referenced storage transaction
	if ver.Asset.String() == common.XINKernelAssetId && len(ver.Extra) > mc.ExtraSizeGeneralLimit {
		h, _ := crypto.HashFromString(hash)
		return nil, &h, nil
	}

	asset, err := common.SafeReadAssetUntilSufficient(ctx, ver.Asset.String())
	if err != nil {
		panic(err)
	}
	outputs, err := node.store.ListUserOutputsByHashAndState(ctx, uid, hash, state)
	if err != nil {
		panic(err)
	}
	if len(outputs) == 0 {
		return nil, nil, fmt.Errorf("unreceived reference %s", hash)
	}
	for _, o := range outputs {
		o.Asset = *asset
	}
	return outputs, nil, nil
}

// be used to refund by mtg without fee
// be used to create prepare call by observer with fee from payer (isolatedFee = true)
// be used to create post call by observer with fee to calculate rest SOL
func (node *Node) GetSystemCallRelatedAsset(ctx context.Context, os []*store.UserOutput) []*ReferencedTxAsset {
	assets := aggregateSystemCallReferenceAssets(os)
	for _, asset := range assets {
		if asset.Solana {
			continue
		}
		deployed, err := node.store.ReadDeployedAsset(ctx, asset.AssetId)
		if err != nil || deployed == nil {
			panic(fmt.Errorf("store.ReadDeployedAsset(%s) => %v %v", asset.AssetId, deployed, err))
		}
		asset.Address = deployed.Address
		asset.Decimal = solanaApp.AssetDecimal
	}
	return assets
}

// Aggregate asset identity and amount without requiring a Solana mint mapping.
// GetSystemCallRelatedAsset fills that mapping for Solana transactions, while
// failed calls can use the aggregate directly for Mixin refunds.
func aggregateSystemCallReferenceAssets(os []*store.UserOutput) []*ReferencedTxAsset {
	am := make(map[string]*ReferencedTxAsset)
	for _, output := range os {
		logger.Printf("node.GetReferencedTxAsset() => %v", output)
		amt := decimal.RequireFromString(output.Amount)
		isSolAsset := output.ChainId == solanaApp.SolanaChainBase
		address := output.Asset.AssetKey
		decimal := output.Asset.Precision
		if !isSolAsset {
			address = ""
			decimal = solanaApp.AssetDecimal
		}
		ra := &ReferencedTxAsset{
			Solana:  isSolAsset,
			Address: address,
			Decimal: decimal,
			Amount:  amt,
			AssetId: output.AssetId,
			ChainId: output.Asset.ChainID,
			Fee:     false,
		}
		if old := am[output.AssetId]; old != nil {
			ra.Amount = ra.Amount.Add(old.Amount)
		}
		am[output.AssetId] = ra
	}
	var assets []*ReferencedTxAsset
	for _, a := range am {
		logger.Printf("node.GetSystemCallRelatedAsset() => %v", a)
		if !a.Amount.IsPositive() {
			panic(a.AssetId)
		}
		assets = append(assets, a)
	}
	// Callers persist the ordered transactions and select the first asset with
	// insufficient balance for compaction, so map iteration order is unsafe here.
	slices.SortFunc(assets, func(a, b *ReferencedTxAsset) int {
		return strings.Compare(a.AssetId, b.AssetId)
	})
	return assets
}

func (node *Node) validateSystemCallParameters(ctx context.Context, req *store.Request, os []*store.UserOutput) error {
	if req == nil {
		return fmt.Errorf("missing system call request")
	}
	_, err := node.readSystemCallFeeInfo(ctx, req)
	if err != nil {
		return err
	}
	return node.validateSystemCallReferencedAssets(ctx, os)
}

func (node *Node) validateSystemCallReferencedAssets(ctx context.Context, os []*store.UserOutput) error {
	checked := make(map[string]bool)
	for _, output := range os {
		if output.ChainId == solanaApp.SolanaChainBase || checked[output.AssetId] {
			continue
		}
		checked[output.AssetId] = true
		deployed, err := node.store.ReadDeployedAsset(ctx, output.AssetId)
		if err != nil {
			panic(fmt.Errorf("store.ReadDeployedAsset(%s) => %v", output.AssetId, err))
		}
		if deployed == nil {
			return fmt.Errorf("external asset is not deployed: %s", output.AssetId)
		}
	}
	return nil
}

func (node *Node) readSystemCallFeeInfo(ctx context.Context, req *store.Request) (*store.FeeInfo, error) {
	extra := req.ExtraBytes()
	switch len(extra) {
	case 25:
		return nil, nil
	case 41:
	default:
		return nil, fmt.Errorf("invalid system call extra length: %d", len(extra))
	}

	feeId := uuid.Must(uuid.FromBytes(extra[25:])).String()
	fee, err := node.store.ReadFeeInfoById(ctx, feeId)
	logger.Printf("store.ReadFeeInfoById(%s) => %v %v", feeId, fee, err)
	if err != nil {
		panic(fmt.Errorf("store.ReadFeeInfoById(%s) => %v", feeId, err))
	}
	if fee == nil { // TODO check fee timestamp against the call timestamp not too old
		return nil, fmt.Errorf("invalid fee id: %s", feeId)
	}
	return fee, nil
}

// should only return error when no valid fees found
func (node *Node) getSystemCallFeeFromXIN(ctx context.Context, call *store.SystemCall) (*store.UserOutput, error) {
	req, err := node.store.ReadRequestByHash(ctx, call.RequestHash)
	if err != nil || req == nil {
		panic(fmt.Errorf("store.ReadRequestByHash(%s) => %v %v", call.RequestHash, req, err))
	}
	fee, err := node.readSystemCallFeeInfo(ctx, req)
	if err != nil {
		return nil, err
	} else if fee == nil {
		return nil, nil
	}

	ratio := decimal.RequireFromString(fee.Ratio)
	plan, err := node.store.ReadLatestOperationParams(ctx, req.CreatedAt)
	if err != nil {
		panic(err)
	}

	if req.Amount.Compare(plan.OperationPriceAmount) <= 0 {
		return nil, nil
	}
	feeOnXIN := req.Amount.Sub(plan.OperationPriceAmount)
	feeOnSol := feeOnXIN.Mul(ratio).RoundCeil(8).String()

	asset, err := common.SafeReadAssetUntilSufficient(ctx, common.SafeSolanaChainId)
	if err != nil {
		panic(err)
	}

	return &store.UserOutput{
		OutputId:        req.Id,
		UserId:          call.UserIdFromPublicPath(),
		TransactionHash: req.MixinHash.String(),
		OutputIndex:     req.MixinIndex,
		AssetId:         common.SafeSolanaChainId,
		ChainId:         common.SafeSolanaChainId,
		Amount:          feeOnSol,
		State:           common.RequestStateInitial,
		CreatedAt:       req.CreatedAt,
		UpdatedAt:       req.CreatedAt,

		Asset: *asset,
	}, nil
}

func (node *Node) getPostProcessCall(ctx context.Context, req *store.Request, flag byte, call *store.SystemCall, data []byte) (*store.SystemCall, error) {
	if len(data) == 0 {
		return nil, nil
	}
	switch call.Type {
	case store.CallTypeMain, store.CallTypePrepare:
	default:
		return nil, nil
	}

	post, tx, err := node.getSubSystemCallFromExtra(ctx, req, data)
	if err != nil || post == nil {
		return nil, err
	}
	post.Superior = call.Superior
	post.Type = store.CallTypePostProcess
	post.Public = call.Public
	post.State = common.RequestStatePending

	main := call
	if call.Type == store.CallTypePrepare {
		main, err = node.store.ReadSystemCallByRequestId(ctx, call.Superior, 0)
		logger.Printf("store.ReadSystemCallByRequestId(%s) => %v %v", call.Superior, main, err)
		if err != nil || main == nil {
			panic(err)
		}
	}
	user, err := node.store.ReadUser(ctx, main.UserIdFromPublicPath())
	if err != nil {
		panic(err)
	}
	if user == nil {
		return nil, fmt.Errorf("store.ReadUser(%s) => nil", main.UserIdFromPublicPath())
	}
	mtgDeposit := solana.MustPublicKeyFromBase58(node.conf.SolanaDepositEntry)
	authority := node.getUserSolanaPublicKeyFromCall(ctx, post)
	if call.Type == store.CallTypePrepare {
		authority = node.getMTGAddress(ctx)
	}
	err = node.VerifySubSystemCallEnvelope(tx, authority, true)
	logger.Printf("node.VerifySubSystemCallEnvelope(%s) => %v", post.RequestId, err)
	if err != nil {
		return nil, err
	}
	err = node.VerifySubSystemCall(ctx, tx, mtgDeposit, solana.MustPublicKeyFromBase58(user.ChainAddress))
	logger.Printf("node.VerifySubSystemCall(%s) => %v", user.ChainAddress, err)
	if err != nil {
		return nil, err
	}

	os, _, err := node.GetSystemCallReferenceOutputs(ctx, main.UserIdFromPublicPath(), main.RequestHash, systemCallReferenceOutputStateValue(main.State))
	if err != nil {
		panic(fmt.Errorf("node.GetSystemCallReferenceTxs(%s) => %v", main.RequestId, err))
	}
	ras := node.GetSystemCallRelatedAsset(ctx, os)

	switch flag {
	case FlagConfirmCallSuccess:
		err = node.comparePostCallWithSolanaTx(ctx, ras, tx, main.Hash.String, user.ChainAddress)
		logger.Printf("node.comparePostCallWithSolanaTx(%s %s) => %v", main.Hash.String, user.ChainAddress, err)
		if err != nil {
			return nil, err
		}
	case FlagConfirmCallFail:
		err = node.verifyFailedPostProcessCall(ctx, call, main, post, tx)
		logger.Printf("node.verifyFailedPostProcessCall(%s) => %v", call.RequestId, err)
		if err != nil {
			return nil, err
		}
	}
	return post, nil
}

func (node *Node) verifyFailedPostProcessCall(ctx context.Context, call, main, post *store.SystemCall, actual *solana.Transaction) error {
	nonce := &store.NonceAccount{
		Address: post.NonceAccount,
		Hash:    actual.Message.RecentBlockhash.String(),
	}
	var expected *solana.Transaction
	switch call.Type {
	case store.CallTypeMain:
		expected = node.CreatePostProcessTransaction(ctx, main, nonce, nil, nil)
	case store.CallTypePrepare:
		expected = node.CreateRefundWithdrawalTransaction(ctx, call, main, nonce)
	default:
		return fmt.Errorf("invalid failed post-process superior type: %s", call.Type)
	}
	if expected == nil {
		return fmt.Errorf("unexpected failed post-process transaction")
	}
	return compareCleanupTransactions(actual, expected)
}

func compareCleanupTransactions(actual, expected *solana.Transaction) error {
	if len(actual.Message.AccountKeys) == 0 || len(expected.Message.AccountKeys) == 0 {
		return fmt.Errorf("cleanup transaction has no fee payer")
	}
	if actual.Message.AccountKeys[0] != expected.Message.AccountKeys[0] {
		return fmt.Errorf("invalid cleanup fee payer: %s", actual.Message.AccountKeys[0])
	}
	if !slices.Equal(actual.Message.Signers(), expected.Message.Signers()) {
		return fmt.Errorf("invalid cleanup signers: %v", actual.Message.Signers())
	}
	if len(actual.Message.Instructions) != len(expected.Message.Instructions) {
		return fmt.Errorf("invalid cleanup instruction count: %d %d", len(actual.Message.Instructions), len(expected.Message.Instructions))
	}

	for i, actualIx := range actual.Message.Instructions {
		expectedIx := expected.Message.Instructions[i]
		actualProgram, err := actual.Message.Program(actualIx.ProgramIDIndex)
		if err != nil {
			return fmt.Errorf("resolve cleanup program %d: %w", i, err)
		}
		expectedProgram, err := expected.Message.Program(expectedIx.ProgramIDIndex)
		if err != nil {
			return fmt.Errorf("resolve expected cleanup program %d: %w", i, err)
		}
		if actualProgram != expectedProgram {
			return fmt.Errorf("invalid cleanup program %d: %s", i, actualProgram)
		}

		actualAccounts, err := actualIx.ResolveInstructionAccounts(&actual.Message)
		if err != nil {
			return fmt.Errorf("resolve cleanup accounts %d: %w", i, err)
		}
		expectedAccounts, err := expectedIx.ResolveInstructionAccounts(&expected.Message)
		if err != nil {
			return fmt.Errorf("resolve expected cleanup accounts %d: %w", i, err)
		}
		if len(actualAccounts) != len(expectedAccounts) {
			return fmt.Errorf("invalid cleanup account count %d: %d %d", i, len(actualAccounts), len(expectedAccounts))
		}
		for j := range actualAccounts {
			a, e := actualAccounts[j], expectedAccounts[j]
			if a.PublicKey != e.PublicKey || a.IsSigner != e.IsSigner || a.IsWritable != e.IsWritable {
				return fmt.Errorf("invalid cleanup account %d:%d: %s", i, j, a.PublicKey)
			}
		}

		if i == 1 && actualProgram == solana.ComputeBudget {
			if len(actualIx.Data) != 9 || len(expectedIx.Data) != 9 || actualIx.Data[0] != expectedIx.Data[0] {
				return fmt.Errorf("invalid compute budget instruction")
			}
			continue
		}
		if !bytes.Equal(actualIx.Data, expectedIx.Data) {
			return fmt.Errorf("invalid cleanup instruction data: %d", i)
		}
	}
	return nil
}

func (node *Node) getSubSystemCallFromExtra(ctx context.Context, req *store.Request, data []byte) (*store.SystemCall, *solana.Transaction, error) {
	if len(data) < 16 {
		return nil, nil, nil
	}
	id, raw := uuid.Must(uuid.FromBytes(data[:16])).String(), data[16:]
	return node.buildSystemCallFromBytes(ctx, req, id, raw, true)
}

// Returns validation errors for oversized transactions, unresolved address
// lookups, or malformed nonce-advance instructions. The returned call omits
// superior, type, public, and skip_postprocess fields.
func (node *Node) buildSystemCallFromBytes(ctx context.Context, req *store.Request, id string, raw []byte, withdrawn bool) (*store.SystemCall, *solana.Transaction, error) {
	tx, err := solana.TransactionFromBytes(raw)
	logger.Printf("solana.TransactionFromBytes(%x) => %v %v", raw, tx, err)
	if err != nil {
		return nil, nil, err
	}
	err = solanaApp.ValidateTransactionSize(tx)
	if err != nil {
		logger.Printf("solana.ValidateTransactionSize(%s %s) => %v", req.Id, id, err)
		return nil, nil, err
	}
	err = node.processTransactionWithAddressLookups(ctx, tx)
	if err != nil {
		if errors.Is(err, errInvalidAddressLookup) {
			return nil, nil, err
		}
		panic(err)
	}
	advance, err := solanaApp.NonceAccountFromTx(tx)
	logger.Printf("solana.NonceAccountFromTx() => %v %v", advance, err)
	if err != nil {
		return nil, nil, err
	}
	msg, err := tx.Message.MarshalBinary()
	if err != nil {
		panic(err)
	}
	call := &store.SystemCall{
		RequestId:       id,
		RequestHash:     req.MixinHash.String(),
		NonceAccount:    advance.GetNonceAccount().PublicKey.String(),
		MessageHash:     crypto.Sha256Hash(msg).String(),
		Raw:             tx.MustToBase64(),
		State:           common.RequestStateInitial,
		CreatedAt:       req.CreatedAt,
		UpdatedAt:       req.CreatedAt,
		RequestSignerAt: sql.NullTime{Valid: true, Time: req.CreatedAt},
	}
	if withdrawn {
		call.WithdrawalTraces = sql.NullString{Valid: true, String: ""}
	}
	return call, tx, nil
}

func (node *Node) checkUserSystemCall(ctx context.Context, tx *solana.Transaction) error {
	if common.CheckTestEnvironment(ctx) {
		return nil
	}

	// ensure the transaction is signed by fee payer
	if !tx.IsSigner(node.SolanaPayer()) {
		return fmt.Errorf("tx.IsSigner(payer) => %t", false)
	}

	// make sure fee payer is only used for the first nonce advance transaction
	index, err := solanaApp.GetSignatureIndexOfAccount(*tx, node.SolanaPayer())
	if err != nil {
		return err
	}
	for i, ins := range tx.Message.Instructions[1:] {
		if slices.Contains(ins.Accounts, uint16(index)) {
			return fmt.Errorf("invalid instruction: %d %v", i+1, ins)
		}
	}
	return nil
}

type prepareMovement struct {
	Kind         string
	TokenAddress string
	Source       string
	Destination  string
}

func (m prepareMovement) key() string {
	return strings.Join([]string{m.Kind, m.TokenAddress, m.Source, m.Destination}, ":")
}

func (node *Node) comparePrepareCallWithSolanaTx(tx *solana.Transaction, as []*ReferencedTxAsset, mtg, user solana.PublicKey) error {
	expected := make(map[string]*big.Int)
	for _, a := range as {
		amount := a.Amount.Mul(decimal.New(1, int32(a.Decimal))).BigInt()
		if amount.Sign() <= 0 {
			return fmt.Errorf("invalid prepare asset amount: %s %s", a.AssetId, a.Amount.String())
		}

		var movement prepareMovement
		switch {
		case a.Solana && a.AssetId == a.ChainId:
			source := mtg.String()
			if a.Fee {
				source = node.SolanaPayer().String()
			}
			movement = prepareMovement{
				Kind:         "system-transfer",
				TokenAddress: a.Address,
				Source:       source,
				Destination:  user.String(),
			}
		case a.Solana:
			movement = prepareMovement{
				Kind:         "token-transfer",
				TokenAddress: a.Address,
				Source:       mtg.String(),
				Destination:  user.String(),
			}
		default:
			movement = prepareMovement{
				Kind:         "token-mint",
				TokenAddress: a.Address,
				Destination:  user.String(),
			}
		}

		addPrepareMovement(expected, movement, amount)
	}

	actual := make(map[string]*big.Int)
	for index, ix := range tx.Message.Instructions {
		programKey, err := tx.Message.Program(ix.ProgramIDIndex)
		if err != nil {
			panic(err)
		}
		accounts, err := ix.ResolveInstructionAccounts(&tx.Message)
		if err != nil {
			panic(err)
		}

		switch programKey {
		case system.ProgramID:
			transfer, ok := solanaApp.DecodeSystemTransfer(accounts, ix.Data)
			if !ok {
				continue
			}
			recipient := transfer.GetRecipientAccount().PublicKey
			if !recipient.Equals(user) {
				return fmt.Errorf("invalid prepare SOL recipient: %s", recipient.String())
			}
			movement := prepareMovement{
				Kind:         "system-transfer",
				TokenAddress: solanaApp.SolanaEmptyAddress,
				Source:       transfer.GetFundingAccount().PublicKey.String(),
				Destination:  user.String(),
			}
			addPrepareMovement(actual, movement, new(big.Int).SetUint64(*transfer.Lamports))
		case solana.TokenProgramID, solana.Token2022ProgramID:
			if transfer, ok := solanaApp.DecodeTokenTransferChecked(accounts, ix.Data); ok {
				mint := transfer.GetMintAccount().PublicKey
				recipient := transfer.GetDestinationAccount().PublicKey
				userAta := solanaApp.FindAssociatedTokenAddress(user, mint, programKey)
				if !recipient.Equals(userAta) {
					return fmt.Errorf("invalid prepare token recipient: %s", recipient.String())
				}
				movement := prepareMovement{
					Kind:         "token-transfer",
					TokenAddress: mint.String(),
					Source:       transfer.GetOwnerAccount().PublicKey.String(),
					Destination:  user.String(),
				}
				addPrepareMovement(actual, movement, new(big.Int).SetUint64(*transfer.Amount))
				continue
			}
			if mint, ok := solanaApp.DecodeTokenMintTo(accounts, ix.Data); ok {
				token := mint.GetMintAccount().PublicKey
				recipient := mint.GetDestinationAccount().PublicKey
				userAta := solanaApp.FindAssociatedTokenAddress(user, token, programKey)
				if !recipient.Equals(userAta) {
					return fmt.Errorf("invalid prepare mint recipient: %s", recipient.String())
				}
				movement := prepareMovement{
					Kind:         "token-mint",
					TokenAddress: token.String(),
					Destination:  user.String(),
				}
				addPrepareMovement(actual, movement, new(big.Int).SetUint64(*mint.Amount))
				continue
			}
		case tokenAta.ProgramID, solana.ComputeBudget, solana.MemoProgramID:
			continue
		default:
			return fmt.Errorf("invalid prepare instruction %d program: %s", index, programKey.String())
		}
	}

	for key, amount := range actual {
		want := expected[key]
		if want == nil {
			return fmt.Errorf("unexpected prepare asset movement: %s %s", key, amount.String())
		}
		if want.Cmp(amount) != 0 {
			return fmt.Errorf("invalid prepare asset amount: %s %s %s", key, amount.String(), want.String())
		}
	}
	for key, want := range expected {
		got := actual[key]
		if got == nil {
			return fmt.Errorf("missing prepare asset movement: %s %s", key, want.String())
		}
	}
	return nil
}

func addPrepareMovement(m map[string]*big.Int, movement prepareMovement, amount *big.Int) {
	key := movement.key()
	if m[key] == nil {
		m[key] = new(big.Int)
	}
	m[key].Add(m[key], amount)
}

func (node *Node) comparePostCallWithSolanaTx(ctx context.Context, as []*ReferencedTxAsset, tx *solana.Transaction, signature, user string) error {
	rpcTx, err := node.RPCGetTransaction(ctx, signature)
	if err != nil || rpcTx == nil {
		panic(fmt.Errorf("solana.RPCGetTransaction(%s) => %v %v", signature, rpcTx, err))
	}
	if rpcTx.Meta.Err != nil {
		return fmt.Errorf("node.RPCGetTransaction(%s) => %s", signature, formatTransactionError(rpcTx.Meta.Err))
	}
	utx, err := rpcTx.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	err = node.processTransactionWithAddressLookups(ctx, utx)
	if err != nil {
		panic(err)
	}

	expectedTransfers := make(map[string]*ReferencedTxAsset)
	expectedBurns := make(map[string]*ReferencedTxAsset)
	for _, a := range as {
		// Keep Solana transfers and external-asset burns in separate ledgers.
		// A mixed burn+transfer for the same mint must not satisfy one expected amount.
		if a.Solana {
			addExpectedSystemCallMovement(expectedTransfers, a)
		} else {
			addExpectedSystemCallMovement(expectedBurns, a)
		}
	}
	cs := node.buildUserBalanceChangesFromMeta(ctx, utx, rpcTx.Meta, solana.MPK(user))
	for address, change := range cs {
		if old := expectedTransfers[address]; old != nil {
			old.Amount = old.Amount.Add(change.Amount)
			continue
		}
		if old := expectedBurns[address]; old != nil {
			old.Amount = old.Amount.Add(change.Amount)
			continue
		}
		if !change.Amount.IsPositive() {
			continue
		}
		asset := &ReferencedTxAsset{
			Solana:  true,
			Address: address,
			Decimal: int(change.Decimals),
			Amount:  change.Amount,
			AssetId: solanaApp.SolanaChainBase,
			ChainId: solanaApp.SolanaChainBase,
		}
		if address != solanaApp.SolanaEmptyAddress {
			asset.AssetId = ethereum.BuildChainAssetId(solanaApp.SolanaChainBase, address)

			da, err := node.store.ReadDeployedAssetByAddress(ctx, address)
			if err != nil {
				panic(fmt.Errorf("store.ReadDeployedAssetByAddress(%s) => %v", address, err))
			}
			if da != nil {
				asset.Solana = false
				asset.AssetId = da.AssetId
				asset.ChainId = da.ChainId
			}
		}
		if asset.Solana {
			addExpectedSystemCallMovement(expectedTransfers, asset)
		} else {
			addExpectedSystemCallMovement(expectedBurns, asset)
		}
	}

	actualTransfers, actualBurns := buildInitialAssetMovementMaps(tx, "", node.SolanaPayer().String())
	// Compare each instruction class independently so one class cannot make up
	// for a shortfall in the other.
	err = node.comparePostProcessMovements(ctx, solanaApp.InitialAssetMovementTransfer, expectedTransfers, actualTransfers)
	if err != nil {
		return err
	}
	err = node.comparePostProcessMovements(ctx, solanaApp.InitialAssetMovementBurn, expectedBurns, actualBurns)
	if err != nil {
		return err
	}
	return nil
}

func (node *Node) compareDepositCallWithSolanaTx(ctx context.Context, tx *solana.Transaction, signature, user string) error {
	rpcTx, err := node.RPCGetTransaction(ctx, signature)
	if err != nil || rpcTx == nil {
		panic(fmt.Errorf("solana.RPCGetTransaction(%s) => %v %v", signature, rpcTx, err))
	}
	if rpcTx.Meta.Err != nil {
		return fmt.Errorf("node.RPCGetTransaction(%s) => %s", signature, formatTransactionError(rpcTx.Meta.Err))
	}
	dtx, err := rpcTx.Transaction.GetTransaction()
	if err != nil {
		panic(err)
	}
	err = node.processTransactionWithAddressLookups(ctx, dtx)
	if err != nil {
		panic(err)
	}
	transfers, err := solanaApp.ExtractTransfersFromTransaction(ctx, dtx, rpcTx.Meta, nil)
	if err != nil {
		panic(err)
	}
	expectedChanges, err := node.parseSolanaBlockBalanceChanges(ctx, transfers)
	if err != nil {
		panic(err)
	}

	actualTransfers, actualBurns := buildInitialAssetMovementMaps(tx, user, "")
	// Deposits are generated per receiver, while expectedChanges is keyed as
	// receiver:mint. Filter to this user before splitting transfer and burn.
	expectedTransfers, expectedBurns := node.collectExpectedDepositMovements(ctx, expectedChanges, user)
	err = node.compareDepositMovements(ctx, signature, tx, solanaApp.InitialAssetMovementTransfer, expectedTransfers, actualTransfers)
	if err != nil {
		return err
	}
	err = node.compareDepositMovements(ctx, signature, tx, solanaApp.InitialAssetMovementBurn, expectedBurns, actualBurns)
	if err != nil {
		return err
	}
	return nil
}

func (node *Node) collectExpectedDepositMovements(ctx context.Context, expectedChanges map[string]*big.Int, user string) (map[string]*big.Int, map[string]*big.Int) {
	expectedTransfers := make(map[string]*big.Int)
	expectedBurns := make(map[string]*big.Int)
	changes := filterExpectedDepositChangesByUser(expectedChanges, user)
	for address, expected := range changes {
		if node.shouldSkipExpectedDepositMovement(ctx, address, expected) {
			continue
		}

		expectedMap := expectedTransfers
		if node.isDeployedAssetAddress(ctx, address) {
			expectedMap = expectedBurns
		}
		expectedMap[address] = addBigInt(expectedMap[address], expected)
	}
	return expectedTransfers, expectedBurns
}

func filterExpectedDepositChangesByUser(expectedChanges map[string]*big.Int, user string) map[string]*big.Int {
	changes := make(map[string]*big.Int)
	for key, expected := range expectedChanges {
		receiver, address, ok := strings.Cut(key, ":")
		if !ok || receiver != user {
			continue
		}
		changes[address] = addBigInt(changes[address], expected)
	}
	return changes
}

func (node *Node) shouldSkipExpectedDepositMovement(ctx context.Context, address string, expected *big.Int) bool {
	if address == solanaApp.SolanaEmptyAddress && expected.Uint64() < 10 {
		return true
	}
	isNFT, err := node.RPCCheckNFT(ctx, address)
	if err != nil {
		panic(fmt.Errorf("node.RPCCheckNFT(%s) => %v", address, err))
	}
	return isNFT
}

func addExpectedSystemCallMovement(m map[string]*ReferencedTxAsset, asset *ReferencedTxAsset) {
	old := m[asset.Address]
	if old != nil {
		old.Amount = old.Amount.Add(asset.Amount)
		return
	}
	copy := *asset
	m[asset.Address] = &copy
}

func buildInitialAssetMovementMaps(tx *solana.Transaction, includeSender, excludeSender string) (map[string]*solanaApp.InitialAssetMovement, map[string]*solanaApp.InitialAssetMovement) {
	transfers := make(map[string]*solanaApp.InitialAssetMovement)
	burns := make(map[string]*solanaApp.InitialAssetMovement)
	for _, movement := range solanaApp.ExtractInitialAssetMovements(tx) {
		if includeSender != "" && movement.Sender != includeSender {
			continue
		}
		if excludeSender != "" && movement.Sender == excludeSender {
			continue
		}
		target := transfers
		if movement.Kind == solanaApp.InitialAssetMovementBurn {
			target = burns
		}
		old := target[movement.TokenAddress]
		if old == nil {
			copy := *movement
			copy.Value = new(big.Int).Set(movement.Value)
			target[movement.TokenAddress] = &copy
			continue
		}
		old.Value = addBigInt(old.Value, movement.Value)
	}
	return transfers, burns
}

func (node *Node) comparePostProcessMovements(ctx context.Context, kind solanaApp.InitialAssetMovementKind, expected map[string]*ReferencedTxAsset, actual map[string]*solanaApp.InitialAssetMovement) error {
	for address, asset := range expected {
		if node.shouldSkipPostProcessMovement(ctx, address, asset.Amount) {
			continue
		}
		expectedValue := asset.Amount.Mul(decimal.New(1, int32(asset.Decimal))).BigInt()
		movement := actual[address]
		if movement == nil {
			return fmt.Errorf("missing %s user balance change: %s", kind, address)
		}
		if movement.Value.Cmp(expectedValue) != 0 {
			actualAmount := decimal.NewFromBigInt(movement.Value, -int32(movement.Decimal))
			return fmt.Errorf("invalid %s user balance change: %s %s %s", kind, address, actualAmount.String(), asset.Amount.String())
		}
	}
	for address, movement := range actual {
		amount := decimal.NewFromBigInt(movement.Value, -int32(movement.Decimal))
		if node.shouldSkipPostProcessMovement(ctx, address, amount) {
			continue
		}
		if expected[address] == nil {
			return fmt.Errorf("unexpected %s user balance change: %s %s", kind, address, amount.String())
		}
	}
	return nil
}

func (node *Node) shouldSkipPostProcessMovement(ctx context.Context, address string, amount decimal.Decimal) bool {
	dust := decimal.RequireFromString("0.00000001")
	if amount.Cmp(dust) < 0 {
		return true
	}

	isNFT, err := node.RPCCheckNFT(ctx, address)
	if err != nil {
		panic(fmt.Errorf("node.RPCCheckNFT(%s) => %v", address, err))
	}
	return isNFT
}

func (node *Node) compareDepositMovements(ctx context.Context, signature string, tx *solana.Transaction, kind solanaApp.InitialAssetMovementKind, expected map[string]*big.Int, actual map[string]*solanaApp.InitialAssetMovement) error {
	for address, amount := range expected {
		movement := actual[address]
		if movement == nil {
			return fmt.Errorf("non-existed %s deposit: %s %s %s", kind, signature, address, tx.MustToBase64())
		}
		if movement.Value.Cmp(amount) != 0 {
			return fmt.Errorf("invalid %s deposit: %s %s %s %s %s", kind, signature, address, amount.String(), movement.Value.String(), tx.MustToBase64())
		}
	}
	for address, movement := range actual {
		if expected[address] == nil {
			if node.shouldSkipDepositMovement(ctx, address, movement.Value) {
				continue
			}
			return fmt.Errorf("unexpected %s deposit: %s %s %s %s", kind, signature, address, movement.Value.String(), tx.MustToBase64())
		}
	}
	return nil
}

func (node *Node) shouldSkipDepositMovement(ctx context.Context, address string, value *big.Int) bool {
	if address == solanaApp.SolanaEmptyAddress && value.Uint64() < 10 {
		return true
	}
	isNFT, err := node.RPCCheckNFT(ctx, address)
	if err != nil {
		panic(fmt.Errorf("node.RPCCheckNFT(%s) => %v", address, err))
	}
	return isNFT
}

func (node *Node) isDeployedAssetAddress(ctx context.Context, address string) bool {
	if address == solanaApp.SolanaEmptyAddress {
		return false
	}
	da, err := node.store.ReadDeployedAssetByAddress(ctx, address)
	if err != nil {
		panic(fmt.Errorf("store.ReadDeployedAssetByAddress(%s) => %v", address, err))
	}
	return da != nil
}

func addBigInt(a, b *big.Int) *big.Int {
	if a == nil {
		return new(big.Int).Set(b)
	}
	return new(big.Int).Add(a, b)
}

func attachSystemCall(extra []byte, cid string, raw []byte) []byte {
	extra = append(extra, uuid.Must(uuid.FromString(cid)).Bytes()...)
	extra = append(extra, raw...)
	return extra
}
