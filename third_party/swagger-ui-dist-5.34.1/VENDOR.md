# swagger-ui-dist 5.34.1 (vendored)

Used only for the public API documentation pages (`/api/v1`, `/api/v2`).

| | |
|---|---|
| Package | `swagger-ui-dist@5.34.1` (npm), licence Apache-2.0 |
| Selected because | the newest release that is at least two weeks old on 2026-11-23 (published 2026-10-30; 5.35.x and 5.36.0 were newer than that) |
| Tarball | `https://registry.npmjs.org/swagger-ui-dist/-/swagger-ui-dist-5.34.1.tgz` |
| Tarball integrity (npm registry) | `sha512-VSr8fXfI93obYaQicLJbLUKGRxDojj/SfRJDDLCioGTUBj4BK/Nj4eE9jCeCAnJxVLi7iS+08PbiZLyrQKBkJQ==` (verified) |
| Tarball SHA-256 | `8e7286de0230e431fc8a5a75490d28117af3d0c306413b6f9e17f7bb55045fb6` |
| Supported OpenAPI | up to 3.2.0 (the bundle's default `findBy` version; it carries the 3.2 schema `2025-09-17`) |

Copied from the tarball without modification:

| File | In the repository | SHA-256 |
|---|---|---|
| `swagger-ui-bundle.js` | `internal/web/ui/static/` | `36eeb4d5640b9c036b1d0f854499af1cceebbdb260b790a03a18be5e8488fbf5` |
| `swagger-ui.css` | `internal/web/ui/static/` | `c3cfe8248c4b0b89673600fb829924eafbac6de4bc4b2b06f5f5243483074d0a` |
| `LICENSE`, `NOTICE`, `swagger-ui-bundle.js.LICENSE.txt` | this directory | see the files |

The files were extracted from the verified tarball with `tar`. `npm install` was
never run: the package's `@scarf/scarf` dependency (a download-statistics
helper that runs at install time) is therefore not present, and the bundle
contains no telemetry code (no occurrence of "telemetry" or "scarf").

## Evaluation (static scan of `swagger-ui-bundle.js`, 1.68 MB)

* No `eval(` and no `document.write`. The `Function` constructor appears only in
  the usual global-object and `bind` fallbacks (`this||new Function("return
  this")()`, a lodash root lookup, a `Function.prototype.bind` shim). They are
  guarded by short-circuits that a current browser never reaches, so the
  documentation pages run under a CSP **without** `'unsafe-eval'`. If a browser
  did reach one, the CSP would block it rather than run it. Confirming this in
  a real browser is an owner item (no browser exists on the development VM).
* It never contacts a host on its own. The absolute URLs in the bundle are
  XML/JSON-schema namespace identifiers, specification links and the default
  validator address `https://validator.swagger.io/validator`, which
  `docs.js` disables with `validatorUrl: null`.
* `swagger-ui.css` uses `data:` URIs for icons (`url(data:image/svg+xml…)`,
  four `data:image/png|jpeg`). The documentation pages therefore add
  `img-src 'self' data:` to the otherwise unchanged CSP. That is the only
  relaxation: no `unsafe-inline`, no `unsafe-eval`, no external origin, and it
  does not apply to any other route.
* React sets element styles through the CSSOM, which a strict `style-src` does
  not block. If a real browser nevertheless reports a `style-src` violation,
  that must be reported rather than silenced with `unsafe-inline`.

## Updating

Pick the newest release at least two weeks old, verify the registry integrity,
repeat the scan, replace both files, update this table and re-run
`go test ./internal/web/ui ./internal/web/apidocs`.
