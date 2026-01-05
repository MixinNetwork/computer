package config

import (
	"fmt"
	"net/http"
	_ "net/http/pprof"

	computer "github.com/MixinNetwork/computer/solana"
	"github.com/MixinNetwork/mixin/logger"
)

func handleDevConfig(c *computer.DevConfig) {
	logger.SetLevel(logger.INFO)
	if c == nil {
		return
	}
	logger.SetLevel(c.LogLevel)
	if c.ProfilePort > 1000 {
		l := fmt.Sprintf("127.0.0.1:%d", c.ProfilePort)
		go http.ListenAndServe(l, http.DefaultServeMux)
	}
	if c.Network == "" {
		c.Network = computer.MainNetworkName
	}
}
