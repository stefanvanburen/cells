# cells

A [language server](https://microsoft.github.io/language-server-protocol/) for [CEL (Common Expression Language)](https://cel.dev).
It operates on individual `.cel` files, providing various LSP features.

## Installation

```console
$ go install github.com/stefanvanburen/cells/cmd/cells@latest
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

Neovim doesn't recognize `.cel` files by default
(until [0.12 is released](https://github.com/neovim/neovim/pull/37834)),
so you'll also need to add a filetype detection rule:

```lua
vim.filetype.add({
  extension = {
    cel = "cel",
  },
})
```

To verify it's working, open a `.cel` file and run `:checkhealth lsp` or `:LspInfo`.
