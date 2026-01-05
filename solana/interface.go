package solana

import (
	"context"

	"github.com/MixinNetwork/computer/store"
	"github.com/MixinNetwork/safe/messenger"
	"github.com/MixinNetwork/safe/mtg"
)

type Config struct {
	Computer *Configuration `toml:"computer"`
	Dev      *DevConfig     `toml:"dev"`
}

type DevConfig struct {
	ProfilePort int    `toml:"profile-port"`
	LogLevel    int    `toml:"log-level"`
	Network     string `toml:"network"`
}

type Configuration struct {
	AppId                   string             `toml:"app-id"`
	StoreDir                string             `toml:"store-dir"`
	MessengerConversationId string             `toml:"messenger-conversation-id"`
	MonitorConversationId   string             `toml:"monitor-conversation-id"`
	Timestamp               int64              `toml:"timestamp"`
	Threshold               int                `toml:"threshold"`
	AssetId                 string             `toml:"asset-id"`
	ObserverId              string             `toml:"observer-id"`
	ObserverPublicKey       string             `toml:"observer-spend-public-key"`
	OperationPriceAssetId   string             `toml:"operation-price-asset-id"`
	OperationPriceAmount    string             `toml:"operation-price-amount"`
	MPCKeyNumber            int                `toml:"mpc-key-number"`
	MixinMessengerAPI       string             `toml:"mixin-messenger-api"`
	MixinRPC                string             `toml:"mixin-rpc"`
	SolanaRPC               string             `toml:"solana-rpc"`
	SolanaKey               string             `toml:"solana-key"`
	SolanaDepositEntry      string             `toml:"solana-deposit-entry"`
	IsSpendKeyCanonical     bool               `toml:"is-spend-key-canonical"`
	MTG                     *mtg.Configuration `toml:"mtg"`
}

func (c *Configuration) Messenger() *messenger.MixinConfiguration {
	return &messenger.MixinConfiguration{
		UserId:         c.MTG.App.AppId,
		SessionId:      c.MTG.App.SessionId,
		Key:            c.MTG.App.SessionPrivateKey,
		ConversationId: c.MessengerConversationId,
		ReceiveBuffer:  128,
		SendBuffer:     64,
	}
}

type Network interface {
	ReceiveMessage(context.Context) (*messenger.MixinMessage, error)
	QueueMessage(ctx context.Context, receiver string, b []byte) error
}

func OpenSQLite3Store(path string) (*store.SQLite3Store, error) {
	return store.OpenSQLite3Store(path)
}
