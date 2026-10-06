# gmlls

A minimal GameMaker Language (GML) language server, written in Go. It focuses
on the essentials an editor needs to feel alive for GML projects: symbols,
completion, hover, signature help, re-indexing on disk changes, and
unknown-function warnings.

## Features

- **Document symbols** — functions, methods (`name = function(...)`), macros
  (`#macro`), and enums per file, read from comment/string-free text so
  commented-out code never leaks in.
- **Workspace symbols** — the same declarations plus asset names.
- **Completion** — project symbols, assets, GML keywords, and built-ins from
  your own `GmlSpec.xml`. After a dot: enum members (project *and* spec
  enums like `AudioEffectType.`) and `global.` variables.
- **Definition, references, hover** — `go-to-definition` jumps to the
  declaration; enum members and `global.x` resolve through the qualifier.
- **Signature help** — parameter lists for project functions and spec
  built-ins, with the active parameter tracked across partial input, commas,
  and multi-line signatures, and `(` / `,` trigger characters.
- **Re-index on disk changes** — a rescan ticker (default every 2 s) picks up
  new/edited/deleted `.gml` files and asset folders; files open in the editor
  are skipped because their buffers are the source of truth. A
  `workspace/didChangeWatchedFiles` notification triggers an immediate rescan.
- **Unknown-function diagnostics** — warnings when `name(...)` resolves to
  nothing in the project or the spec. Publishes after open, debounced after
  edits, re-published after rescans, and clears on close.

## Build

```
go build -o gmlls .
```

No runtime dependencies; GameMaker files are read directly from disk.

## Editor setup

Start it as a language server named `gmlls` and pass the initial options in
`initialize`:

| option            | default | meaning                                        |
| ----------------- | ------- | ---------------------------------------------- |
| `gmlSpec`         | —       | path to `GmlSpec.xml` (also read from `GMLLS_SPEC`) |
| `rescanSeconds`   | `2`     | rescan interval; `0` disables disk watching    |
| `diagnostics`     | `true`  | `false` turns off unknown-function warnings    |
| `ignore`          | `[]`    | names the checker never warns about            |

Example (Neovim):

```lua
require("lspconfig").gmlls.setup {
  cmd = { "gmlls" },
  init_options = {
    gmlSpec = os.getenv("HOME") .. "/.config/gmlls/GmlSpec.xml",
  },
}
```

## Command line

`gmlls -check <project-root> [gmlSpec.xml]` scans a project without an editor
and prints every potential unknown function as `file:line:col message`, which
is useful for tuning the `ignore` list against a real codebase:

```
$ gmlls -check ~/projects/my-game ~/.config/gmlls/GmlSpec.xml
/home/uami/.../Create_0.gml:10:1 unknown function: draw_sprit
3 potential unknown functions
```

## Caveats

- Enum members and `global.*` are completed after a dot; struct fields and
  instance variables are not indexed, so `obj.x` completion is left to the
  editor.
- The unknown-function checker is intentionally conservative: member calls,
  `function` declarations, and `new Known()` constructors are never flagged,
  and buffer locals/variables are tracked per file.
- `GmlSpec.xml` comes from your GameMaker installation and is *not* bundled;
  the server works without it, but built-in completion/hover/signatures
  require it.