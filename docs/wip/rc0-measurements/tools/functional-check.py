#!/usr/bin/env python3
"""RC0 functional acceptance through the TLS reverse proxy (stdlib only).

    functional-check.py --ca CA.crt --secrets DIR [--sample N] [--seed S] > results.txt

Talks HTTPS to Traefik on 127.0.0.1:18443 with SNI/Host magnet.rc0.test (the address is
resolved locally by this script, no /etc/hosts change). Reads the legacy source through
`docker exec rc0-pg psql` as the read-only role. Output: one line per check, PASS/FAIL
and counts; never a torrent name, a path, a token, a password or a cookie. Torrent ids
and the categories of the sampled shapes are printed (they are not private).

DIR holds pg.env (LEGACY_RO_PW) and legacy-api-keys.txt (synthetic accounts).
Accounts used: "test" (password "test", administrator) and "rc0user02" (password
"test", ordinary user) - both synthetic legacy accounts imported by `migrate users`.
"""
import argparse
import base64
import datetime as dt
import http.client
import json
import random
import re
import socket
import ssl
import subprocess
import sys
import time
import urllib.parse

HOST, PORT = "magnet.rc0.test", 18443
EXPECTED_CSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

_orig_getaddrinfo = socket.getaddrinfo


def _getaddrinfo(host, *a, **kw):
    if isinstance(host, str) and host.endswith(".rc0.test"):
        host = "127.0.0.1"
    return _orig_getaddrinfo(host, *a, **kw)


socket.getaddrinfo = _getaddrinfo

RESULTS = {"PASS": 0, "FAIL": 0}


def report(ok, name, detail=""):
    RESULTS["PASS" if ok else "FAIL"] += 1
    print(f"{'PASS' if ok else 'FAIL'}  {name}" + (f"  ({detail})" if detail else ""), flush=True)
    return ok


class Client:
    def __init__(self, ctx, host=HOST):
        self.ctx, self.host, self.cookies, self.bearer = ctx, host, {}, None

    def req(self, method, path, body=None, headers=None, raw_cookie=True):
        h = {"Host": f"{self.host}:{PORT}", "User-Agent": "rc0-functional-check"}
        if self.cookies and raw_cookie:
            h["Cookie"] = "; ".join(f"{k}={v}" for k, v in self.cookies.items())
        if self.bearer:
            h["Authorization"] = "Bearer " + self.bearer
        if body is not None and not isinstance(body, bytes):
            body = urllib.parse.urlencode(body).encode()
            h["Content-Type"] = "application/x-www-form-urlencoded"
            h["Origin"] = f"https://{self.host}:{PORT}"
        h.update(headers or {})
        for attempt in range(20):
            c = http.client.HTTPSConnection(self.host, PORT, context=self.ctx, timeout=120)
            c.request(method, path, body=body, headers=h)
            r = c.getresponse()
            data = r.read()
            hdrs = r.headers
            c.close()
            if r.status == 429 and attempt < 19:
                time.sleep(float(hdrs.get("Retry-After", "2") or 2))
                continue
            break
        for sc in hdrs.get_all("Set-Cookie") or []:
            kv = sc.split(";", 1)[0]
            k, _, v = kv.partition("=")
            if "max-age=0" in sc.lower() or v == "":
                self.cookies.pop(k, None)
            else:
                self.cookies[k] = v
        self.last_set_cookies = hdrs.get_all("Set-Cookie") or []
        return r.status, hdrs, data

    def csrf(self, path):
        st, _, b = self.req("GET", path)
        m = re.search(rb'name="csrf_token" value="([^"]*)"', b)
        return st, (m.group(1).decode() if m else None), b

    def login(self, user, pw):
        _, tok, _ = self.csrf("/login")
        st, hdrs, _ = self.req("POST", "/login", {"username": user, "password": pw, "csrf_token": tok or ""})
        return st, hdrs

    def mint(self, kind):
        tab, path = ("api", "/account/api-tokens") if kind == "api" else ("rss", "/account/rss-tokens")
        _, tok, _ = self.csrf(f"/account?tab={tab}")
        st, _, b = self.req("POST", path, {"label": "rc0-functional", "csrf_token": tok or ""})
        m = re.search(rb"<code>(m[gt][tf]_[A-Za-z0-9_-]+)</code>", b)
        return st, (m.group(1).decode() if m else None)

    def json(self, path):
        st, h, b = self.req("GET", path)
        try:
            return st, json.loads(b) if b else None, h
        except ValueError:
            return st, None, h


def psql(secrets, sql):
    env = dict(PGPASSWORD=secrets["LEGACY_RO_PW"])
    out = subprocess.run(["docker", "exec", "-i", "-e", "PGPASSWORD", "rc0-pg", "psql", "-X", "-q", "-At", "-h", "127.0.0.1",
                          "-U", "legacy_ro", "-d", "legacy_torrents", "-v", "ON_ERROR_STOP=1"],
                         input=sql.encode(), capture_output=True, check=True, env={**env, "PATH": "/usr/bin:/bin"})
    return out.stdout.decode().splitlines()


def api_files(cl, tid, details):
    """The complete (path, size) list of a torrent through the API, by whichever mode the
    server offers; also exercises the other mode for small torrents."""
    files = []

    def nm(o):
        return o["name"] if o.get("name") is not None else base64.b64decode(o["name_base64"]).decode("utf-8", "surrogateescape")

    if details["full_tree_available"]:
        st, tree, _ = cl.json(f"/api/v2/torrents/{tid}/files/tree")
        if st != 200:
            raise RuntimeError(f"tree status {st}")

        def walk(d, prefix):
            for f in d["files"]:
                files.append((prefix + nm(f), f["size"]))
            for sub in d["dirs"]:
                walk(sub, prefix + nm(sub) + "/")

        walk(tree["root"], "")
        return files, "tree"
    st, prob, _ = cl.json(f"/api/v2/torrents/{tid}/files/tree")
    if st != 422:
        raise RuntimeError(f"tree for a torrent without full_tree_available: {st}")

    def walk_dir(path, prefix):
        off = 0
        while off is not None:
            st, page, _ = cl.json(f"/api/v2/torrents/{tid}/files?dir={path}&offset={off}&limit=1000")
            if st != 200:
                raise RuntimeError(f"files page status {st}")
            for e in page["entries"]:
                if e["type"] == "dir":
                    walk_dir(e["path"], prefix + nm(e) + "/")
                else:
                    files.append((prefix + nm(e), e["size"]))
            off = page["next_offset"]

    if details["file_count"] > 20000:
        # The API allows 2 requests/s per token: a full walk of a 100k-file torrent takes
        # tens of minutes. Its root summary is compared instead (verify-content compared
        # every file of every torrent).
        st, page, _ = cl.json(f"/api/v2/torrents/{tid}/files?dir=&offset=0&limit=1000")
        if st != 200:
            raise RuntimeError(f"files page status {st}")
        return page["dir"], "dirs-summary"
    walk_dir("", "")
    return files, "dirs"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ca", required=True)
    ap.add_argument("--secrets", required=True)
    ap.add_argument("--sample", type=int, default=300)
    ap.add_argument("--seed", type=int, default=20261126)
    ap.add_argument("--only", default="")
    a = ap.parse_args()
    secrets = dict(line.strip().split("=", 1) for line in open(f"{a.secrets}/pg.env") if "=" in line)
    legacy_keys = [line.split()[1] for line in open(f"{a.secrets}/legacy-api-keys.txt")]
    ctx = ssl.create_default_context(cafile=a.ca)

    # --- anonymous, proxy, headers
    anon = Client(ctx)
    for p in ["/healthz", "/readyz"]:
        st, _, _ = anon.req("GET", p)
        report(st == 200, f"{p} through the proxy", f"status {st}")
    st, h, _ = anon.req("GET", "/torrents")
    report(st in (302, 303) and "/login" in (h.get("Location") or ""), "anonymous /torrents redirects to sign-in", f"status {st}")
    st, h, _ = anon.req("GET", "/admin")
    report(st in (302, 303, 403) , "anonymous /admin is refused or redirected", f"status {st}")
    st, h, _ = anon.req("GET", "/api/v2/torrents")
    report(st == 401, "anonymous API v2 is 401", f"status {st}")
    st, h, _ = anon.req("GET", "/login")
    report(h.get("Content-Security-Policy") == EXPECTED_CSP, "production CSP on /login")
    print(f"INFO  Strict-Transport-Security: {h.get('Strict-Transport-Security') or 'absent'}", flush=True)
    st, h, _ = anon.req("GET", "/healthz", headers={"Host": f"other.rc0.test:{PORT}"})
    report(st == 404, "an unconfigured Host header is not routed by the proxy (valid SNI, other Host)", f"status {st}")

    # --- sign in as the migrated administrator (legacy bcrypt hash of "test")
    adm = Client(ctx)
    st, h = adm.login("test", "test")
    sess = [c for c in adm.last_set_cookies if c.startswith("__Host-")]
    report(st == 303, "sign-in of migrated account test/test", f"status {st}")
    report(bool(sess) and all("secure" in c.lower() and "httponly" in c.lower() for c in sess), "session cookie is __Host-, Secure, HttpOnly")
    for p in ["/admin", "/admin/crawlers", "/admin/quarantine", "/admin/users", "/admin/invitations", "/admin/trackers", "/admin/api/overview"]:
        st, h, b = adm.req("GET", p)
        report(st == 200 and "no-store" in (h.get("Cache-Control") or ""), f"admin {p}", f"status {st}")
        if p == "/admin/users":
            report(b"$2y$" not in b and b"$argon2" not in b, "admin users page shows no password hash")
    st, h, b = adm.req("GET", "/account?tab=profile")
    report(st == 200, "account page", f"status {st}")
    st, api_tok = adm.mint("api")
    report(st == 200 and bool(api_tok), "API token minted on the account page", f"status {st}")
    st, rss_tok = adm.mint("rss")
    report(st == 200 and bool(rss_tok), "RSS token minted on the account page", f"status {st}")

    # --- an ordinary migrated user
    usr = Client(ctx)
    st, _ = usr.login("rc0user02", "test")
    report(st == 303, "sign-in of migrated ordinary user (argon2id/bcrypt legacy hash)", f"status {st}")
    st, h, _ = usr.req("GET", "/admin")
    report(st == 403, "ordinary user gets 403 on /admin", f"status {st}")
    st, h, _ = usr.req("GET", "/admin/api/overview")
    report(st == 403, "ordinary user gets 403 on /admin/api/overview", f"status {st}")
    st, _ = Client(ctx).login("test", "wrong-password")
    report(st != 303, "a wrong password is refused", f"status {st}")
    st, _ = Client(ctx).login("unsupported-hash", "test")
    report(st != 303, "an account with an unsupported legacy hash cannot sign in", f"status {st}")

    # --- API v2
    api = Client(ctx)
    api.bearer = api_tok
    st, page, _ = api.json("/api/v2/torrents?limit=50")
    report(st == 200 and len(page["items"]) == 50, "API v2 newest page", f"status {st}")
    for q, sort, order in [("1080p", "discovered", "desc"), ("1080p", "size", "desc"), ("x264", "name", "asc"), ("flac", "discovered", "asc")]:
        seen, prev, cur, pages, ok = set(), None, None, 0, True
        keyf = {"discovered": lambda t: (t["discovered_at"], t["id"]), "size": lambda t: (t["total_size"], t["id"]),
                "name": lambda t: 0}[sort]  # name: the database collation decides, checked below
        order_ids = []
        for _ in range(4):
            path = f"/api/v2/torrents?q={q}&sort={sort}&order={order}&limit=100" + (f"&cursor={urllib.parse.quote(cur)}" if cur else "")
            st, page, _ = api.json(path)
            if st != 200:
                ok = False
                break
            pages += 1
            for t in page["items"]:
                ok &= t["id"] not in seen and q.lower() in t["name"].lower()
                seen.add(t["id"])
                order_ids.append(t["id"])
                k = keyf(t)
                if prev is not None:
                    ok &= (k <= prev) if order == "desc" else (k >= prev)
                prev = k
            cur = page["next_cursor"]
            if not cur:
                break
        if sort == "name" and order_ids:
            want = psql(secrets, f"SELECT id FROM magneticod.torrents WHERE id IN ({','.join(map(str, order_ids))}) ORDER BY name {order}, id {order}")
            ok &= [int(x) for x in want] == order_ids
        report(ok, f"API search keyset pagination q={q} sort={sort} order={order}", f"{pages} pages, {len(seen)} items, ordered, no duplicates")
    st, _, _ = api.req("GET", "/api/v2/torrents?q=a&cursor=garbage")
    report(st == 400, "invalid cursor is 400", f"status {st}")
    st, _, _ = api.req("GET", "/api/v2/feeds/latest.rss")
    report(st == 401, "an API token does not open the feed endpoint", f"status {st}")

    # --- RSS
    st, h, b = anon.req("GET", f"/api/v2/feeds/latest.rss?feed_token={rss_tok}")
    report(st == 200 and b.lstrip().startswith(b"<?xml") and b.count(b"<item>") == 1000, "v2 RSS feed with an RSS token", f"status {st}, items {b.count(b'<item>')}")
    report("no-store" in (h.get("Cache-Control") or "") and h.get("Referrer-Policy") == "no-referrer", "feed is no-store, no-referrer")
    lk = legacy_keys[0]
    st, h, b = anon.req("GET", f"/api/v1/feed/rss/last?api-token={lk}")
    report(st == 200 and b.count(b"<item>") == 1000, "legacy v1 RSS URL with a migrated legacy token (query)", f"status {st}")
    st, h, b = anon.req("GET", "/api/v1/feed/rss/last", headers={"api-token": lk})
    report(st == 200, "legacy v1 RSS with the api-token header", f"status {st}")
    leg = Client(ctx)
    leg.bearer = lk
    st, _, _ = leg.req("GET", "/api/v2/torrents")
    report(st == 401, "the legacy token is refused by API v2", f"status {st}")
    st, _, _ = anon.req("GET", "/api/v1/feed/rss/last?api-token=" + "0" * 32)
    report(st == 401, "an unknown legacy token is 401", f"status {st}")

    # --- content: the migrated torrents against the legacy source
    rnd = random.Random(a.seed)
    max_id = int(psql(secrets, "SELECT max(id) FROM magneticod.torrents")[0])
    cats = {
        "random": f"SELECT id FROM magneticod.torrents TABLESAMPLE SYSTEM (1) REPEATABLE ({a.seed}) LIMIT {a.sample}",
        "empty-name": "SELECT id FROM magneticod.torrents WHERE name = '' LIMIT 10",
        "control-name": "SELECT id FROM magneticod.torrents WHERE name ~ '[\\x01-\\x1f\\x7f]' LIMIT 10",
        "bidi-name": "SELECT id FROM magneticod.torrents WHERE name ~ '[\\u200e\\u200f\\u202a-\\u202e\\u2066-\\u2069]' LIMIT 10",
        "outer-space-name": "SELECT id FROM magneticod.torrents WHERE name <> btrim(name) LIMIT 10",
        "empty-path": "SELECT DISTINCT torrent_id FROM (SELECT torrent_id FROM magneticod.files WHERE path = '' LIMIT 200) s LIMIT 10",
        "dot-components": "SELECT DISTINCT torrent_id FROM (SELECT torrent_id FROM magneticod.files WHERE path ~ '(^|/)\\.\\.?(/|$)' LIMIT 200) s LIMIT 10",
        "empty-components": "SELECT DISTINCT torrent_id FROM (SELECT torrent_id FROM magneticod.files WHERE path ~ '//' OR path LIKE '/%' OR path LIKE '%/' LIMIT 200) s LIMIT 10",
        "control-path": "SELECT DISTINCT torrent_id FROM (SELECT torrent_id FROM magneticod.files WHERE path ~ '[\\x01-\\x1f\\x7f]' LIMIT 200) s LIMIT 10",
        "replacement-char-path": "SELECT DISTINCT torrent_id FROM (SELECT torrent_id FROM magneticod.files WHERE path ~ '\\uFFFD' LIMIT 200) s LIMIT 10",
        "duplicate-paths": "SELECT torrent_id FROM magneticod.files WHERE torrent_id IN (SELECT id FROM magneticod.torrents TABLESAMPLE SYSTEM (2) REPEATABLE (3)) GROUP BY torrent_id HAVING count(*) > count(DISTINCT path) LIMIT 10",
        "atypical-single": "SELECT t.id FROM magneticod.torrents t TABLESAMPLE SYSTEM (1) REPEATABLE (5) JOIN magneticod.files f ON f.torrent_id = t.id WHERE f.path <> t.name GROUP BY t.id HAVING count(*) = 1 LIMIT 10",
        "largest": "SELECT torrent_id FROM magneticod.files WHERE torrent_id IS NOT NULL GROUP BY torrent_id ORDER BY count(*) DESC LIMIT 3",
        "edges": f"SELECT id FROM magneticod.torrents WHERE id IN ((SELECT min(id) FROM magneticod.torrents), {max_id})",
    }
    if a.only:
        cats = {k: v for k, v in cats.items() if k in a.only.split(",")}
    ids = {}
    for cat, sql in cats.items():
        for line in psql(secrets, sql):
            ids.setdefault(int(line), cat)
    print(f"INFO  content sample: {len(ids)} torrents: " + ", ".join(f"{c}={sum(1 for v in ids.values() if v == c)}" for c in cats), flush=True)
    idlist = ",".join(map(str, sorted(ids)))
    meta = {}
    for line in psql(secrets, f"SELECT json_build_object('id', id, 'h', encode(info_hash, 'hex'), 'n', name, 's', total_size, 'd', discovered_on) FROM magneticod.torrents WHERE id IN ({idlist})"):
        o = json.loads(line)
        meta[o["id"]] = o
    srcfiles = {}
    for line in psql(secrets, f"SELECT json_build_object('id', torrent_id, 'f', json_agg(json_build_array(path, size))) FROM magneticod.files WHERE torrent_id IN ({idlist}) GROUP BY torrent_id"):
        o = json.loads(line)
        srcfiles[o["id"]] = sorted((p, s) for p, s in o["f"])
    mism, modes, nfiles, nsummary, tsummary = [], {}, 0, 0, 0
    for tid in sorted(ids):
        m = meta[tid]
        try:
            st, d, _ = api.json(f"/api/v2/torrents/{tid}")
            if st != 200:
                raise RuntimeError(f"details {st}")
            want_at = dt.datetime.fromtimestamp(m["d"], dt.timezone.utc)
            got_at = dt.datetime.fromisoformat(d["discovered_at"].replace("Z", "+00:00"))
            bad = [f for f, ok in [("name", d["name"] == m["n"]), ("total_size", d["total_size"] == m["s"]), ("info_hash", d["info_hash"] == m["h"]),
                                   ("discovered_at", got_at == want_at), ("file_count", d["file_count"] == len(srcfiles[tid]))] if not ok]
            files, mode = api_files(api, tid, d)
            modes[mode] = modes.get(mode, 0) + 1
            if mode == "dirs-summary":
                # Only the root summary (file count and total size) is compared: these files are
                # NOT compared one by one through the API (verify-content did that in the database).
                nsummary += files["file_count"]
                tsummary += 1
                if files["file_count"] != len(srcfiles[tid]) or files["size"] != sum(sz for _, sz in srcfiles[tid]):
                    bad.append("file_summary")
            else:
                nfiles += len(files)
                if sorted(files) != srcfiles[tid]:
                    bad.append("file_list")
            st2, lk2, _ = api.json(f"/api/v2/torrents?info_hash={m['h']}")
            if st2 != 200 or [t["id"] for t in lk2["items"]] != [tid]:
                bad.append("info_hash_lookup")
            st3, h3, _ = anon.req("GET", f"/magnet/{m['h']}")
            if st3 not in (301, 302, 303, 307, 308) or not (h3.get("Location") or "").startswith("magnet:?xt=urn:btih:" + m["h"]):
                bad.append("magnet_redirect")
            if bad:
                mism.append((tid, ids[tid], bad))
        except Exception as e:  # noqa: BLE001 - reported as a failure of this torrent
            mism.append((tid, ids[tid], [f"error: {type(e).__name__}: {str(e)[:80]}"]))
    report(not mism, "migrated torrents equal the legacy source (metadata, info-hash lookup, magnet redirect; complete file list with duplicates, or the root summary above 20,000 files)",
           f"{len(ids)} torrents; complete file lists compared for {len(ids) - tsummary} torrents ({nfiles} files); "
           f"root summary only for {tsummary} torrents ({nsummary} files); modes {modes}, mismatches {len(mism)}")
    for tid, cat, bad in mism[:50]:
        print(f"      mismatch id={tid} category={cat} fields={bad}")

    # --- UI pages for both file-browser modes (status only)
    for tid in [t for t, c in ids.items() if c == "largest"][:1] + [t for t, c in ids.items() if c == "random"][:3]:
        st, _, b = adm.req("GET", f"/torrents/{tid}")
        report(st == 200, f"torrent page renders (id {tid})", f"status {st}, {len(b)} bytes")
    st, _, b = adm.req("GET", "/torrents?q=1080p+x264")
    report(st == 200, "UI search page", f"status {st}")
    docs_csp = "default-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'; object-src 'none'"
    st, h, _ = anon.req("GET", "/api/v2")
    report(st == 303, "API documentation requires sign-in", f"status {st}")
    for p in ["/api/v2/openapi.yaml", "/api/v1/openapi.yaml", "/api/v2", "/api/v1"]:
        st, h, _ = adm.req("GET", p)
        want = docs_csp if p in ("/api/v2", "/api/v1") else EXPECTED_CSP
        report(st == 200 and h.get("Content-Security-Policy") == want, f"API documentation {p} (signed in; its documented CSP)", f"status {st}")

    # --- both file-browser modes on the same medium-sized torrents: the whole tree and a
    # complete directory-by-directory walk must give the same list (and the source's)
    medium = [int(x) for x in psql(secrets, "SELECT torrent_id FROM magneticod.files WHERE torrent_id IN (SELECT id FROM magneticod.torrents TABLESAMPLE SYSTEM (1) REPEATABLE (11)) GROUP BY torrent_id HAVING count(*) BETWEEN 300 AND 3000 LIMIT 5")]
    bad_modes = 0
    for tid in medium:
        st, d, _ = api.json(f"/api/v2/torrents/{tid}")
        tree, _ = api_files(api, tid, d)
        walk, _ = api_files(api, tid, {**d, "full_tree_available": False, "file_count": min(d["file_count"], 20000)}) if not d["full_tree_available"] else (None, None)
        if walk is None:
            files = []
            def walk_dir(path, prefix):
                off = 0
                while off is not None:
                    st, page, _ = api.json(f"/api/v2/torrents/{tid}/files?dir={path}&offset={off}&limit=1000")
                    for e in page["entries"]:
                        n = e["name"] if e.get("name") is not None else base64.b64decode(e["name_base64"]).decode("utf-8", "surrogateescape")
                        if e["type"] == "dir":
                            walk_dir(e["path"], prefix + n + "/")
                        else:
                            files.append((prefix + n, e["size"]))
                    off = page["next_offset"]
            walk_dir("", "")
            walk = files
        if sorted(tree) != sorted(walk):
            bad_modes += 1
    report(bad_modes == 0 and len(medium) > 0, "tree mode and directory-page mode return the same file list", f"{len(medium)} torrents of 300-3000 files, differing {bad_modes}")

    print(f"SUMMARY pass={RESULTS['PASS']} fail={RESULTS['FAIL']}")
    sys.exit(1 if RESULTS["FAIL"] else 0)


if __name__ == "__main__":
    main()
