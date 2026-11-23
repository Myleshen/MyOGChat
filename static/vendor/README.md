# Vendored frontend libraries

No build step, no CDN at runtime. Each file is copied verbatim from the named
release; the app injects it on first need (`app.js` → `loadVendor`), and none of
these are in the service worker's prefetch list except highlight.js and marked.

| Library | Version | Files | Upstream |
|---|---|---|---|
| KaTeX | 0.19.1 | `katex/katex.min.js`, `katex/katex.min.css`, `katex/contrib/auto-render.min.js`, `katex/fonts/*.woff2` | <https://github.com/KaTeX/KaTeX/releases/tag/v0.19.1> (`katex.tar.gz`) |
| Mermaid | 12.3.0 | `mermaid/mermaid.min.js` | <https://www.npmjs.com/package/mermaid/v/12.3.0> (`dist/mermaid.min.js`) |
| highlight.js | 11.12.0 | `highlight.min.js`, `highlight-theme.css`, `highlight-light.css` | <https://highlightjs.org/download/> (BSD-3-Clause) |
| marked | 15.0.12 | `marked.min.js` | <https://github.com/markedjs/marked> (MIT) |

KaTeX ships all three font formats; only the `.woff2` set is kept (296 KB)
because every browser that can run this app supports it — a browser that cannot
falls through the stylesheet's own `woff`/`ttf` fallbacks and loses glyphs, not
the app.

Sizes: `katex.min.js` 273 KB (76 KB gzip), `katex.min.css` 25 KB (3.6 KB gzip),
fonts 296 KB, `mermaid.min.js` 5.5 MB (1.6 MB gzip). Mermaid is the reason both
libraries load lazily.

Both are MIT licensed.
