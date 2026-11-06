package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pressly/cli"
	"go.vanburen.xyz/ok"
)

// run executes the cells command tree over args, returning what it wrote to
// stdout and stderr along with the exit code main would have used.
func run(t *testing.T, stdin string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runReader(t, strings.NewReader(stdin), args...)
}

// runReader is [run] with stdin read from r.
func runReader(t *testing.T, r io.Reader, args ...string) (stdout, stderr string, code int) {
	t.Helper()

	var outBuf, errBuf bytes.Buffer
	err := cli.ParseAndRun(t.Context(), rootCommand(), args, &cli.RunOptions{
		Stdin:  r,
		Stdout: &outBuf,
		Stderr: &errBuf,
	})
	code = exitCode(err, &errBuf)
	return outBuf.String(), errBuf.String(), code
}

// unreadReader fails the test if it is read, for commands that must reject
// their arguments before waiting on stdin.
type unreadReader struct{ t *testing.T }

func (r unreadReader) Read([]byte) (int, error) {
	r.t.Error("stdin was read")
	return 0, io.EOF
}

// writeFile writes content to a file in a fresh temporary directory and
// returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	ok.MustNoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// readFile returns the contents of the named file.
func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	ok.MustNoError(t, err)
	return string(content)
}

func TestVersion(t *testing.T) {
	t.Parallel()

	// What the toolchain stamped into the test binary is not fixed, so this
	// checks the shape rather than the values.
	stdout, stderr, code := run(t, "", "version")
	ok.Equal(t, code, 0)
	ok.Equal(t, stderr, "")
	ok.True(t, strings.HasPrefix(stdout, "cells    "), ok.Sprintf("stdout: %q", stdout))
	ok.True(t, strings.Contains(stdout, "\ngo       go1."), ok.Sprintf("stdout: %q", stdout))

	// --version prints the same, and belongs to the root alone.
	flagOut, stderr, code := run(t, "", "--version")
	ok.Equal(t, code, 0)
	ok.Equal(t, stderr, "")
	ok.Equal(t, flagOut, stdout)
	_, _, code = run(t, "", "check", "--version")
	ok.Equal(t, code, 2)

	// Without it, the root still wants a subcommand.
	_, stderr, code = run(t, "")
	ok.Equal(t, code, 2)
	ok.True(t, strings.HasSuffix(stderr, "error: subcommand required\n"), ok.Sprintf("stderr: %q", stderr))
}

func TestFormatStdin(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := run(t, "1+2", "format")
	ok.Equal(t, stdout, "1 + 2")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

func TestFormatFile(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1+2")

	stdout, stderr, code := run(t, "", "format", path)
	ok.Equal(t, stdout, "1 + 2")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
	// Without --write the file is left alone.
	ok.Equal(t, readFile(t, path), "1+2")
}

func TestFormatWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.cel")
	ok.MustNoError(t, os.WriteFile(path, []byte("1+2"), 0o640))
	link := filepath.Join(dir, "link.cel")
	ok.MustNoError(t, os.Symlink("a.cel", link))
	before, err := os.Stat(path)
	ok.MustNoError(t, err)

	stdout, stderr, code := run(t, "", "format", "--write", path)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
	ok.Equal(t, readFile(t, path), "1 + 2")

	// The file is replaced by a rename, not rewritten in place, so an
	// interrupted write cannot leave it partial. It keeps its permissions,
	// and nothing else is left in its directory.
	after, err := os.Stat(path)
	ok.MustNoError(t, err)
	ok.True(t, !os.SameFile(before, after))
	ok.Equal(t, after.Mode().Perm(), os.FileMode(0o640))
	entries, err := os.ReadDir(dir)
	ok.MustNoError(t, err)
	ok.Equal(t, len(entries), 2)

	// Through a symlink, the file it points to is replaced and the link kept.
	ok.MustNoError(t, os.WriteFile(path, []byte("3+4"), 0o640))
	_, _, code = run(t, "", "format", "--write", link)
	ok.Equal(t, code, 0)
	ok.Equal(t, readFile(t, path), "3 + 4")
	target, err := os.Readlink(link)
	ok.MustNoError(t, err)
	ok.Equal(t, target, "a.cel")
}

func TestFormatWriteStdin(t *testing.T) {
	t.Parallel()

	// --write needs a file to write back to, which is known before stdin is
	// read, so the command does not wait on it.
	path := writeFile(t, "a.cel", "1+2")
	tests := []struct {
		name string
		args []string
	}{
		{"implicit", []string{"format", "--write"}},
		{"explicit", []string{"format", "--write", "-"}},
		{"among_files", []string{"format", "--write", path, "-"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, code := runReader(t, unreadReader{t}, tt.args...)
			ok.Equal(t, stdout, "")
			ok.Equal(t, stderr, "error: format: cannot use --write with stdin\n")
			ok.Equal(t, code, 2)
			// Nothing was written to a file named alongside stdin.
			ok.Equal(t, readFile(t, path), "1+2")
		})
	}
}

func TestFormatSyntaxError(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1 +")

	// format reports a syntax error as the line check prints for it.
	checkOut, _, _ := run(t, "", "check", path)
	stdout, stderr, code := run(t, "", "format", path)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, checkOut)
	ok.Equal(t, strings.HasPrefix(stderr, path+":1:4: error: "), true, ok.Sprintf("stderr: %q", stderr))
	ok.Equal(t, code, 1)

	_, stderr, code = run(t, "1 +", "format")
	ok.Equal(t, strings.HasPrefix(stderr, "<stdin>:1:4: error: "), true, ok.Sprintf("stderr: %q", stderr))
	ok.Equal(t, code, 1)
}

func TestFormatDiff(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1+2")

	stdout, stderr, code := run(t, "", "format", "--diff", path)
	ok.Equal(t, stderr, "")
	// A file that needs formatting is reported as a difference, like gofmt -d.
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, "-1+2"), true, ok.Sprintf("diff output: %q", stdout))
	ok.Equal(t, strings.Contains(stdout, "+1 + 2"), true, ok.Sprintf("diff output: %q", stdout))
	// --diff alone reports without rewriting.
	ok.Equal(t, readFile(t, path), "1+2")
}

func TestFormatDiffWrite(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1+2")

	stdout, _, code := run(t, "", "format", "--diff", "--write", path)
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, "+1 + 2"), true, ok.Sprintf("diff output: %q", stdout))
	// --diff and --write together both report and rewrite.
	ok.Equal(t, readFile(t, path), "1 + 2")
}

func TestFormatAlreadyFormatted(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1 + 2")

	stdout, stderr, code := run(t, "", "format", "--diff", path)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

func TestFormatMissingFile(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := run(t, "", "format", "no-such-file.cel")
	ok.Equal(t, stdout, "")
	ok.Equal(t, code, 2)
	ok.Equal(t, strings.HasPrefix(stderr, "no-such-file.cel: "), true, ok.Sprintf("stderr: %q", stderr))
}

func TestCheckClean(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1 + 2")

	stdout, stderr, code := run(t, "", "check", path)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

func TestCheckDiagnostics(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1 + 'x'")

	stdout, stderr, code := run(t, "", "check", path)
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 1)
	want := path + ":1:3: warning: found no matching overload for '+' applied to '(int, string)'\n"
	ok.Equal(t, stdout, want)

	// - checks stdin, naming it <stdin>.
	stdout, stderr, code = run(t, "1 + 'x'", "check", "-")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 1)
	ok.Equal(t, stdout, "<stdin>:1:3: warning: found no matching overload for '+' applied to '(int, string)'\n")
}

func TestCheckMultipleFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	good := filepath.Join(dir, "good.cel")
	bad := filepath.Join(dir, "bad.cel")
	ok.MustNoError(t, os.WriteFile(good, []byte("1 + 2"), 0o600))
	ok.MustNoError(t, os.WriteFile(bad, []byte("1 + 'x'"), 0o600))

	stdout, _, code := run(t, "", "check", good, bad)
	ok.Equal(t, code, 1)
	// Only the file with a diagnostic is reported, and it names itself.
	ok.Equal(t, strings.Contains(stdout, good), false, ok.Sprintf("stdout: %q", stdout))
	ok.Equal(t, strings.HasPrefix(stdout, bad+":"), true, ok.Sprintf("stdout: %q", stdout))

	// A file that cannot be read outranks diagnostics in the others, and does
	// not stop them from being checked.
	missing := filepath.Join(dir, "missing.cel")
	stdout, stderr, code := run(t, "", "check", bad, missing)
	ok.Equal(t, code, 2)
	ok.Equal(t, strings.HasPrefix(stdout, bad+":"), true, ok.Sprintf("stdout: %q", stdout))
	ok.Equal(t, strings.HasPrefix(stderr, missing+": "), true, ok.Sprintf("stderr: %q", stderr))
}

func TestCheckUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"no_files", []string{"check"}, "error: check: no files specified\n"},
		{"stdin_twice", []string{"check", "-", "-"}, "error: check: stdin (-) can be named only once\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			stdout, stderr, code := runReader(t, unreadReader{t}, tt.args...)
			ok.Equal(t, stdout, "")
			ok.Equal(t, stderr, tt.stderr)
			ok.Equal(t, code, 2)
		})
	}
}

func TestHover(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "size('abc') > 2")

	stdout, stderr, code := run(t, "", "hover", path+":1:1")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
	ok.Equal(t, strings.HasPrefix(stdout, "`size`"), true, ok.Sprintf("stdout: %q", stdout))
}

func TestHoverNothingThere(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "1 + 2")

	stdout, stderr, code := run(t, "", "hover", path+":1:2")
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

func TestReferences(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x + x")

	stdout, stderr, code := run(t, "", "references", path+":1:1")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
	want := fmt.Sprintf("%s:1:1\n%s:1:5\n%s:1:9\n", path, path, path)
	ok.Equal(t, stdout, want)
}

func TestRename(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x")

	stdout, stderr, code := run(t, "", "rename", "--new-name", "y", path+":1:1")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
	ok.Equal(t, stdout, "y + y")
	// Without --write the file is left alone.
	ok.Equal(t, readFile(t, path), "x + x")
}

func TestRenameWrite(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x")

	before, err := os.Stat(path)
	ok.MustNoError(t, err)

	stdout, _, code := run(t, "", "rename", "--new-name", "y", "--write", path+":1:1")
	ok.Equal(t, stdout, "")
	ok.Equal(t, code, 0)
	ok.Equal(t, readFile(t, path), "y + y")
	// Like format --write, the file is replaced rather than rewritten.
	after, err := os.Stat(path)
	ok.MustNoError(t, err)
	ok.True(t, !os.SameFile(before, after))
}

func TestRenameRequiresNewName(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x")

	_, stderr, code := run(t, "", "rename", path+":1:1")
	ok.Equal(t, code, 2)
	ok.Equal(t, strings.Contains(stderr, "new-name"), true, ok.Sprintf("stderr: %q", stderr))
}

func TestRenameToKeyword(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x")

	_, stderr, code := run(t, "", "rename", "--new-name", "true", path+":1:1")
	ok.Equal(t, code, 2)
	ok.Equal(t, stderr, "error: rename: cannot rename to CEL keyword: \"true\"\n")
	ok.Equal(t, readFile(t, path), "x + x")
}

func TestPositionArgument(t *testing.T) {
	t.Parallel()

	path := writeFile(t, "a.cel", "x + x")

	tests := []struct {
		name string
		arg  string
		want string
	}{
		{"no_position", path, "expected file:line:col"},
		{"line_only", path + ":1", "expected file:line:col"},
		{"non_numeric_line", path + ":one:1", "invalid line number"},
		{"non_numeric_column", path + ":1:one", "invalid column number"},
		{"zero_indexed", path + ":0:1", "line and column must be >= 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := run(t, "", "hover", tt.arg)
			ok.Equal(t, code, 2)
			ok.Equal(t, strings.Contains(stderr, tt.want), true, ok.Sprintf("stderr: %q", stderr))
		})
	}
}

func TestPositionArgumentCount(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "", "hover")
	ok.Equal(t, code, 2)
	ok.Equal(t, strings.Contains(stderr, "expected one argument"), true, ok.Sprintf("stderr: %q", stderr))
}

// writeConfigAnd writes a CEL environment configuration and a .cel file into
// one temporary directory, returning both paths. The .cel file is therefore
// covered by the configuration whether it is named with --config or found by
// the search.
func writeConfigAnd(t *testing.T, config, cel string) (configPath, celPath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "cel.yaml")
	celPath = filepath.Join(dir, "a.cel")
	ok.MustNoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	ok.MustNoError(t, os.WriteFile(celPath, []byte(cel), 0o600))
	return configPath, celPath
}

const testConfig = `
name: test
variables:
  - name: count
    type: "int"
`

func TestConfigDeclaresVariables(t *testing.T) {
	t.Parallel()

	// Undeclared with no configuration in reach...
	uncovered := writeFile(t, "a.cel", "count > 1")
	stdout, _, code := run(t, "", "check", uncovered)
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, "undeclared reference to 'count'"), true, ok.Sprintf("stdout: %q", stdout))

	// ...and known with one.
	config, cel := writeConfigAnd(t, testConfig, "count > 1")
	stdout, stderr, code := run(t, "", "check", "--config", config, cel)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

// A configuration beside the file, or above it, is found without --config.
func TestConfigDiscovery(t *testing.T) {
	t.Parallel()

	t.Run("same_directory", func(t *testing.T) {
		t.Parallel()

		_, cel := writeConfigAnd(t, testConfig, "count > 1")
		stdout, stderr, code := run(t, "", "check", cel)
		ok.Equal(t, stdout, "")
		ok.Equal(t, stderr, "")
		ok.Equal(t, code, 0)
	})

	t.Run("parent_directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		ok.MustNoError(t, os.WriteFile(filepath.Join(dir, "cel.yaml"), []byte(testConfig), 0o600))
		nested := filepath.Join(dir, "policies", "deep")
		ok.MustNoError(t, os.MkdirAll(nested, 0o700))
		cel := filepath.Join(nested, "a.cel")
		ok.MustNoError(t, os.WriteFile(cel, []byte("count > 1"), 0o600))

		stdout, stderr, code := run(t, "", "check", cel)
		ok.Equal(t, stdout, "")
		ok.Equal(t, stderr, "")
		ok.Equal(t, code, 0)
	})

	t.Run("nearest_wins", func(t *testing.T) {
		t.Parallel()

		// An inner configuration declares a different variable than the outer
		// one, so which was used is visible in the diagnostic.
		dir := t.TempDir()
		ok.MustNoError(t, os.WriteFile(filepath.Join(dir, "cel.yaml"), []byte(testConfig), 0o600))
		inner := filepath.Join(dir, "inner")
		ok.MustNoError(t, os.MkdirAll(inner, 0o700))
		ok.MustNoError(t, os.WriteFile(filepath.Join(inner, "cel.yaml"),
			[]byte("name: inner\nvariables:\n  - name: other\n    type: \"int\"\n"), 0o600))
		cel := filepath.Join(inner, "a.cel")
		ok.MustNoError(t, os.WriteFile(cel, []byte("count > 1"), 0o600))

		stdout, _, code := run(t, "", "check", cel)
		ok.Equal(t, code, 1)
		ok.Equal(t, strings.Contains(stdout, "undeclared reference to 'count'"), true, ok.Sprintf("stdout: %q", stdout))
	})

	t.Run("explicit_config_wins", func(t *testing.T) {
		t.Parallel()

		// A --config naming a configuration that declares nothing beats the
		// one sitting beside the file.
		_, cel := writeConfigAnd(t, testConfig, "count > 1")
		empty := writeFile(t, "empty.yaml", "name: empty\n")

		stdout, _, code := run(t, "", "check", "--config", empty, cel)
		ok.Equal(t, code, 1)
		ok.Equal(t, strings.Contains(stdout, "undeclared reference to 'count'"), true, ok.Sprintf("stdout: %q", stdout))
	})
}

// Each file is checked against the configuration that covers it, so one
// command can span directories configured differently.
func TestConfigDiscoveryPerFile(t *testing.T) {
	t.Parallel()

	_, covered := writeConfigAnd(t, testConfig, "count > 1")
	uncovered := writeFile(t, "b.cel", "count > 1")

	stdout, _, code := run(t, "", "check", covered, uncovered)
	ok.Equal(t, code, 1)
	// Only the file with no configuration above it complains.
	ok.Equal(t, strings.Contains(stdout, covered), false, ok.Sprintf("stdout: %q", stdout))
	ok.Equal(t, strings.HasPrefix(stdout, uncovered+":"), true, ok.Sprintf("stdout: %q", stdout))
}

func TestConfigMakesDiagnosticsErrors(t *testing.T) {
	t.Parallel()

	config, cel := writeConfigAnd(t, testConfig, `count + "x"`)

	// A configuration says what the expression may name, so failing to check
	// against it is an error rather than a warning.
	stdout, _, code := run(t, "", "check", "--config", config, cel)
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, ": error: "), true, ok.Sprintf("stdout: %q", stdout))

	// The same expression with nothing declared is only a warning.
	uncovered := writeFile(t, "b.cel", `count + "x"`)
	stdout, _, code = run(t, "", "check", uncovered)
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, ": warning: "), true, ok.Sprintf("stdout: %q", stdout))
}

func TestConfigAppliesToEveryCommand(t *testing.T) {
	t.Parallel()

	config, cel := writeConfigAnd(t, testConfig, "count + count")

	// --config is a root flag, so every command accepts it. Hover does not
	// yet report a declared variable's type — that needs hover to consult the
	// environment, which it does not do today — but it must still not fail.
	_, stderr, code := run(t, "", "hover", "--config", config, cel+":1:1")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)

	stdout, _, code := run(t, "", "references", "--config", config, cel+":1:1")
	ok.Equal(t, code, 0)
	ok.Equal(t, strings.Count(stdout, "\n"), 2, ok.Sprintf("stdout: %q", stdout))

	stdout, _, code = run(t, "", "rename", "--config", config, "--new-name", "n", cel+":1:1")
	ok.Equal(t, code, 0)
	ok.Equal(t, stdout, "n + n")
}

func TestConfigErrors(t *testing.T) {
	t.Parallel()

	_, cel := writeConfigAnd(t, testConfig, "1 + 1")

	t.Run("missing", func(t *testing.T) {
		t.Parallel()

		_, stderr, code := run(t, "", "check", "--config", "no-such-config.yaml", cel)
		ok.Equal(t, code, 2)
		ok.True(t, strings.HasPrefix(stderr, "error: --config: "), ok.Sprintf("stderr: %q", stderr))
		ok.Contains(t, stderr, "no-such-config.yaml")
	})

	t.Run("malformed", func(t *testing.T) {
		t.Parallel()

		config, cel := writeConfigAnd(t, "name: [unterminated\n", "1 + 1")
		_, stderr, code := run(t, "", "check", "--config", config, cel)
		ok.Equal(t, code, 2)
		// The failing file is named, so the reader knows what to fix.
		ok.Equal(t, strings.Contains(stderr, config), true, ok.Sprintf("stderr: %q", stderr))
	})
}

// --descriptor-set makes the message types in a FileDescriptorSet resolvable,
// so a configuration can declare a variable of one. The set here is built by
// the internal/lsp tests; this one only needs to prove the flag reaches the
// environment, so it uses a well-known type already linked into the binary and
// checks that a bad path is reported.
func TestDefinition(t *testing.T) {
	t.Parallel()

	t.Run("declared_variable", func(t *testing.T) {
		t.Parallel()

		// testConfig declares "count" on line 4, column 11.
		config, cel := writeConfigAnd(t, testConfig, "count > 1")
		stdout, stderr, code := run(t, "", "definition", cel+":1:1")
		ok.Equal(t, stderr, "")
		ok.Equal(t, code, 0)
		ok.Equal(t, stdout, fmt.Sprintf("%s:4:11\n", config))
	})

	t.Run("macro_binding", func(t *testing.T) {
		t.Parallel()

		// No configuration needed: the macro binds the name in the file.
		cel := writeFile(t, "a.cel", "[1, 2].map(x, x * 2)")
		stdout, stderr, code := run(t, "", "definition", cel+":1:15")
		ok.Equal(t, stderr, "")
		ok.Equal(t, code, 0)
		ok.Equal(t, stdout, fmt.Sprintf("%s:1:12\n", cel))
	})

	t.Run("nothing_to_point_at", func(t *testing.T) {
		t.Parallel()

		cel := writeFile(t, "a.cel", `size("abc") > 1`)
		stdout, stderr, code := run(t, "", "definition", cel+":1:1")
		ok.Equal(t, stdout, "")
		ok.Equal(t, stderr, "")
		ok.Equal(t, code, 0)
	})
}

func TestDescriptorSetFlag(t *testing.T) {
	t.Parallel()

	config, cel := writeConfigAnd(t, testConfig, "count > 1")

	t.Run("missing_file", func(t *testing.T) {
		t.Parallel()

		_, stderr, code := run(t, "", "check", "--config", config, "--descriptor-set", "no-such.binpb", cel)
		ok.Equal(t, code, 2)
		ok.Equal(t, strings.Contains(stderr, "no-such.binpb"), true, ok.Sprintf("stderr: %q", stderr))
	})

	t.Run("not_a_descriptor_set", func(t *testing.T) {
		t.Parallel()

		junk := writeFile(t, "junk.binpb", "this is not a descriptor set at all")
		_, stderr, code := run(t, "", "check", "--config", config, "--descriptor-set", junk, cel)
		ok.Equal(t, code, 2)
		ok.Equal(t, strings.Contains(stderr, "FileDescriptorSet"), true, ok.Sprintf("stderr: %q", stderr))
	})
}

func TestExtensionEnablesLibrary(t *testing.T) {
	t.Parallel()

	// isIP comes from the network extension, so this only checks clean with
	// --ext=network.
	path := writeFile(t, "a.cel", "isIP('1.2.3.4')")

	stdout, _, code := run(t, "", "check", path)
	ok.Equal(t, code, 1)
	ok.Equal(t, strings.Contains(stdout, "isIP"), true, ok.Sprintf("stdout: %q", stdout))

	stdout, stderr, code := run(t, "", "check", "--ext=network", path)
	ok.Equal(t, stdout, "")
	ok.Equal(t, stderr, "")
	ok.Equal(t, code, 0)
}

func TestExtensionUnknown(t *testing.T) {
	t.Parallel()

	// Every command validates --ext for itself, so check every command.
	path := writeFile(t, "a.cel", "1 + 2")
	commands := [][]string{
		{"format", path},
		{"check", path},
		{"hover", path + ":1:1"},
		{"references", path + ":1:1"},
		{"rename", "--new-name", "y", path + ":1:1"},
	}
	for _, args := range commands {
		t.Run(args[0], func(t *testing.T) {
			t.Parallel()

			_, stderr, code := run(t, "", append([]string{args[0], "--ext=nope"}, args[1:]...)...)
			ok.Equal(t, code, 2)
			ok.Equal(t, strings.Contains(stderr, "nope"), true, ok.Sprintf("stderr: %q", stderr))
		})
	}
}

func TestUnknownCommand(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, "", "nope")
	ok.Equal(t, code, 2)
	ok.Equal(t, strings.Contains(stderr, "nope"), true, ok.Sprintf("stderr: %q", stderr))
}
