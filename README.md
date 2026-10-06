idk, i needed this

(Neovim):

```lua
require("lspconfig").gmlls.setup {
  cmd = { "gmlls" },
  init_options = {
    gmlSpec = os.getenv("HOME") .. "/.config/gmlls/GmlSpec.xml",
  },
}
```

