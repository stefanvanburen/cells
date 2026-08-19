package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/pressly/cli"
	"go.vanburen.xyz/cells/internal/lsp"
	"znkr.io/diff/textdiff"
)

func main() {
	root := &cli.Command{
		Name:    "cells",
		Summary: "A language server for CEL (Common Expression Language)",
		SubCommands: []*cli.Command{
			serveCommand(),
			formatCommand(),
			checkCommand(),
			hoverCommand(),
			referencesCommand(),
			renameCommand(),
		},
	}
	if err := cli.ParseAndRun(context.Background(), root, os.Args[1:], nil); err != nil {
		if exitErr, ok := errors.AsType[*exitError](err); ok {
			os.Exit(exitErr.code)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// exitError is returned by commands that want to exit with a specific code
// without printing an additional error message.
type exitError struct {
	code int
}

func (e *exitError) Error() string { return "" }

func serveCommand() *cli.Command {
	return &cli.Command{
		Name:    "serve",
		Summary: "Start the CEL language server (communicates over stdin/stdout)",
		Exec: func(_ context.Context, _ *cli.State) error {
			return lsp.Serve()
		},
	}
}

func formatCommand() *cli.Command {
	return &cli.Command{
		Name:    "format",
		Summary: "Format CEL source files",
		Usage:   "cells format [--write] [--diff] [file...]",
		Flags: cli.FlagsFunc(func(f *flag.FlagSet) {
			f.Bool("write", false, "write result to source file instead of stdout")
			f.Bool("diff", false, "display diffs instead of rewriting files")
		}),
		FlagConfigs: []cli.FlagConfig{
			{Name: "write", Short: "w"},
			{Name: "diff", Short: "d"},
		},
		Exec: func(_ context.Context, s *cli.State) error {
			writeBack := cli.GetFlag[bool](s, "write")
			showDiff := cli.GetFlag[bool](s, "diff")

			if len(s.Args) == 0 {
				s.Args = []string{"-"}
			}

			exitCode := 0
			for _, filename := range s.Args {
				var content []byte
				var err error
				if filename == "-" {
					content, err = io.ReadAll(os.Stdin)
					if err != nil {
						fmt.Fprintf(os.Stderr, "<stdin>: %v\n", err)
						exitCode = 1
						continue
					}
				} else {
					content, err = os.ReadFile(filename)
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: %v\n", filename, err)
						exitCode = 1
						continue
					}
				}
				formatted, err := lsp.Format(string(content))
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				if showDiff {
					if formatted != string(content) {
						fmt.Print(diffUnified(filename, string(content), formatted))
						exitCode = 1
					}
				}
				if writeBack {
					if filename == "-" {
						fmt.Fprintf(os.Stderr, "format: cannot use --write with stdin\n")
						exitCode = 1
						continue
					}
					if formatted != string(content) {
						if err := os.WriteFile(filename, []byte(formatted), 0o644); err != nil {
							fmt.Fprintf(os.Stderr, "%s: %v\n", filename, err)
							exitCode = 1
						}
					}
				}
				if !showDiff && !writeBack {
					fmt.Print(formatted)
				}
			}
			if exitCode != 0 {
				return &exitError{exitCode}
			}
			return nil
		},
	}
}

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:    "check",
		Summary: "Check CEL source files for parse and type errors",
		Usage:   "cells check <file> [file...]",
		Exec: func(_ context.Context, s *cli.State) error {
			if len(s.Args) == 0 {
				return fmt.Errorf("check: no files specified")
			}
			exitCode := 0
			for _, filename := range s.Args {
				content, err := os.ReadFile(filename)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				diags, err := lsp.Check(string(content))
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				for _, d := range diags {
					fmt.Printf("%s:%d:%d: %s: %s\n", filename, d.Line, d.Col, d.Severity, d.Message)
					exitCode = 1
				}
			}
			if exitCode != 0 {
				return &exitError{exitCode}
			}
			return nil
		},
	}
}

func hoverCommand() *cli.Command {
	return &cli.Command{
		Name:    "hover",
		Summary: "Show documentation for the element at the given position",
		Usage:   "cells hover <file>:<line>:<col>",
		Exec: func(_ context.Context, s *cli.State) error {
			if len(s.Args) != 1 {
				return fmt.Errorf("hover: expected one argument of the form file:line:col")
			}
			filename, line, col, err := parsePosition(s.Args[0])
			if err != nil {
				return err
			}
			content, err := os.ReadFile(filename)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filename, err)
			}
			result, err := lsp.Hover(string(content), line, col)
			if err != nil {
				return fmt.Errorf("hover: %w", err)
			}
			if result != "" {
				fmt.Println(result)
			}
			return nil
		},
	}
}

func referencesCommand() *cli.Command {
	return &cli.Command{
		Name:    "references",
		Summary: "List all references to the element at the given position",
		Usage:   "cells references <file>:<line>:<col>",
		Exec: func(_ context.Context, s *cli.State) error {
			if len(s.Args) != 1 {
				return fmt.Errorf("references: expected one argument of the form file:line:col")
			}
			filename, line, col, err := parsePosition(s.Args[0])
			if err != nil {
				return err
			}
			content, err := os.ReadFile(filename)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filename, err)
			}
			refs, err := lsp.References(string(content), line, col)
			if err != nil {
				return fmt.Errorf("references: %w", err)
			}
			for _, ref := range refs {
				fmt.Printf("%s:%d:%d\n", filename, ref.Line, ref.Col)
			}
			return nil
		},
	}
}

func renameCommand() *cli.Command {
	return &cli.Command{
		Name:    "rename",
		Summary: "Rename the identifier at the given position",
		Usage:   "cells rename --new-name=<name> [--write] <file>:<line>:<col>",
		Flags: cli.FlagsFunc(func(f *flag.FlagSet) {
			f.String("new-name", "", "new name for the identifier")
			f.Bool("write", false, "write result to source file instead of stdout")
		}),
		FlagConfigs: []cli.FlagConfig{
			{Name: "new-name", Required: true},
			{Name: "write", Short: "w"},
		},
		Exec: func(_ context.Context, s *cli.State) error {
			newName := cli.GetFlag[string](s, "new-name")
			writeBack := cli.GetFlag[bool](s, "write")

			if len(s.Args) != 1 {
				return fmt.Errorf("rename: expected one argument of the form file:line:col")
			}
			filename, line, col, err := parsePosition(s.Args[0])
			if err != nil {
				return err
			}
			content, err := os.ReadFile(filename)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filename, err)
			}
			result, err := lsp.Rename(string(content), line, col, newName)
			if err != nil {
				return fmt.Errorf("rename: %w", err)
			}
			if writeBack {
				return os.WriteFile(filename, []byte(result), 0o644)
			}
			fmt.Print(result)
			return nil
		},
	}
}

// parsePosition parses an argument of the form "file:line:col" where line and col are 1-indexed.
// Columns are measured in UTF-8 bytes.
func parsePosition(arg string) (filename string, line, col int, err error) {
	rest, colStr, ok := strings.CutLast(arg, ":")
	if !ok {
		return "", 0, 0, fmt.Errorf("invalid position %q: expected file:line:col", arg)
	}
	filename, lineStr, ok := strings.CutLast(rest, ":")
	if !ok {
		return "", 0, 0, fmt.Errorf("invalid position %q: expected file:line:col", arg)
	}

	line, err = strconv.Atoi(lineStr)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid line number in %q: %w", arg, err)
	}
	col, err = strconv.Atoi(colStr)
	if err != nil {
		return "", 0, 0, fmt.Errorf("invalid column number in %q: %w", arg, err)
	}
	if line < 1 || col < 1 {
		return "", 0, 0, fmt.Errorf("line and column must be >= 1 in %q", arg)
	}
	return filename, line, col, nil
}

// diffUnified returns a unified diff of original vs formatted for the named file.
func diffUnified(filename, original, formatted string) string {
	d := textdiff.Unified(original, formatted)
	if d == "" {
		return ""
	}
	return fmt.Sprintf("--- %s\n+++ %s\n%s", filename, filename, d)
}
