package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	bot "github.com/MixinNetwork/bot-api-go-client/v3"
	solanaApp "github.com/MixinNetwork/computer/apps/solana"
	"github.com/MixinNetwork/mixin/crypto"
	"github.com/MixinNetwork/safe/common"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/system"
	"github.com/gagliardetto/solana-go/programs/token"
)

const (
	recoverySystemCallMigrationKey = "SCHEMA:VERSION:RECOVER_SYSTEM_CALL_E7B90D0A"

	failedSystemCallID  = "e7b90d0a-639f-3ce4-9129-9c7c1f82f663"
	recoveryUserID      = "281474976710860"
	recoverySource      = "9Qji1YiSqDho92vdquZRQmB4A5mCDHDboaWFkMvvQtLX"
	recoveryDestination = "72x3zjRxDMdpMDtzzHRTKrf9Hu6unWqfhHGbAf2N4LpK"

	recoveryDestinationMember    = "fcb87491-4fa0-4c2f-b387-262b63cbc112"
	recoveryDestinationThreshold = byte(1)

	// Finalized balance at Solana slot 441551757.
	recoverySOLAmount        uint64 = 1_514_340_633
	maxSolanaTransactionSize uint64 = 1232
)

type recoveryTokenBalance struct {
	Mint   string
	Amount uint64
}

type recoveryTransactionContext struct {
	payer        solana.PublicKey
	nonceAddress solana.PublicKey
	nonceHash    solana.Hash
}

// Finalized SPL Token balances at Solana slot 441551796. The destination's
// associated token accounts for all these mints already exist.
var recoveryTokenBalances = []recoveryTokenBalance{
	{Mint: "BEDmzMzBR6wxxpMjbDoHZfWKdacrTx3hhxBGiar3KWCP", Amount: 14_697_319_383},
	{Mint: "Bun5Nx21e73Z76ob89RJ1HeTWRRHfB3fxDHtCvBy2Yuf", Amount: 52_342_329_118},
	{Mint: "CvJmpFKM3jT7o3K5yumJNB1ffmz19XWGZXT4KtmDB6x8", Amount: 69_675_121},
	{Mint: "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v", Amount: 31_120_304_441},
	{Mint: "FYobXhwqEt7nFangy6bb31nqGiRCVx16owSmFZfkjrKv", Amount: 42_601_731},
	{Mint: "FtUCRrJPCAXs3hJksduogAPbdjUH4PekdjyAHEjU6izr", Amount: 492_424_174_101},
	{Mint: "GELF6RkNYmFgaMaFkFkorDspRsVaorgX8Zv8G4cVeKSZ", Amount: 20_105_656},
	{Mint: "Gfg8Nzf5YPCccEPBrV9eJgqUot74ZWi1RiikAaxx492t", Amount: 8_490_647_688},
	{Mint: "HBiHPHC6JFCE3cfCrERiFnJUVTzGErhZm9RmsTtzDJUb", Amount: 8_138_341_214_327},
	{Mint: "HhotoxePjdLjXohpZNNjPH5NirVNhZenEh7UzR1V2DCV", Amount: 8_942_378_267_971},
}

func (s *SQLite3Store) Migrate(ctx context.Context, observer bool) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	applied, err := migrationApplied(ctx, s.db, recoverySystemCallMigrationKey)
	if err != nil || applied {
		return err
	}

	original, err := s.ReadSystemCallByRequestId(ctx, failedSystemCallID, common.RequestStatePending)
	if err != nil {
		return err
	}
	if original == nil {
		return fmt.Errorf("required recovery system call not found: %s", failedSystemCallID)
	}
	if original.Type != CallTypeDeposit {
		return fmt.Errorf("invalid recovery system call type: %s", original.Type)
	}
	if original.Signature.Valid {
		return fmt.Errorf("recovery system call already has a signature")
	}

	sourceUser, err := s.ReadUserByChainAddress(ctx, recoverySource)
	if err != nil {
		return err
	}
	if sourceUser == nil || sourceUser.UserId != recoveryUserID {
		return fmt.Errorf("invalid recovery source user: %v", sourceUser)
	}

	destinationUser, err := s.ReadUserByChainAddress(ctx, recoveryDestination)
	if err != nil {
		return err
	}
	if destinationUser == nil {
		return fmt.Errorf("recovery destination is not a Computer user: %s", recoveryDestination)
	}
	if err := validateRecoveryDestinationUser(destinationUser); err != nil {
		return err
	}

	txContext, err := recoveryTransactionContextFromOriginal(original)
	if err != nil {
		return err
	}

	var nonce *NonceAccount
	if observer {
		nonce, err = s.ReadNonceAccount(ctx, original.NonceAccount)
		if err != nil {
			return err
		}
		if nonce == nil {
			return fmt.Errorf("recovery nonce account not found: %s", original.NonceAccount)
		}
		if nonce.Hash != txContext.nonceHash.String() {
			return fmt.Errorf("original and stored nonce hashes do not match")
		}
		if nonce.CallId.Valid && nonce.CallId.String != original.RequestId {
			return fmt.Errorf("recovery nonce account is occupied by %s", nonce.CallId.String)
		}
	}

	recovery, err := buildRecoverySystemCall(original, sourceUser, destinationUser, txContext)
	if err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer common.Rollback(tx)

	err = s.execOne(ctx, tx, "UPDATE system_calls SET state=? WHERE id=? AND state=?",
		common.RequestStateFailed, original.RequestId, common.RequestStatePending)
	if err != nil {
		return fmt.Errorf("SQLite3Store UPDATE system_calls %v", err)
	}

	err = s.writeSystemCall(ctx, tx, recovery)
	if err != nil {
		return err
	}

	if observer {
		result, err := tx.ExecContext(ctx, `UPDATE nonce_accounts
			SET mix=?, call_id=?, updated_at=?
			WHERE address=? AND hash=? AND (call_id IS NULL OR call_id=?)`,
			sourceUser.MixAddress, recovery.RequestId, recovery.CreatedAt,
			nonce.Address, nonce.Hash, original.RequestId,
		)
		if err != nil {
			return fmt.Errorf("SQLite3Store UPDATE nonce_accounts %v", err)
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return fmt.Errorf("failed to occupy recovery nonce account: %d %v", rows, err)
		}
	}

	err = insertMigrationProperty(ctx, tx, recoverySystemCallMigrationKey, recovery.RequestId, recovery.CreatedAt)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func validateRecoveryDestinationUser(user *User) error {
	mix, err := bot.NewMixAddressFromString(user.MixAddress)
	if err != nil {
		return fmt.Errorf("invalid recovery destination mix address: %w", err)
	}
	members := mix.Members()
	if mix.Threshold != recoveryDestinationThreshold ||
		len(members) != 1 || members[0] != recoveryDestinationMember {
		return fmt.Errorf("invalid recovery destination mix address: %v, %d", members, mix.Threshold)
	}
	return nil
}

func recoveryTransactionContextFromOriginal(original *SystemCall) (*recoveryTransactionContext, error) {
	originalTx, err := solana.TransactionFromBase64(original.Raw)
	if err != nil {
		return nil, fmt.Errorf("invalid original system call transaction: %w", err)
	}
	if len(originalTx.Message.AccountKeys) == 0 || len(originalTx.Message.Instructions) == 0 {
		return nil, fmt.Errorf("invalid original system call transaction")
	}

	advance, err := solanaApp.NonceAccountFromTx(originalTx)
	if err != nil {
		return nil, fmt.Errorf("invalid original nonce instruction: %w", err)
	}
	payer := originalTx.Message.AccountKeys[0]
	if advance.GetNonceAccount().PublicKey.String() != original.NonceAccount {
		return nil, fmt.Errorf("original nonce account mismatch")
	}
	if advance.GetNonceAuthorityAccount().PublicKey != payer {
		return nil, fmt.Errorf("original nonce authority is not the fee payer")
	}

	return &recoveryTransactionContext{
		payer:        payer,
		nonceAddress: advance.GetNonceAccount().PublicKey,
		nonceHash:    originalTx.Message.RecentBlockhash,
	}, nil
}

func buildRecoverySystemCall(original *SystemCall, sourceUser, destinationUser *User, txContext *recoveryTransactionContext) (*SystemCall, error) {
	payer := txContext.payer

	source, err := solana.PublicKeyFromBase58(sourceUser.ChainAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid recovery source: %w", err)
	}
	destination, err := solana.PublicKeyFromBase58(destinationUser.ChainAddress)
	if err != nil {
		return nil, fmt.Errorf("invalid recovery destination: %w", err)
	}
	if source == destination {
		return nil, fmt.Errorf("recovery source and destination are identical")
	}

	instructions := []solana.Instruction{
		system.NewAdvanceNonceAccountInstruction(
			txContext.nonceAddress,
			solana.SysVarRecentBlockHashesPubkey,
			payer,
		).Build(),
	}
	for _, balance := range recoveryTokenBalances {
		mint, err := solana.PublicKeyFromBase58(balance.Mint)
		if err != nil {
			return nil, fmt.Errorf("invalid recovery token mint %s: %w", balance.Mint, err)
		}
		sourceATA := solanaApp.FindAssociatedTokenAddress(source, mint, solana.TokenProgramID)
		destinationATA := solanaApp.FindAssociatedTokenAddress(destination, mint, solana.TokenProgramID)
		instructions = append(instructions, token.NewTransferInstruction(
			balance.Amount,
			sourceATA,
			destinationATA,
			source,
			nil,
		).Build())
	}
	instructions = append(instructions, system.NewTransferInstruction(
		recoverySOLAmount,
		source,
		destination,
	).Build())

	recoveryTx, err := solana.NewTransaction(instructions, txContext.nonceHash, solana.TransactionPayer(payer))
	if err != nil {
		return nil, fmt.Errorf("build recovery transaction: %w", err)
	}
	raw, err := recoveryTx.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal recovery transaction: %w", err)
	}
	if uint64(len(raw)) > maxSolanaTransactionSize {
		return nil, fmt.Errorf("recovery transaction is too large: %d", len(raw))
	}
	message, err := recoveryTx.Message.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshal recovery transaction message: %w", err)
	}

	createdAt := original.CreatedAt
	return &SystemCall{
		RequestId:       common.UniqueId(original.RequestId, "recover all balances"),
		Superior:        common.UniqueId(original.RequestId, "recover all balances"),
		RequestHash:     original.RequestHash,
		Type:            CallTypeMain,
		NonceAccount:    txContext.nonceAddress.String(),
		Public:          hex.EncodeToString(sourceUser.FingerprintWithPath()),
		SkipPostProcess: false,
		MessageHash:     crypto.Sha256Hash(message).String(),
		Raw:             base64.StdEncoding.EncodeToString(raw),
		State:           common.RequestStatePending,
		WithdrawalTraces: sql.NullString{
			Valid:  true,
			String: "",
		},
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}, nil
}

type migrationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func migrationApplied(ctx context.Context, q migrationQueryer, key string) (bool, error) {
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM properties WHERE key=?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func insertMigrationProperty(ctx context.Context, tx *sql.Tx, key, value string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO properties (key, value, created_at, updated_at) VALUES (?, ?, ?, ?)",
		key, value, now, now,
	)
	return err
}
