# Agent task 01 — Import `tgragnato/magnetico` into the `magnetico-web` development branch

**Target:** the existing Forgejo repository `magnetico-web` (`git.skobk.in/skobkin/magnetico-web`), in the already-created **`refactor/0.8.0-unify-and-new-storage` branch based on `master`**. **Not a new repository.**

**Status:** implementation assignment, phase 1 of 3. **Language:** English. **Do not deploy or migrate anything.**

## Objective

Import a pinned, reproducible source snapshot from `https://github.com/tgragnato/magnetico` into the new branch. The owner has already committed removal of the PHP/Symfony working files and the new seed documentation. Preserve the existing PHP repository history and, preferably, the upstream Go fork's Git ancestry. Produce an auditable starting point for the unified Magnetico 0.8.0 Go project.

The original Magnetico is `https://github.com/boramalper/magnetico`; `tgragnato/magnetico` is a fork of it. Keep appropriate attribution to **both** projects and preserve required licenses and notices.

## Critical rules

1. **Verify branch and clean state first.** Work only in the owner's new development branch. Do not overwrite `master`, rewrite published history, force-push, delete tags or remotes, or modify a production checkout. If uncommitted or untracked files may be lost, stop and report them; never silently `git clean -fdx` or discard them. No remote push unless explicitly authorized.
2. Record the current PHP HEAD SHA, branch name, remote URLs, upstream fork URL, exact selected upstream commit SHA, and toolchain prerequisites. Pin an upstream SHA; do not allow an upstream branch to drift mid-import.
3. Before replacing the current root README or any remaining PHP-related file, inspect historical project files (`README.md`, `.woodpecker`, `AGENTS.md`, templates/assets, license), identify important context for the future Go web port, and record the exact PHP commit/branch from which it can be retrieved. **Do not port PHP behavior or templates yet.** The initial PHP cleanup has already been committed by the owner; do not repeat it. Old PHP source must remain recoverable through Git history; no PHP files or build artifacts should be retained in the new working tree merely as archival copies.
4. The imported Go source files must initially be a faithful **upstream snapshot**, **except that the existing tracked `AGENTS.md` and `docs/` files must survive the import unchanged**. Verify that all imported upstream files match the pinned upstream commit. Do not mix functionality removal, Go module renaming, schema changes or codec implementation into the snapshot commit. Later separate commits may add attribution, project docs and development guidance.
5. **License:** both `boramalper/magnetico` and `tgragnato/magnetico` use **AGPL-3.0** (`COPYING`); `magnetico-web` PHP uses MIT. Do not leave the old root `LICENSE` implying that imported AGPL code is MIT. Keep upstream `COPYING`, copyright notices and required source/attribution information. If some original MIT-licensed PHP content is subsequently reused (e.g. templates or assets), keep the necessary MIT attribution/notices for those portions. Do not claim dual licensing or relicense third-party work without authorization. Call out any discovered license conflicts for owner review.

## Suggested Git-history-preserving strategy

The owner will already have created and committed `refactor/0.8.0-unify-and-new-storage`, removed obsolete PHP files, and committed this seed package (`AGENTS.md`, `docs/specs/`, `docs/wip/`). **Do not remove these files.** Treat their exact committed contents as protected input.

Prefer one two-parent merge commit with the **current development history** and the pinned `tgragnato/magnetico` upstream commit as parents. This preserves the PHP branch's earlier commits and the upstream Go ancestry simultaneously. Since the seed docs were committed in advance, the resulting merge tree intentionally contains **upstream source + pre-existing seed docs**; it cannot equal upstream's complete tree byte-for-byte. Compare only the imported upstream paths to the pinned upstream tree, excluding the protected seed files.

Illustrative procedure **for plan review, not copy-and-paste execution**:

1. Confirm current branch (`refactor/0.8.0-unify-and-new-storage`), clean worktree, both prior user commits and existence of `AGENTS.md`, `docs/specs/` and `docs/wip/`. Capture an immutable seed-commit SHA and all protected paths.
2. Add/fetch `https://github.com/tgragnato/magnetico.git` as a **separate named remote**, record the full upstream SHA, inspect license/source/test/toolchain. Do not rewrite or replace the owner's Forgejo `origin` remote.
3. Arrange a merge commit with both histories. An `ours` strategy may establish unrelated ancestry, **but only when coupled with explicit index/worktree replacement of upstream source paths**. Preserve tracked seed paths when replacing the tree (e.g. restore them from the pre-merge seed SHA after importing upstream). Never run `git rm -r -- .` or `git clean -fdx` without first securing and independently verifying the protected files; prefer a path-aware approach.
4. Check the proposed merge tree: exact upstream files at all corresponding imported paths, plus the unchanged seed docs; PHP runtime files absent; no stale MIT-only root license; upstream `COPYING` present. Show full staged diff and proposed parent SHAs before committing if a conflict or unexpected overlap appears.
5. Commit the snapshot import, optionally follow with a small dedicated attribution/bootstrap commit. Never force-push or alter `master`.

If a history-preserving merge is impractical, describe alternatives and request approval before a snapshot-only import. An attribution README is required in either case and is **not** a replacement for AGPL notices.

## Requested sequence and commit boundaries

**Commit A — upstream source snapshot:** imported Go source paths match the selected upstream commit; **pre-committed `AGENTS.md` and `docs/` remain unchanged**. Old tracked PHP/Symfony files and PHP-specific CI files are absent; imported `COPYING`, Go sources, tests, `go.mod`, `go.sum` and upstream metadata are present. Keep upstream's Go module path unchanged *in this commit*. Do not delete upstream components yet.

**Commit B — provenance and development scaffolding (if needed):** update the README with an accurate explanation:

- based on `tgragnato/magnetico` at its exact commit (include clickable upstream link and SHA), itself derived from `boramalper/magnetico`;
- the PHP `skobkin/magnetico-web` history remains accessible at `master`/recorded SHA, with its original MIT-licensed source;
- the new unified Go project's intended 0.8.0 scope and the fact it is **under development / not deployable**;
- upstream AGPL-3.0 and retained notices;
- source repository in Forgejo, possible GitHub mirror, not yet a published production image.

The owner has **already committed** the approved seed documents at:

```text
AGENTS.md
docs/specs/wire-format-v1.md
docs/specs/postgresql-persistence-v2.md
docs/wip/01-import-go-upstream.md
docs/wip/02-upstream-cleanup.md
docs/wip/03-storage-persistence.md
```

Preserve these files byte-for-byte in phase 1; the second spec is the revised persistence v2 draft. Keep the owner's root `AGENTS.md`; do not overwrite it with upstream-specific agent instructions. An imported `CLAUDE.md` can remain unmodified as part of the raw snapshot and be reviewed in phase 2.

## Verification

- `git status --short`, `git ls-tree -r --name-only HEAD`, verification that the seed files match the pre-import commit, and a comparison of **upstream-derived** tracked files with the pinned upstream SHA.
- `git log --graph --oneline` and `git rev-list --parents -n 1 <import-merge-commit>` demonstrate the two ancestries (if using the preferred approach).
- `git show <old-php-sha>:README.md` and at least one PHP template still work; they are absent from the new HEAD tree.
- Preserve the upstream's documented Go/toolchain version; attempt `go test ./...` and build with the **correct** toolchain where available. Report unavailable toolchains or upstream failures without introducing unrelated fixes.
- Confirm no database connections, schema migrations, deployments or production services were touched.

## Deliverables and stop condition

Write `docs/wip/upstream-import-report.md` containing the commit SHA(s), pinned upstream SHA, Git strategy used, changed file inventory, seed-file preservation checks, license/provenance note, exact build/test outcomes, and any blockers. **Stop after phase 1.** Do not start pruning or implementing storage without the owner's review/next instruction.
