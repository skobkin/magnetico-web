#!/usr/bin/env python3
"""End-to-end search latency of chosen query shapes through the proxy and the published
web service (RC0 automated acceptance). Reuses the client of functional-check.py.

    search-latency.py --ca CA.crt --secrets DIR [--repeat 3]

Signs in as the synthetic administrator, mints one API token, then times
GET /api/v2/torrents for each shape (q, sort) REPEAT times. Prints shape, status and
milliseconds; never a name.
"""
import argparse
import importlib.util
import os
import ssl
import statistics
import time
import urllib.parse

spec = importlib.util.spec_from_file_location("fc", os.path.join(os.path.dirname(os.path.abspath(__file__)), "functional-check.py"))
fc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fc)

SHAPES = [("", "discovered"), ("1080p", "discovered"), ("1080p", "size"), ("1080p", "name"), ("1080p x264", "discovered"),
          ("flac", "discovered"), ("mp3 x265", "discovered"), ("pdf 1080p", "discovered"), ("epub 720p", "discovered"),
          ("mp4 flac", "discovered"), ("mp4 flac", "size"), ("mp4 flac", "name"), ("zzqxj", "discovered")]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--ca", required=True)
    ap.add_argument("--secrets", required=True)
    ap.add_argument("--repeat", type=int, default=3)
    a = ap.parse_args()
    ctx = ssl.create_default_context(cafile=a.ca)
    adm = fc.Client(ctx)
    st, _ = adm.login("test", "test")
    assert st == 303, st
    st, tok = adm.mint("api")
    assert tok, st
    api = fc.Client(ctx)
    api.bearer = tok
    print("| q | sort | status | ms (each run) | median |")
    print("|---|---|---|---|---|")
    for q, sort in SHAPES:
        times, sts = [], set()
        for _ in range(a.repeat):
            path = "/api/v2/torrents?limit=50&sort=" + sort + ("&q=" + urllib.parse.quote(q) if q else "")
            t0 = time.monotonic()
            st, _, _ = api.req("GET", path)
            times.append((time.monotonic() - t0) * 1000)
            sts.add(st)
        print(f"| {q or '(none)'} | {sort} | {','.join(map(str, sorted(sts)))} | {' / '.join(f'{t:.0f}' for t in times)} | {statistics.median(times):.0f} |", flush=True)


if __name__ == "__main__":
    main()
