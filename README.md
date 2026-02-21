# cells

A language server for [CEL (Common Expression Language)](https://cel.dev).

## Features

- **Semantic highlighting** — keywords, operators, strings, numbers, macros, functions, methods, variables, and type conversions are highlighted with appropriate semantic token types.

## Building

```bash
go install github.com/stefanvanburen/cells/cmd/cells@latest
```

Or from a local checkout:

```bash
go build -o cells ./cmd/cells/
```

## Usage

```bash
# Start the language server (communicates over stdin/stdout)
cells serve
```

## Editor Setup

### Neovim

Add this to your Neovim config (e.g. `~/.config/nvim/init.lua` or equivalent):

```lua
vim.api.nvim_create_autocmd("FileType", {
  pattern = "cel",
  callback = function()
    vim.lsp.start({
      name = "cells",
      cmd = { "cells", "serve" },
    })
  end,
})
```

Neovim doesn't recognize `.cel` files by default, so you'll also need to add a filetype detection rule:

```lua
vim.filetype.add({
  extension = {
    cel = "cel",
  },
})
```

To verify it's working, open a `.cel` file and run `:checkhealth lsp` or `:LspInfo`.

## Testing

```bash
go test ./...
```

Integration tests boot the LSP server over an in-memory pipe, open `.cel` test files, and verify semantic token responses.
