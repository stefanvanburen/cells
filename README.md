# cells

A [language server](https://microsoft.github.io/language-server-protocol/) for [CEL (Common Expression Language)](https://cel.dev).

## Features

* Semantic highlighting
* Diagnostics
* Formatting
* Hover
* References
* Completion
* Signature help
* Variable renaming

## Building

```console
go install github.com/stefanvanburen/cells/cmd/cells@latest
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
