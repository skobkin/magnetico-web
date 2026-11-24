# Task 08 measurements

Raw evidence for `docs/wip/08-ci-oci-report.md`. Everything here was produced on the development VM
(Docker 29.10.0, Buildx 0.39.1) from the commit named in each file; nothing was produced by
Woodpecker, on arm64 hardware, or against a public registry.

| File | What |
|---|---|
| `image-local.txt` | the locally built amd64 image: size, runtime user, permissions, packages, config, history; arm64 archive facts |
| `image-smoke-amd64.txt` | `ci/image-smoke.sh` against that image |
