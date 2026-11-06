package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/pressly/cli"
	"github.com/pressly/cli/flagtype"
	"go.vanburen.xyz/cells/internal/lsp"
	"go.vanburen.xyz/cells/internal/version"
	"znkr.io/diff/textdiff"
)

// The exit codes follow diff and grep: 0 when there is nothing to report,
// exitFindings when the input has problems to report, and exitTrouble when
// cells could not do what it was asked, such as for a misused flag, an
// unreadable file, or a configuration that does not load.
const (
	exitFindings = 1
	exitTrouble  = 2
)

func main() {
	err := cli.ParseAndRun(context.Background(), rootCommand(), os.Args[1:], nil)
	os.Exit(exitCode(err, os.Stderr))
}

// exitCode returns the code to exit with after a run that returned err,
// reporting err on stderr unless it is an [*exitError].
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := errors.AsType[*exitError](err); ok {
		return exitErr.code
	}
	fmt.Fprintf(stderr, "error: %v\n", err)
	return exitTrouble
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
			f.Var(flagtype.StringSlice(), "descriptor-set", "path to an encoded FileDescriptorSet making its message types resolvable (repeatable)")
		}),
		SubCommands: []*cli.Command{
			serveCommand(),
			formatCommand(),
			checkCommand(),
			hoverCommand(),
			referencesCommand(),
			definitionCommand(),
			renameCommand(),
			versionCommand(),
		},
	}
}

// options builds the CEL environment options from the flags every command
// shares. Extension names are validated here so that a misspelling is reported
// against the flag rather than from inside the environment, and a named
// configuration is loaded here so that a bad one is reported once rather than
// once per file.
func options(s *cli.State) (lsp.Options, error) {
	opts := lsp.Options{
		Extensions:     s.GetFlag[[]string]("ext"),
		ConfigPath:     s.GetFlag[string]("config"),
		DescriptorSets: s.GetFlag[[]string]("descriptor-set"),
	}
	if err := lsp.ValidateExtensions(opts.Extensions); err != nil {
		return lsp.Options{}, err
	}
	if opts.ConfigPath != "" {
		if _, err := lsp.LoadConfig(opts.ConfigPath); err != nil {
			return lsp.Options{}, err
		}
	}
	return opts, nil
}

// optionsFor returns opts as they apply to the named file. Without an explicit
// --config, each file is checked against the nearest configuration above it,
// so that one command can span directories that are configured differently.
// filename may be "-", meaning stdin, which is searched for from the working
// directory.
func optionsFor(opts lsp.Options, filename string) lsp.Options {
	if opts.ConfigPath != "" {
		return opts
	}
	dir := "."
	if filename != "-" {
		dir = filepath.Dir(filename)
	}
	opts.ConfigPath = lsp.FindConfig(dir)
	return opts
}

// exitError is returned by commands that have already reported what went
// wrong, and only need to exit with code.
type exitError struct {
	code int
}

func (e *exitError) Error() string { return "" }

// stdinName is how input read from stdin is named in what cells reports.
const stdinName = "<stdin>"

// checkInputs reports a usage error when more than one of args names stdin,
// since only one of them could read it.
func checkInputs(command string, args []string) error {
	if i := slices.Index(args, "-"); i >= 0 && slices.Contains(args[i+1:], "-") {
		return fmt.Errorf("%s: stdin (-) can be named only once", command)
	}
	return nil
}

// readInput returns the content of the named file, or of stdin when filename
// is "-", along with the name to report it by.
func readInput(s *cli.State, filename string) (name, content string, err error) {
	if filename == "-" {
		b, err := io.ReadAll(s.Stdin)
		return stdinName, string(b), err
	}
	b, err := os.ReadFile(filename)
	return filename, string(b), err
}

// printDiagnostics writes each diagnostic as file:line:col: severity: message.
func printDiagnostics(w io.Writer, name string, diags []lsp.CheckDiagnostic) {
	for _, d := range diags {
		fmt.Fprintf(w, "%s:%d:%d: %s: %s\n", name, d.Line, d.Col, d.Severity, d.Message)
	}
}

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:    "version",
		Summary: "Show what this binary was built from",
		Exec: func(_ context.Context, s *cli.State) error {
			_, err := io.WriteString(s.Stdout, version.Detail())
			return err
		},
	}
}

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
			writeBack := s.GetFlag[bool]("write")
			showDiff := s.GetFlag[bool]("diff")
			opts, err := options(s)
			if err != nil {
				return err
			}

			if len(s.Args) == 0 {
				s.Args = []string{"-"}
			}
			if err := checkInputs("format", s.Args); err != nil {
				return err
			}
			if writeBack && slices.Contains(s.Args, "-") {
				return errors.New("format: cannot use --write with stdin")
			}

			code := 0
			for _, filename := range s.Args {
				name, content, err := readInput(s, filename)
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", name, err)
					code = max(code, exitTrouble)
					continue
				}
				formatted, err := lsp.Format(content, optionsFor(opts, filename))
				if parseErr, ok := errors.AsType[*lsp.ParseError](err); ok {
					printDiagnostics(s.Stderr, name, parseErr.Diagnostics)
					code = max(code, exitFindings)
					continue
				}
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", name, err)
					code = max(code, exitTrouble)
					continue
				}
				if showDiff && formatted != content {
					fmt.Fprint(s.Stdout, diffUnified(name, content, formatted))
					code = max(code, exitFindings)
				}
				if writeBack && formatted != content {
					if err := os.WriteFile(filename, []byte(formatted), 0o644); err != nil {
						fmt.Fprintf(s.Stderr, "%s: %v\n", name, err)
						code = max(code, exitTrouble)
					}
				}
				if !showDiff && !writeBack {
					fmt.Fprint(s.Stdout, formatted)
				}
			}
			if code != 0 {
				return &exitError{code}
			}
			return nil
		},
	}
}

func checkCommand() *cli.Command {
	return &cli.Command{
		Name:    "check",
		Summary: "Check CEL source files for parse and type errors",
		Usage:   "cells check <file|-> [file...]",
		Exec: func(_ context.Context, s *cli.State) error {
			opts, err := options(s)
			if err != nil {
				return err
			}
			if len(s.Args) == 0 {
				return errors.New("check: no files specified")
			}
			if err := checkInputs("check", s.Args); err != nil {
				return err
			}
			code := 0
			for _, filename := range s.Args {
				name, content, err := readInput(s, filename)
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", name, err)
					code = max(code, exitTrouble)
					continue
				}
				diags, err := lsp.Check(content, optionsFor(opts, filename))
				if err != nil {
					fmt.Fprintf(s.Stderr, "%s: %v\n", name, err)
					code = max(code, exitTrouble)
					continue
				}
				printDiagnostics(s.Stdout, name, diags)
				if len(diags) > 0 {
					code = max(code, exitFindings)
				}
			}
			if code != 0 {
				return &exitError{code}
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
			result, err := lsp.Hover(string(content), line, col, optionsFor(opts, filename))
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
			refs, err := lsp.References(string(content), line, col, optionsFor(opts, filename))
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

func definitionCommand() *cli.Command {
	return &cli.Command{
		Name:    "definition",
		Summary: "Show where the name at the given position was declared",
		Usage:   "cells definition <file>:<line>:<col>",
		Exec: func(_ context.Context, s *cli.State) error {
			opts, err := options(s)
			if err != nil {
				return err
			}
			if len(s.Args) != 1 {
				return fmt.Errorf("definition: expected one argument of the form file:line:col")
			}
			filename, line, col, err := parsePosition(s.Args[0])
			if err != nil {
				return err
			}
			content, err := os.ReadFile(filename)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filename, err)
			}
			definition, found, err := lsp.FindDefinition(filename, string(content), line, col, optionsFor(opts, filename))
			if err != nil {
				return fmt.Errorf("definition: %w", err)
			}
			if !found {
				return nil
			}
			fmt.Fprintf(s.Stdout, "%s:%d:%d\n", definition.Path, definition.Line, definition.Col)
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
			newName := s.GetFlag[string]("new-name")
			writeBack := s.GetFlag[bool]("write")
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
			result, err := lsp.Rename(string(content), line, col, newName, optionsFor(opts, filename))
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
// Columns are measured in UTF-16 code units, matching LSP positions.
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
