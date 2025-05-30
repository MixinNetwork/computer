package config

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"

	computer "github.com/MixinNetwork/computer/solana"
	"github.com/pelletier/go-toml"
)

type Configuration struct {
	Computer *computer.Configuration `toml:"computer"`
	Dev      *DevConfig              `toml:"dev"`
}

func ReadConfiguration(path, role string) (*Configuration, error) {
	if strings.HasPrefix(path, "~/") {
		usr, _ := user.Current()
		path = filepath.Join(usr.HomeDir, (path)[2:])
	}
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var conf Configuration
	err = toml.Unmarshal(f, &conf)
	if err != nil {
		return nil, err
	}
	handleDevConfig(conf.Dev)
	return &conf, nil
}
