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
* Hover (including declared variable types, and message field types and comments)
* References
* Go to definition
* Completion (including declared variables and message fields)
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

### `cells definition`

Show where the name at a given position was declared.

```console
$ cells definition file.cel:1:1
cel.yaml:5:11
```

CEL has nothing to import, and every standard-library and extension function is
defined in Go, so the names with a definition to point at are the ones
[a configuration](#environment) declared — its `variables` and `functions`
entries — and the ones a macro binds, such as the `x` in `list.map(x, x * 2)`
or the `v` in `cel.bind(v, ..., ...)`, which resolve within the file itself.

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

`cells` finds `cel.yaml` on its own: with no `--config`, it looks in the
directory of each file being checked and then in each parent, so a
configuration covers the tree beneath it and a repository with differently
configured subdirectories works without any flags. The nearest one wins, and
`--config` overrides the search entirely.

The language server discovers a configuration per document the same way, and
picks up edits to it without a restart. A configuration that fails to load is
reported as a diagnostic on the files that depend on it.

With the environment declared, a genuine mistake is an error rather than a
warning lost among undeclared references:

```console
$ echo 'retries + "x"' > bad.cel
$ cells check --config=cel.yaml bad.cel
bad.cel:1:9: error: found no matching overload for '+' applied to '(int, string)'
```

`--config` applies to every command. To name one explicitly for the language
server, set `config` in your editor's `initializationOptions`:

```json
{ "config": "/path/to/cel.yaml" }
```

The configuration also accepts `container` and `imports` for namespacing,
`functions` for host-provided functions, and `extensions` (see below).
`stdlib` subsetting is not supported.

### Protobuf schemas

To declare a variable whose type is a protobuf message, point `cells` at an
encoded `FileDescriptorSet` so it can resolve the type:

```console
$ buf build -o descriptors.binpb          # or protoc --descriptor_set_out=...
$ cells check --descriptor-set=descriptors.binpb --config=cel.yaml policy.cel
```

```yaml
# cel.yaml
name: my-service
variables:
  - name: request
    type_name: "my.service.Request"
```

Field access is then checked against the message:

```console
$ echo 'request.nosuchfield == 1' > bad.cel
$ cells check --descriptor-set=descriptors.binpb --config=cel.yaml bad.cel
bad.cel:1:8: error: undefined field 'nosuchfield'
```

Hover and completion report a field's type along with the comment written
above it in the `.proto` file, which travels in the descriptor set:

```console
$ cells hover --descriptor-set=descriptors.binpb --config=cel.yaml policy.cel:1:9
`method`: `string`

The HTTP method, uppercased.
```

`buf build` includes the comments by default; `protoc` needs
`--include_source_info`. Without them the type is reported on its own.

Hover reports a declared variable's type and whatever `description` the
configuration gave it, and completion offers the declared names — after a dot
on a message-typed value, its fields:

```console
$ cells hover --descriptor-set=descriptors.binpb --config=cel.yaml policy.cel:1:1
`request`: `my.service.Request`

The request being authorized.
```

`context_variable` declares every field of a message as a top-level name
instead, which is how many hosts present their input:

```yaml
context_variable:
  type_name: "my.service.Request"
```

```console
$ echo 'method == "POST" && retries < 3' | cells check --descriptor-set=descriptors.binpb --config=cel.yaml /dev/stdin
```

`--descriptor-set` is repeatable. For the language server, set
`descriptorSets` in your editor's `initializationOptions`:

```json
{ "descriptorSets": ["/path/to/descriptors.binpb"] }
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
