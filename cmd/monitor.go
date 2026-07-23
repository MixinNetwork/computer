package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/MixinNetwork/bot-api-go-client/v3"
	"github.com/MixinNetwork/computer/apps/solana"
	computer "github.com/MixinNetwork/computer/solana"
	"github.com/MixinNetwork/computer/store"
	mc "github.com/MixinNetwork/mixin/common"
	"github.com/MixinNetwork/mixin/logger"
	"github.com/MixinNetwork/safe/common"
	"github.com/MixinNetwork/safe/mtg"
	"github.com/fox-one/mixin-sdk-go/v3"
	"github.com/fox-one/mixin-sdk-go/v3/mixinnet"
	"github.com/shopspring/decimal"
)

type UserStore interface {
	ReadProperty(ctx context.Context, k string) (string, error)
	WriteProperty(ctx context.Context, k, v string) error
}

func MonitorComputer(ctx context.Context, node *computer.Node, mixin *mixin.Client, mdb *mtg.SQLite3Store, store *store.SQLite3Store, conf *computer.Configuration, group *mtg.Group, conversationId, version string) {
	logger.Printf("MonitorComputer(%s, %s)", group.GenesisId(), conversationId)
	startedAt := time.Now()

	app := conf.MTG.App
	conv, err := bot.ConversationShow(ctx, conversationId, &bot.SafeUser{
		UserId:            app.AppId,
		SessionId:         app.SessionId,
		SessionPrivateKey: app.SessionPrivateKey,
	})
	if err != nil {
		panic(err)
	}

	for {
		time.Sleep(1 * time.Minute)
		msg, err := bundleComputerState(ctx, node, mixin, mdb, store, conf, group, startedAt, version)
		if err != nil {
			logger.Verbosef("Monitor.bundleComputerState() => %v", err)
			continue
		}
		postMessages(ctx, store, conv, conf.MTG, msg, conf.ObserverId)
		time.Sleep(3 * time.Minute)
	}
}

func bundleComputerState(ctx context.Context, node *computer.Node, mixin *mixin.Client, mdb *mtg.SQLite3Store, db *store.SQLite3Store, conf *computer.Configuration, grp *mtg.Group, startedAt time.Time, version string) (string, error) {
	state := "🧱🧱🧱🧱🧱 Computer 🧱🧱🧱🧱🧱\n"
	state = state + fmt.Sprintf("⏲️ Run time: %s\n", time.Since(startedAt).String())
	state = state + fmt.Sprintf("⏲️ Group: %s 𝕋%d\n", mixinnet.HashMembers(grp.GetMembers())[:16], grp.GetThreshold())

	state = state + "\n𝗠𝙏𝗚\n"
	req, err := db.ReadLatestRequest(ctx)
	if err != nil {
		return "", err
	} else if req != nil {
		state = state + fmt.Sprintf("🎆 Latest request: %x\n", req.MixinHash[:8])
	}

	tl, _, err := mdb.ListTransactions(ctx, mtg.TransactionStateInitial, 1000)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🫰 Initial Transactions: %d\n", len(tl))
	tl, _, err = mdb.ListTransactions(ctx, mtg.TransactionStateSigned, 1000)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🫰 Signed Transactions: %d\n", len(tl))
	tl, _, err = mdb.ListTransactions(ctx, mtg.TransactionStateSnapshot, 1000)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🫰 Snapshot Transactions: %d\n", len(tl))
	tl, err = mdb.ListConfirmedWithdrawalTransactionsAfter(ctx, time.Time{}, 1000)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🫰 Withdrawal Transactions: %d\n", len(tl))
	state = state + fmt.Sprintf("🫰 Solana Deposit Entry: %s\n", node.SolanaDepositEntry())

	state = state + "\n𝗔𝙋𝗣\n"
	uc, err := db.CountUsers(ctx)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🔑 Registered Users: %d\n", uc)
	tc, err := db.CountUserSystemCallByState(ctx, common.RequestStateInitial)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("💷 Initial Transactions: %d\n", tc)
	tc, err = db.CountUserSystemCallByState(ctx, common.RequestStatePending)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("💶 Pending Transactions: %d\n", tc)
	tc, err = db.CountUserSystemCallByState(ctx, common.RequestStateDone)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("💵 Done Transactions: %d\n", tc)
	tc, err = db.CountUserSystemCallByState(ctx, common.RequestStateFailed)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("💸 Failed Transactions: %d\n", tc)

	state = state + "\nSigner\n"
	ss, err := db.SessionsState(ctx)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("🔑 Initial Sessions: %d\n", ss.Initial)
	state = state + fmt.Sprintf("🔑 Pending Sessions: %d\n", ss.Pending)
	state = state + fmt.Sprintf("🔑 Final Sessions: %d\n", ss.Done)
	offset := node.ReadPropertyAsTime(ctx, store.MPCMessageTimeKey)
	state = state + fmt.Sprintf("🔑 Processed MPC Message: %s\n", offset)

	state = state + "\nBalances\n"
	_, c, err := common.SafeAssetBalance(ctx, mixin, []string{conf.MTG.App.AppId}, 1, conf.AssetId)
	if err != nil {
		return "", err
	}
	state = state + fmt.Sprintf("💍 MSST outputs: %d\n", c)
	if conf.MTG.App.AppId == conf.ObserverId {
		xinBalance, c, err := common.SafeAssetBalance(ctx, mixin, []string{conf.MTG.App.AppId}, 1, mtg.StorageAssetId)
		if err != nil {
			return "", err
		}
		state = state + fmt.Sprintf("💍 XIN outputs: %d, total: %s XIN\n", c, xinBalance.String())

		solBalance, c, err := common.SafeAssetBalance(ctx, mixin, []string{conf.MTG.App.AppId}, 1, common.SafeSolanaChainId)
		if err != nil {
			return "", err
		}
		state = state + fmt.Sprintf("💍 SOL outputs: %d, total: %s SOL\n", c, solBalance.String())

		c, err = node.CountSpareNonceAccounts(ctx)
		if err != nil {
			return "", err
		}
		state = state + fmt.Sprintf("💍 Spare Nonce Accounts: %d\n", c)

		balance, err := node.GetPayerBalance(ctx)
		if err != nil {
			return "", err
		}
		b := decimal.NewFromUint64(balance).Div(decimal.New(1, solana.SolanaDecimal)).String()
		state = state + fmt.Sprintf("💍 Payer %s Balance: %s SOL\n", node.SolanaPayer(), b)

		limit := mc.NewIntegerFromString("0.5")
		if node.Prod() && (xinBalance.Cmp(limit) < 0 || solBalance.Cmp(limit) < 0 || mc.NewIntegerFromString(b).Cmp(limit) < 0) {
			state = state + "@40518661\n"
		}

		state = state + "\nObserver\n"
		_, am, err := db.ListPendingBurnSystemCalls(ctx)
		state = state + fmt.Sprintf("💶 Pending Burn Calls: %d\n", len(am))
	}

	state = state + fmt.Sprintf("\n🦷 Binary version: %s", version)
	return state, nil
}

func postMessages(ctx context.Context, store UserStore, conv *bot.Conversation, conf *mtg.Configuration, msg, observer string) {
	app := conf.App
	var messages []*bot.MessageRequest
	for i := range conv.Participants {
		s := conv.Participants[i]
		if s.UserId == app.AppId {
			continue
		}
		u, err := fetchConversationUser(ctx, store, s.UserId, conf)
		if err != nil || checkBot(u, observer) {
			logger.Verbosef("Monitor.fetchConversationUser(%s) => %v %v", s.UserId, u, err)
			continue
		}
		messages = append(messages, &bot.MessageRequest{
			ConversationId: conv.ConversationId,
			RecipientId:    s.UserId,
			Category:       bot.MessageCategoryPlainText,
			MessageId:      common.UniqueId(msg, s.UserId),
			DataBase64:     base64.RawURLEncoding.EncodeToString([]byte(msg)),
		})
	}
	err := bot.PostMessages(ctx, messages, &bot.SafeUser{
		UserId:            app.AppId,
		SessionId:         app.SessionId,
		SessionPrivateKey: app.SessionPrivateKey,
	})
	logger.Verbosef("Monitor.PostMessages(\n%s) => %d %v", msg, len(messages), err)
}

func fetchConversationUser(ctx context.Context, store UserStore, id string, conf *mtg.Configuration) (*bot.User, error) {
	app := conf.App
	key := fmt.Sprintf("MONITOR:USER:%s", id)
	val, err := store.ReadProperty(ctx, key)
	if err != nil {
		return nil, err
	}
	if val != "" {
		var u bot.User
		err = json.Unmarshal([]byte(val), &u)
		return &u, err
	}

	u, err := bot.GetUser(ctx, id, &bot.SafeUser{
		UserId:            app.AppId,
		SessionId:         app.SessionId,
		SessionPrivateKey: app.SessionPrivateKey,
	})
	if err != nil || u == nil {
		return nil, err
	}
	val = string(common.MarshalJSONOrPanic(u))
	err = store.WriteProperty(ctx, key, val)
	return u, err
}

func checkBot(u *bot.User, observer string) bool {
	if u.UserId == observer {
		return false
	}
	id, err := strconv.ParseInt(u.IdentityNumber, 10, 64)
	if err != nil {
		panic(u.IdentityNumber)
	}
	return id > 7000000000
}
