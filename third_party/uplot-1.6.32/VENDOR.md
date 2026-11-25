# uPlot 1.6.32 (vendored)

Used only for the charts of the administrator section (`/admin`).

| | |
|---|---|
| Package | `uplot@1.6.32` (npm), licence MIT, copyright (c) 2022 Leon Sorokin |
| Selected because | the newest stable release; it was published on 2025-03-14, far more than two weeks before 2026-11-25 (`dist-tags.latest`) |
| Tarball | `https://registry.npmjs.org/uplot/-/uplot-1.6.32.tgz` |
| Tarball integrity (npm registry) | `sha512-KIMVnG68zvu5XXUbC4LQEPnhwOxBuLyW1AHtpm6IKTXImkbLgkMy+jabjLgSLMasNuGGzQm/ep3tOkyTxpiQIw==` (verified) |
| Tarball SHA-256 | `4b8a8191e425658e3ea8c8c1314b0fc679c861ee3a1af2b20c7b16ba50b5133d` |

Copied from the verified tarball without modification (`tar`; `npm install` was never run; the package has no dependencies):

| File | In the repository | SHA-256 |
|---|---|---|
| `dist/uPlot.iife.min.js` | `internal/web/ui/static/uPlot.iife.min.js` | `19c8d4c6ad88929a79f4ae49d6f7161566dfd0ba3d15cc495e974f787eb78f1f` |
| `dist/uPlot.min.css` | `internal/web/ui/static/uPlot.min.css` | `df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04` |
| `LICENSE` | this directory, shipped as `licenses/uplot-LICENSE` (archives) and `/usr/share/licenses/magnetico/uplot-LICENSE` (image) | `8f989229699b4fe2f1a0432d0e9edc338a8a911e250e2d1b01ecd770a5f5b1bd` |

`TestVendoredUPlotChecksumsMatchVendorMd` fails if the files and this table disagree.

## Evaluation (static scan of `uPlot.iife.min.js`, 51 KB)

* No `eval(`, no `Function` constructor, no `document.write`, no `innerHTML`, no `insertRule`, no `createElement("style")`, no `setAttribute`, no `cssText`, no network API (`fetch`, `XMLHttpRequest`, `importScripts`).
* Styling is done through the CSSOM only (`element.style.<property> = …`), which the application's Content-Security-Policy
  (`default-src 'self'; …`, no `'unsafe-inline'`) allows; the stylesheet is a same-origin file. The CSP is **not** changed for the charts.
* **Not verified on the development machine:** rendering in a real browser under that policy (no usable browser could reach loopback there).
  This is an item of `docs/wip/rc0-acceptance-checklist.md`.
