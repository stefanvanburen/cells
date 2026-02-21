package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pressly/cli"
	"github.com/stefanvanburen/cells/internal/lsp"
)

func main() {
	var root *cli.Command
	root = &cli.Command{
		Name:      "cells",
		ShortHelp: "A language server for CEL (Common Expression Language)",
		Exec: func(_ context.Context, _ *cli.State) error {
			fmt.Println(cli.DefaultUsage(root))
			return nil
		},
		SubCommands: []*cli.Command{
			{
				Name:      "serve",
				ShortHelp: "Start the CEL language server (communicates over stdin/stdout)",
				Exec: func(_ context.Context, _ *cli.State) error {
					return lsp.Serve()
				},
			},
		},
	}
	if err := cli.ParseAndRun(context.Background(), root, os.Args[1:], nil); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
