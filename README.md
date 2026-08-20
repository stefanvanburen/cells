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
Prints `file:line:col: error: message` for each diagnostic and exits 1 if any are found.

```console
$ cells check file.cel
$ cells check *.cel
```

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
