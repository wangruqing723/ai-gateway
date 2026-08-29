# Vendored Web Runtime

These files are pinned locally so the privileged admin page does not execute mutable third-party JavaScript at runtime, and so the page renders correctly with no outbound network access at all.

## Scripts

| File | Version | Source | SHA-256 |
|---|---:|---|---|
| `alpine.min.js` | 3.14.9 | `https://cdn.jsdelivr.net/npm/alpinejs@3.14.9/dist/cdn.min.js` | `3ed1eed252488921df65e363d6715deb04d7f92aaedb9e52199fdf73cb1e0ad3` |
| `tailwindcss.js` | 3.4.17 | `https://cdn.tailwindcss.com/3.4.17` | `176e894661aa9cdc9a5cba6c720044cbbf7b8bd80d1c9a142a7c24b1b6c50d15` |

## Fonts

All three families are variable fonts (`wght` is a range, not an enumeration), so one file
covers every weight the page uses. Only the `latin` and `latin-ext` subsets are vendored;
the page has no Cyrillic, Greek, or Vietnamese content.

`@font-face` rules live inline in `index.html` — they are not in a separate stylesheet,
because `style-src` no longer allows any external origin.

| File | Family | Axes | Source | SHA-256 |
|---|---|---|---|---|
| `inter-latin.woff2` | Inter v20 | `wght 100..900` | Google Fonts `css2?family=Inter:wght@400..800` (`latin`) | `c940764593d0fe5d596be327ca7558855e018039fb78509aa21921fd3644c3e4` |
| `inter-latin-ext.woff2` | Inter v20 | `wght 100..900` | Google Fonts `css2?family=Inter:wght@400..800` (`latin-ext`) | `a28eb6d3ccb534ae0c94ca999371df024aab60b08c3c8a5720ee9e32fa0faaa2` |
| `jetbrains-mono-latin.woff2` | JetBrains Mono v24 | `wght 400..800` | Google Fonts `css2?family=JetBrains+Mono:wght@400..700` (`latin`) | `2c32b9b3ee358c119e210f6f5195f9bd34894d78a785ff2e95d60e718e400af4` |
| `jetbrains-mono-latin-ext.woff2` | JetBrains Mono v24 | `wght 400..800` | Google Fonts `css2?family=JetBrains+Mono:wght@400..700` (`latin-ext`) | `9c38cb2d0d2d93c1ee6e21fa78db76f13ea7e15e15cc64214c7ca89b6aaa35c4` |
| `material-symbols-outlined.woff2` | Material Symbols Outlined v368 | `FILL 0..1`, `wght 100..700`, `opsz 20..48` | Google Fonts `css2?family=Material+Symbols+Outlined:opsz,wght,FILL,GRAD@20..48,400..700,0..1,0&icon_names=<37 names>` | `6c3fa04444d5b116edcff137550ec7f79d2d4334b8ea30895e76332ad9d1c030` |

### Material Symbols is subset by icon name

Material Symbols is a **ligature** font: the markup writes the icon's name as text
(`<span class="material-symbols-outlined">dns</span>`) and the font substitutes a glyph.
Two consequences:

- The file is subset via `icon_names=` to only the icons the page actually uses. Adding a new
  icon to `index.html` therefore requires **re-subsetting this font** — a name that is not in
  the subset renders as its literal English text (`dns`, `alt_route`, …), not as a missing glyph.
  Enumerate the names from both static markup and Alpine `x-text` expressions, including the
  `navItems` array.
- `font-display: block` (not `swap`) is deliberate: during the fallback phase the browser paints
  the ligature name as ordinary text, so `swap` would flash a screen of English words before the
  icons appear. The text fonts use `swap` to keep first paint readable.

The 37 icons currently subset:

```
add alt_route bolt check_circle close content_copy delete description dns drag_indicator
edit error filter_alt_off health_and_safety hub key key_off keyboard_arrow_down
keyboard_arrow_up lan menu monitor_heart monitoring progress_activity receipt_long refresh
restart_alt save schedule search speed sync task_alt timer tune visibility warning
```

The `FILL` axis must survive subsetting — `.icon-fill` in `index.html` switches active nav
icons to `FILL 1`. Verify with `fontTools`:

```python
from fontTools.ttLib import TTFont
print([(a.axisTag, a.minValue, a.defaultValue, a.maxValue)
       for a in TTFont('material-symbols-outlined.woff2').  ['fvar'].axes])
```

`.woff2` is not in Go's builtin MIME table and distroless has no `/etc/mime.types`, so
`staticContentType` in `cmd/gateway/helpers.go` maps it explicitly. Without that the fonts are
served as `application/octet-stream` and `X-Content-Type-Options: nosniff` makes the browser
refuse them.

---

Update a file only together with its pinned version, source URL, checksum, and browser smoke test.
