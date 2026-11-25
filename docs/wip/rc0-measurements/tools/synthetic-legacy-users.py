#!/usr/bin/env python3
"""Synthetic legacy PHP account database for the RC0 automated acceptance.

    synthetic-legacy-users.py SCHEMA_SQL HASHES_FILE OUT_SQL TOKENS_OUT

SCHEMA_SQL is internal/migration/users/testdata/legacy_users.sql (the five Doctrine
migrations verbatim). HASHES_FILE holds three PHP-generated hashes of the password
"test", one per line: bcrypt cost 13, the PHP argon2id default, bcrypt cost 10. The
output SQL (schema + data) and the list of legacy API keys for the RSS checks contain
credentials of these fake accounts: keep them outside the repository.

Accounts (every password is "test" except the deliberately unsupported hash):
  1  test               bcrypt-13   ROLE_USER, ROLE_ADMIN (the documented admin after set-role)
  2..25 rc0user02..25   bcrypt-13 / argon2id / bcrypt-10 in turn, created 2018..2025
  26 unsupported-hash   an MD5-crypt hash: the migrator must count it as needs-reset
  27 Пользователь       non-ASCII username (bcrypt-13)
  28 many-tokens        55 legacy API tokens (above the 50-token cap; imported, warned)
Invitations: two per user 2..6, some used by later accounts. API tokens: 0..3 per user.
"""
import json
import secrets
import sys
from datetime import datetime, timedelta


def q(s: str) -> str:
    return "'" + s.replace("'", "''") + "'"


def main() -> None:
    schema_sql, hashes_file, out_sql, tokens_out = sys.argv[1:5]
    hashes = [line.strip() for line in open(hashes_file, encoding="utf-8") if line.strip()]
    assert len(hashes) == 3 and hashes[0].startswith("$2y$13$") and hashes[1].startswith("$argon2id$"), "unexpected hashes file"
    users = []
    base = datetime(2018, 7, 1, 12, 0, 0)
    users.append((1, "test", hashes[0], "test@rc0.invalid", ["ROLE_USER", "ROLE_ADMIN"], base))
    for i in range(2, 26):
        users.append((i, f"rc0user{i:02d}", hashes[i % 3], f"rc0user{i:02d}@rc0.invalid", ["ROLE_USER"], base + timedelta(days=97 * i)))
    users.append((26, "unsupported-hash", "$1$rc0salt$Zq0jY6lJZ0w7yEo0qR7aZ1", "unsupported@rc0.invalid", ["ROLE_USER"], base + timedelta(days=2000)))
    users.append((27, "Пользователь", hashes[0], "nonascii@rc0.invalid", ["ROLE_USER"], base + timedelta(days=2100)))
    users.append((28, "many-tokens", hashes[0], "many-tokens@rc0.invalid", ["ROLE_USER"], base + timedelta(days=2200)))

    lines = [open(schema_sql, encoding="utf-8").read(), "", "-- synthetic RC0 accounts"]
    for uid, name, pw, email, roles, created in users:
        lines.append(f"INSERT INTO users.users (id, username, password, email, roles, created_at) OVERRIDING SYSTEM VALUE VALUES "
                     f"({uid}, {q(name)}, {q(pw)}, {q(email)}, {q(json.dumps(roles))}, {q(created.isoformat(sep=' '))});")
    inv_id = 0
    for owner in range(2, 7):
        for k in range(2):
            inv_id += 1
            used_by = 20 + inv_id if inv_id <= 5 else "NULL"
            lines.append(f"INSERT INTO users.invites (id, user_id, used_by_id, code) OVERRIDING SYSTEM VALUE VALUES "
                         f"({inv_id}, {owner}, {used_by}, {q(secrets.token_hex(16))});")
    tokens = []
    for uid in range(1, 26):
        for k in range(uid % 4):
            tokens.append((secrets.token_hex(16), uid, base + timedelta(days=97 * uid + k)))
    for k in range(55):
        tokens.append((secrets.token_hex(16), 28, base + timedelta(days=2200 + k)))
    for key, uid, created in tokens:
        lines.append(f"INSERT INTO users.api_tokens (key, user_id, created_at) VALUES ({q(key)}, {uid}, {q(created.isoformat(sep=' '))});")
    lines.append("DO $do$ DECLARE m integer; BEGIN SELECT MAX(id) + 1 INTO m FROM users.users; EXECUTE format('ALTER TABLE users.users ALTER COLUMN id RESTART WITH %s', m); END $do$;")
    lines.append("DO $do$ DECLARE m integer; BEGIN SELECT MAX(id) + 1 INTO m FROM users.invites; EXECUTE format('ALTER TABLE users.invites ALTER COLUMN id RESTART WITH %s', m); END $do$;")
    with open(out_sql, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    with open(tokens_out, "w", encoding="utf-8") as f:
        for key, uid, _ in tokens:
            f.write(f"{uid} {key}\n")
    print(f"users={len(users)} invites={inv_id} api_tokens={len(tokens)}")


if __name__ == "__main__":
    main()
