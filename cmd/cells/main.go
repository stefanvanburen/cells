package main

import (
	"context"
	"fmt"
	"os"

	"github.com/pressly/cli"
	"github.com/stefanvanburen/cells/internal/lsp"
)

func main() {
	root := &cli.Command{
		Name:      "cells",
		ShortHelp: "A language server for CEL (Common Expression Language)",
		SubCommands: []*cli.Command{
			{
				Name:      "serve",
				ShortHelp: "Start the CEL language server (communicates over stdin/stdout)",
				Exec: func(ctx context.Context, s *cli.State) error {
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
