package main

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/MixinNetwork/computer/cmd"
	"github.com/urfave/cli/v2"
)

//go:embed README.md
var README string

//go:embed VERSION
var VERSION string

func main() {
	VERSION = strings.TrimSpace(VERSION)
	if strings.Contains(VERSION, "COMMIT") {
		panic("please build the application using make command.")
	}
	app := &cli.App{
		Name:                 "safe",
		Usage:                "Mixin Safe",
		Version:              VERSION,
		EnableBashCompletion: true,
		Metadata: map[string]any{
			"README":  README,
			"VERSION": VERSION,
		},
		Commands: []*cli.Command{
			{
				Name:   "computer",
				Usage:  "Run the computer node",
				Action: cmd.ComputerBootCmd,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "config",
						Aliases: []string{"c"},
						Value:   "~/.mixin/safe/config.toml",
						Usage:   "The configuration file path",
					},
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		fmt.Println(err)
	}
}
