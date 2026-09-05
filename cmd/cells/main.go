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
	"github.com/pressly/cli/flagtype"
	"go.vanburen.xyz/cells/internal/lsp"
	"znkr.io/diff/textdiff"
)

func main() {
	if err := cli.ParseAndRun(context.Background(), rootCommand(), os.Args[1:], nil); err != nil {
		if exitErr, ok := errors.AsType[*exitError](err); ok {
			os.Exit(exitErr.code)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// rootCommand builds the cells command tree. It is a function rather than a
// package-level value so that each run, including each test, gets its own
// flag sets.
func rootCommand() *cli.Command {
	return &cli.Command{
		Name:    "cells",
		Summary: "A language server for CEL (Common Expression Language)",
		Flags: cli.FlagsFunc(func(f *flag.FlagSet) {
			f.Var(flagtype.StringSlice(), "ext", "enable a CEL extension library (repeatable); one of: "+strings.Join(lsp.ExtensionNames(), ", "))
			f.String("config", "", "path to a CEL environment configuration declaring variables and types")
		}),
		SubCommands: []*cli.Command{
			serveCommand(),
			formatCommand(),
			checkCommand(),
			hoverCommand(),
			referencesCommand(),
			renameCommand(),
		},
	}
}

// options builds the CEL environment options from the flags every command
// shares. Extension names are validated here so that a misspelling is
// reported against the flag rather than from inside the environment.
func options(s *cli.State) (lsp.Options, error) {
	opts := lsp.Options{Extensions: cli.GetFlag[[]string](s, "ext")}
	if err := lsp.ValidateExtensions(opts.Extensions); err != nil {
		return lsp.Options{}, err
	}
	if path := cli.GetFlag[string](s, "config"); path != "" {
		config, err := lsp.LoadConfig(path)
		if err != nil {
			return lsp.Options{}, err
		}
		opts.Config = config
	}
	return opts, nil
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
		Exec: func(_ context.Context, s *cli.State) error {
			opts, err := options(s)
			if err != nil {
				return err
			}
			return lsp.Serve(opts)
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
			opts, err := options(s)
			if err != nil {
				return err
			}

			if len(s.Args) == 0 {
				s.Args = []string{"-"}
			}

			exitCode := 0
			for _, filename := range s.Args {
				var content []byte
				var err error
				if filename == "-" {
					content, err = io.ReadAll(s.Stdin)
					if err != nil {
						fmt.Fprintf(s.Stderr, "<stdin>: %v\n", err)
						exitCode = 1
						continue
					}
				} else {
					content, err = os.ReadFile(filename)
					if err != nil {
						fmt.Fprintf(s.Stderr, "%s: %v\n", filename, err)
						exitCode = 1
						continue
					}
				}
				formatted, err := lsp.Format(string(content), opts)
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				if showDiff {
					if formatted != string(content) {
						fmt.Fprint(s.Stdout, diffUnified(filename, string(content), formatted))
						exitCode = 1
					}
				}
				if writeBack {
					if filename == "-" {
						fmt.Fprintf(s.Stderr, "format: cannot use --write with stdin\n")
						exitCode = 1
						continue
					}
					if formatted != string(content) {
						if err := os.WriteFile(filename, []byte(formatted), 0o644); err != nil {
							fmt.Fprintf(s.Stderr, "%s: %v\n", filename, err)
							exitCode = 1
						}
					}
				}
				if !showDiff && !writeBack {
					fmt.Fprint(s.Stdout, formatted)
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
			opts, err := options(s)
			if err != nil {
				return err
			}
			if len(s.Args) == 0 {
				return fmt.Errorf("check: no files specified")
			}
			exitCode := 0
			for _, filename := range s.Args {
				content, err := os.ReadFile(filename)
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				diags, err := lsp.Check(string(content), opts)
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", filename, err)
					exitCode = 1
					continue
				}
				for _, d := range diags {
					fmt.Fprintf(s.Stdout, "%s:%d:%d: %s: %s\n", filename, d.Line, d.Col, d.Severity, d.Message)
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
			opts, err := options(s)
			if err != nil {
				return err
			}
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
			result, err := lsp.Hover(string(content), line, col, opts)
			if err != nil {
				return fmt.Errorf("hover: %w", err)
			}
			if result != "" {
				fmt.Fprintln(s.Stdout, result)
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
			opts, err := options(s)
			if err != nil {
				return err
			}
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
			refs, err := lsp.References(string(content), line, col, opts)
			if err != nil {
				return fmt.Errorf("references: %w", err)
			}
			for _, ref := range refs {
				fmt.Fprintf(s.Stdout, "%s:%d:%d\n", filename, ref.Line, ref.Col)
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
			opts, err := options(s)
			if err != nil {
				return err
			}

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
			result, err := lsp.Rename(string(content), line, col, newName, opts)
			if err != nil {
				return fmt.Errorf("rename: %w", err)
			}
			if writeBack {
				return os.WriteFile(filename, []byte(result), 0o644)
			}
			fmt.Fprint(s.Stdout, result)
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
