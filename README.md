# cells

A [language server](https://microsoft.github.io/language-server-protocol/) for [CEL (Common Expression Language)](https://cel.dev).
It operates on individual `.cel` files, providing various LSP features.

## Installation

```console
$ go install go.vanburen.xyz/cells/cmd/cells@latest
```

## Features

* Semantic highlighting
* Diagnostics
* Formatting
* Hover
* References
* Completion
* Signature help
* Variable renaming
* Inlay hints (expression evaluation)

## CLI

In addition to operating as a language server, `cells` provides CLI commands for use outside an editor.

### `cells format`

Format CEL source files.
With no arguments, reads from stdin and writes to stdout.

```console
$ echo "1+2" | cells format
1 + 2
$ cells format file.cel
$ cells format --write file.cel
$ cells format --diff file.cel
$ cells format --diff --write file.cel
```

### `cells check`

Check CEL source files for parse and type errors, plus invalid `duration()`,
`timestamp()`, and `matches()` literal arguments.
Prints `file:line:col: severity: message` for each diagnostic and exits 1 if any are found.

```console
$ cells check file.cel
$ cells check *.cel
```

Type errors are reported as warnings unless [an environment](#environment) is
declared, since without one every reference to a real variable is an undeclared
reference and `cells` cannot tell a typo from a name it was never told about.

### `cells hover`

Show documentation for the element at a given position (`file:line:col`, 1-indexed).

```console
$ cells hover file.cel:1:7
```

### `cells references`

List all references to the identifier at a given position.
Each reference is printed as `file:line:col`.

```console
$ cells references file.cel:1:1
```

### `cells rename`

Rename the identifier at a given position.
Without `--write`, prints the updated content to stdout.

```console
$ cells rename --new-name=newVar file.cel:1:1
$ cells rename --new-name=newVar --write file.cel:1:1
```

## Environment

Real CEL expressions refer to variables their host provides — `request`,
`resource`, a request message. `cells` knows nothing about those by default, so
every one of them reads as an undeclared reference:

```console
$ cat policy.cel
request.method == "POST" && retries < 3
$ cells check policy.cel
policy.cel:1:1: warning: undeclared reference to 'request' (in container '')
policy.cel:1:29: warning: undeclared reference to 'retries' (in container '')
```

Declare them in a [cel-go environment configuration](https://pkg.go.dev/cel.dev/cel-go/common/env#Config),
the same YAML format other CEL tooling reads:

```yaml
# cel.yaml
name: my-service
extensions:
  - name: strings
variables:
  - name: request
    type: "map<string, dyn>"
  - name: retries
    type: "int"
```

```console
$ cells check --config=cel.yaml policy.cel
```

With the environment declared, a genuine mistake is an error rather than a
warning lost among undeclared references:

```console
$ echo 'retries + "x"' > bad.cel
$ cells check --config=cel.yaml bad.cel
bad.cel:1:9: error: found no matching overload for '+' applied to '(int, string)'
```

`--config` applies to every command. For the language server, set `config` in
your editor's `initializationOptions`:

```json
{ "config": "/path/to/cel.yaml" }
```

The configuration also accepts `container` and `imports` for namespacing,
`context_variable` to declare every field of a message as a top-level variable,
`functions` for host-provided functions, and `extensions` (see below). Message
types must be linked into the `cells` binary to be resolvable, so proto schemas
are not yet usable; `stdlib` subsetting is not supported either.

## Extensions

`cells` checks against plain CEL by default. Enable [cel-go extension libraries](https://pkg.go.dev/cel.dev/cel-go/ext)
(`strings`, `math`, `network`, etc.) to match the environment your expressions actually run in.

Also available: `optional` for [optional types](https://pkg.go.dev/cel.dev/cel-go/cel#OptionalTypes)
(`?.`, `[?_]`, `optional.of`), and `jwt` and `hmac` from
[cel-go's security extensions](https://pkg.go.dev/cel.dev/cel-go/ext/security).
Enabling `regex` or `jwt` enables `optional` too, since both depend on it.

On the CLI, pass `--ext` (repeatable) to any command:

```console
$ cells check --ext=network file.cel
$ cells hover --ext=network --ext=strings file.cel:1:1
```

For the language server, set `extensions` in your editor's `initializationOptions`:

```json
{ "extensions": ["network", "strings"] }
```

Extensions can also be named in a [configuration](#environment), under the same
names, where a `version` may be pinned:

```yaml
extensions:
  - name: strings
    version: 2
  - name: network
```

`--ext` adds to whatever the configuration already asks for. Names given
without a version get the newest one.

Run `cells --help` for the full list of supported names.

## Usage

### Neovim

Add to your config (e.g. `~/.config/nvim/init.lua`):

```lua
vim.lsp.config("cells", {
  filetypes = { "cel" },
  cmd = { "cells", "serve" },
})
vim.lsp.enable("cells")
```

Neovim recognizes `.cel` files by default as of [0.12](https://github.com/neovim/neovim/pull/37834).

To verify it's working, open a `.cel` file and run `:checkhealth lsp` or `:LspInfo`.
