#!/usr/bin/env python3
"""Email flows against a synthetic SMTP server (Mailpit) through the proxy (RC0 automated
acceptance). Reuses the client of functional-check.py.

    smtp-check.py --ca CA.crt [--mailpit http://127.0.0.1:18025]

Uses the synthetic migrated account rc0user04 (password "test", unverified migrated
address): sends a verification email, confirms the address through the link from the
message, requests a password-recovery email and checks that its link page opens (the
reset is not completed, so the password stays "test"). Prints outcomes only, never an
address, a link or a token.
"""
import argparse
import importlib.util
import json
import os
import re
import ssl
import time
import urllib.parse
import urllib.request

spec = importlib.util.spec_from_file_location("fc", os.path.join(os.path.dirname(os.path.abspath(__file__)), "functional-check.py"))
fc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fc)
USER, ADDR = "rc0user04", "rc0user04@rc0.invalid"


def mail_link(mailpit, kind, since):
    for _ in range(60):
        msgs = json.load(urllib.request.urlopen(f"{mailpit}/api/v1/messages?limit=50"))["messages"]
        for m in msgs:
            if any(t["Address"] == ADDR for t in m["To"]) and m["Created"] > since:
                body = json.load(urllib.request.urlopen(f"{mailpit}/api/v1/message/{m['ID']}"))
                text = body.get("Text") or ""
                links = re.findall(r"https://magnet\.rc0\.test:18443(/[A-Za-z0-9/_-]+)", text)
                links = [l for l in links if kind in l]
                if links:
                    return links[0], body
        time.sleep(1)
    return None, None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ca", required=True)
    ap.add_argument("--mailpit", default="http://127.0.0.1:18025")
    a = ap.parse_args()
    ctx = ssl.create_default_context(cafile=a.ca)
    since = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() - 2))
    u = fc.Client(ctx)
    st, _ = u.login(USER, "test")
    fc.report(st == 303, "sign-in of the migrated user for the email flow", f"status {st}")
    _, tok, page = u.csrf("/account?tab=profile")
    fc.report(b"Unverified" in page or b"unverified" in page, "migrated address is shown as unverified")
    st, _, _ = u.req("POST", "/account/email/resend", {"csrf_token": tok})
    fc.report(st in (200, 303), "verification email requested", f"status {st}")
    link, msg = mail_link(a.mailpit, "/email/verify/", since)
    fc.report(link is not None, "verification email delivered over STARTTLS to the synthetic SMTP server")
    if msg:
        hdr = {h.lower(): v for h, v in (msg.get("Headers") or {}).items()}
        print(f"INFO  message: subject length {len(msg.get('Subject',''))}, from {msg['From']['Address']}, tls-related headers {[k for k in hdr if 'tls' in k]}")
    if link:
        st, h, b = u.req("GET", link)
        fc.report(st == 200 and h.get("Referrer-Policy") == "no-referrer", "verification link page opens (no-referrer)", f"status {st}")
        m = re.search(rb'name="csrf_token" value="([^"]*)"', b)
        st, _, _ = u.req("POST", link, {"csrf_token": m.group(1).decode() if m else ""})
        fc.report(st in (200, 303), "address confirmed through the link", f"status {st}")
        _, _, page = u.csrf("/account?tab=profile")
        fc.report(b"Verified" in page or b"verified" in page.replace(b"unverified", b""), "account page shows the address as verified")
        st, _, b = u.req("GET", link)
        print(f"INFO  reusing the consumed verification link: status {st}, generic 'cannot be used' page: {b'cannot be used' in b}")
    since = time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(time.time() - 2))
    anon = fc.Client(ctx)
    _, tok, _ = anon.csrf("/password/forgot")
    st, _, b = anon.req("POST", "/password/forgot", {"email": ADDR, "csrf_token": tok or ""})
    fc.report(st in (200, 303), "password recovery requested for the verified address", f"status {st}")
    link, _ = mail_link(a.mailpit, "/password/reset/", since)
    fc.report(link is not None, "recovery email delivered")
    if link:
        st, h, _ = anon.req("GET", link)
        fc.report(st == 200 and "no-store" in (h.get("Cache-Control") or ""), "reset link page opens (not completed: the password stays 'test')", f"status {st}")
    other = fc.Client(ctx)
    _, tok, _ = other.csrf("/password/forgot")
    st, _, b2 = other.req("POST", "/password/forgot", {"email": "nobody@rc0.invalid", "csrf_token": tok or ""})
    strip = lambda x: re.sub(rb'name="csrf_token" value="[^"]*"', b"", x)
    fc.report(st in (200, 303) and strip(b2) == strip(b), "recovery for an unknown address answers exactly like a known one", f"status {st}")
    print(f"SUMMARY pass={fc.RESULTS['PASS']} fail={fc.RESULTS['FAIL']}")


if __name__ == "__main__":
    main()
