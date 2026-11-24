# Task 08 measurements

Raw evidence for [`../08-ci-oci-report.md`](../08-ci-oci-report.md). Everything here was produced on the development
VM (Docker 29.10.0, Buildx 0.39.1, Go 1.27.4) from the commit named in each file; **nothing was produced by Woodpecker,
on arm64 hardware, or against a public registry**.

| File | What |
|---|---|
| `image-final.txt` | the **final** amd64 image: size, runtime user, permissions, packages, config |
| `image-smoke-amd64.txt` | `ci/image-smoke.sh` on the final image, loaded from the OCI layout exactly as the release workflow does (40 checks) |
| `image-scan.txt` | Trivy 0.76.2 (pinned by digest), HIGH/CRITICAL, `linux/amd64` and `linux/arm64`, on the final layout |
| `release-rehearsal.txt` | the promotion rehearsal with the real `crane` against two throwaway registries: identical digest, `verify-published`, smoke test of the published digest, refusal of `latest` |
| `govulncheck.txt` | `govulncheck` v1.10.0 with `GOTOOLCHAIN=go1.27.4`, default and `-tags loadtest,diag` |
| `skip-census.txt` | the skip census of a full `go test -json -p 3 ./...` run (the source of `ci/skip-allowlist.txt`) |
| `image-local.txt` | an **earlier iteration** of the image evidence (before the final Dockerfile and the embedded time zone data were finalised); kept because it holds the arm64 archive facts (executable 27.1 MB, 14.3 MB compressed layers) |
