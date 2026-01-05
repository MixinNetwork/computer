package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"

	computer "github.com/MixinNetwork/computer/solana"
	"github.com/pelletier/go-toml"
)

func ReadConfiguration(path, role string) (*computer.Config, error) {
	if strings.HasPrefix(path, "~/") {
		usr, _ := user.Current()
		path = filepath.Join(usr.HomeDir, (path)[2:])
	}
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var conf computer.Config
	err = toml.Unmarshal(f, &conf)
	if err != nil {
		return nil, err
	}
	handleDevConfig(conf.Dev)
	return &conf, nil
}
