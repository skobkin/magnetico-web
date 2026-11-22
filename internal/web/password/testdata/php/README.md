# PHP legacy password-hash vectors

Test vectors for the legacy hash formats the Go web UI must verify (spec web-v1 §4.3.3), produced by
PHP's own implementations. All passwords are synthetic. The generator needs no network, database or
credentials, and the Go tests only read `fixtures.json`; they never regenerate it.

| Format | Producer |
|---|---|
| `$argon2i$` m=16384,t=2,p=4 | `password_hash(PASSWORD_ARGON2I)` |
| `$argon2id$` m=65536,t=4,p=1 and t=2,p=1 | `sodium_crypto_pwhash_str` (libsodium) |
| `$argon2id$` PHP defaults and m=65536,t=3,p=2 | `password_hash(PASSWORD_ARGON2ID)` |
| `$2y$` cost 10 and 13 | `password_hash(PASSWORD_BCRYPT)` |

Passwords cover ASCII, multi-byte UTF-8 and lengths of 1, 8, 72, 73, 100 and 4096 bytes (no NUL; PHP
rejects it for bcrypt). Each case records PHP's own verdicts: the right password, a wrong password, the
password with a tail appended and, above 72 bytes, its 72-byte prefix. These show bcrypt ignoring
everything past byte 72 while Argon2 does not.

## Runtime

- Image: `php:8.3-cli@sha256:9e2a4e9b1704c05cc469fddbf9b0f0f205e2bb13efd249bbad177289ebdc5c7c`
- PHP 8.3.37, Argon2 provider `standard` (libargon2 via PHP), libsodium 1.0.18, OpenSSL 3.5.7
  (these are also stored in `fixtures.json` under `meta`)
- `generate.php` sha256: `d9ccfe160ab90ce18d58d09d40220a67a7398b575fe2f12678630828f9239ec0`
- `fixtures.json` sha256: `1a46ed72c38b4e3801d85713cbb8f6a33eb26ae51ccc0d51ba6f0094db83b21e`

## Reproduce

Pulling the image is the only network action. Run the generator with the network disabled:

```sh
docker pull php:8.3-cli   # confirm the digest above, then use it explicitly
docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges \
  --user 65534:65534 -v "$PWD/internal/web/password/testdata/php:/work:ro" \
  php@sha256:9e2a4e9b1704c05cc469fddbf9b0f0f205e2bb13efd249bbad177289ebdc5c7c \
  php /work/generate.php > internal/web/password/testdata/php/fixtures.json
```

Salts are random, so a re-run produces different hashes with the same properties; commit a new file only
with an updated checksum above.
