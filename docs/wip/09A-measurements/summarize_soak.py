#!/usr/bin/env python3
"""Render a soak09a run (raw/soak09a.jsonl) as Markdown.

    python3 summarize_soak.py raw/soak09a.jsonl [--pg 'docker exec m-pg-soak psql -U postgres -d bench'] > raw/soak09a-summary.md

Only the JSONL file is needed; with --pg the persisted aggregates of the run are read from the
disposable benchmark database as well (authentication cache effectiveness, batch utilisation,
statistics growth). Nothing here contains a credential: the file has none.
"""
import json
import statistics as st
import subprocess
import sys


def load(path):
    rows = []
    for line in open(path):
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def of(rows, kind):
    return [r["data"] for r in rows if r["kind"] == kind]


def med(v):
    return st.median(v) if v else float("nan")


def pct(v, p):
    if not v:
        return float("nan")
    v = sorted(v)
    return v[min(len(v) - 1, int(p * len(v)))]


def slope_per_hour(points):
    """Least-squares slope of (t_seconds, value) in value per hour."""
    n = len(points)
    if n < 3:
        return float("nan")
    mx = sum(t for t, _ in points) / n
    my = sum(v for _, v in points) / n
    den = sum((t - mx) ** 2 for t, _ in points)
    return sum((t - mx) * (v - my) for t, v in points) / den * 3600 if den else float("nan")


def table(head, rows):
    out = ["| " + " | ".join(head) + " |", "|" + "---|" * len(head)]
    for r in rows:
        out.append("| " + " | ".join(str(c) for c in r) + " |")
    return "\n".join(out)


def f(x, d=1):
    return "–" if x != x else f"{x:.{d}f}"


def main():
    path = sys.argv[1]
    pg = None
    if "--pg" in sys.argv:
        pg = sys.argv[sys.argv.index("--pg") + 1]
    rows = load(path)
    out = []
    out.append("# soak09a summary\n")

    ev = of(rows, "event")
    out.append("## Events\n")
    out.append(table(["t (min)", "event", "details"], [
        (f(e["t_s"] / 60), e["event"], ", ".join(f"{k}={v}" for k, v in e.items() if k not in ("event", "t_s"))) for e in ev]))

    wr = of(rows, "web_round")
    out.append("\n## Web load by phase (rounds of two minutes; percentiles are the median and the worst of the rounds)\n")
    phases = {}
    for r in wr:
        phases.setdefault(r["phase"], []).append(r)
    head = ["phase", "rounds", "requests", "req/s (median)", "ok", "busy 503", "timeouts", "other 5xx", "unexpected 4xx", "429", "transport"]
    body = []
    for ph, rs in phases.items():
        tot = {}
        for r in rs:
            for k, v in r["outcomes"].items():
                tot[k] = tot.get(k, 0) + v
        body.append((ph, len(rs), sum(r["requests"] for r in rs), f(med([r["rps"] for r in rs])), tot.get("ok", 0), tot.get("busy503", 0),
                     tot.get("timeout", 0), tot.get("other5xx", 0), tot.get("unexpected", 0), tot.get("rate429", 0) + tot.get("429", 0), tot.get("transport", 0)))
    out.append(table(head, body))
    out.append("\n### Latency by request class and phase (p50 / p95 / p99 in ms: median of rounds; max = worst p99 of any round)\n")
    body = []
    for ph, rs in phases.items():
        classes = {}
        for r in rs:
            for c, v in r["classes"].items():
                classes.setdefault(c, []).append(v)
        for c, vs in sorted(classes.items()):
            body.append((ph, c, sum(v["n"] for v in vs), f(med([v["p50_ms"] for v in vs])), f(med([v["p95_ms"] for v in vs])), f(med([v["p99_ms"] for v in vs])), f(max(v["p99_ms"] for v in vs))))
    out.append(table(["phase", "class", "n", "p50", "p95", "p99", "worst p99"], body))

    adm = of(rows, "admin_summary")
    if adm:
        a = adm[0]
        out.append(f"\n## Administrator requests\n\nok {a['ok']}, unexpected {a['bad']}.\n")
        body = [(c, v["n"], f(v["p50_ms"]), f(v["p95_ms"]), f(v["p99_ms"]), f(v["max_ms"])) for c, v in sorted(a["classes"].items())]
        out.append(table(["request", "n", "p50", "p95", "p99", "max"], body))

    samples = of(rows, "samples")
    if samples:
        sm = samples[0]["samples"]
        out.append("\n## CPU and memory by service (10 s samples, 30-minute windows)\n")
        for proc in ("web", "ingest"):
            ps = [s for s in sm if s["proc"] == proc]
            if not ps:
                continue
            body = []
            win = 1800
            t_end = max(s["t"] for s in ps)
            for w0 in range(0, int(t_end) + 1, win):
                seg = [s for s in ps if w0 <= s["t"] < w0 + win]
                if len(seg) < 2:
                    continue
                dt = seg[-1]["t"] - seg[0]["t"]
                # the counter restarts with the process (a restart of the service inside the window):
                # sum the positive steps only
                used = sum(max(b["cpu_sec"] - a["cpu_sec"], 0) if b["cpu_sec"] >= a["cpu_sec"] else b["cpu_sec"] for a, b in zip(seg, seg[1:]))
                cpu = used / dt * 100 if dt > 0 else float("nan")
                body.append((f"{w0 // 60}-{(w0 + win) // 60}", f(cpu), f(med([s["rss_mib"] for s in seg])), f(max(s["rss_mib"] for s in seg)),
                             f(max(s["cgroup_mib"] for s in seg)), max(s["threads"] for s in seg), f(med([s["fds"] for s in seg]), 0), max(s["fds"] for s in seg)))
            out.append(f"\n### {proc}\n")
            out.append(table(["window (min)", "CPU % of one core", "RSS MiB (median)", "RSS max", "cgroup max MiB", "threads max", "fds median", "fds max"], body))

    mon = of(rows, "monitor")
    if mon:
        out.append("\n## Goroutines, heap and PostgreSQL (monitor, 30-minute windows, medians)\n")
        body = []
        win = 1800
        t_end = max(m["t_s"] for m in mon)
        for w0 in range(0, int(t_end) + 1, win):
            seg = [m for m in mon if w0 <= m["t_s"] < w0 + win]
            if not seg:
                continue
            g = lambda key, sub: [m[key][sub] for m in seg if isinstance(m.get(key), dict) and sub in m[key]]
            body.append((f"{w0 // 60}-{(w0 + win) // 60}", f(med(g("diag_web", "goroutines")), 0), f(med(g("diag_ingest", "goroutines")), 0),
                         f(med(g("diag_web", "heap_alloc")) / 2**20 if g("diag_web", "heap_alloc") else float("nan")),
                         f(med(g("diag_ingest", "heap_alloc")) / 2**20 if g("diag_ingest", "heap_alloc") else float("nan")),
                         f(med([m["pg_connections"] for m in seg if "pg_connections" in m]), 0), max([m.get("pg_lock_waiters", 0) for m in seg]),
                         f(max([m.get("pg_oldest_tx_s", 0) for m in seg])), max([m.get("queue_rows", 0) for m in seg]),
                         max([m.get("dead_tuples_stats", 0) for m in seg]), max([m.get("dead_tuples_ingest", 0) for m in seg])))
        out.append(table(["window (min)", "web goroutines", "ingest goroutines", "web heap_alloc MiB", "ingest heap_alloc MiB", "PG conns", "lock waiters max",
                          "oldest tx s max", "queue rows max", "stats dead tuples max", "ingest dead tuples max"], body))
        errs = {}
        for m in mon:
            for k in m:
                if k.endswith("_error"):
                    errs[k] = errs.get(k, 0) + 1
        out.append(f"\nMonitor readings with an error field: {errs or 'none'}.\n")
        ev_web = [(m["t_s"], m["web_events"]) for m in mon if m.get("web_events")]
        ev_ing = [(m["t_s"], m["ingest_events"]) for m in mon if m.get("ingest_events")]
        if ev_web and ev_ing:
            out.append(f"cgroup memory events at the end: web {ev_web[-1][1]}, ingest {ev_ing[-1][1]}.\n")

    procs = {p["proc"]: p for p in of(rows, "process")}
    if procs:
        out.append("\n## Heap after GC (gctrace) and shutdown\n")
        body = []
        for name, p in procs.items():
            series = p.get("heap_after_gc_mib_series", [])
            n = len(series)
            first, last = series[: n // 3], series[-(n // 3):] if n >= 3 else []
            body.append((name, p.get("gc_count"), f(p.get("heap_after_gc_mib_max")), f(med(series)), f(med(first)), f(med(last)),
                         p.get("cgroup_peak_mib"), p.get("exit_code"), p.get("warn_error_lines")))
        out.append(table(["process", "GCs", "heap after GC max MiB", "median", "median, first third", "median, last third", "cgroup peak MiB", "exit", "WARN/ERROR lines"], body))

    ck = of(rows, "check")
    if ck:
        out.append("\n## End-state checks (after the graceful stop of ingest)\n")
        out.append(table(["check", "value"], [(k, v) for k, v in ck[0].items()]))

    if pg:
        def q(sql):
            r = subprocess.run(pg.split() + ["-Atc", sql], capture_output=True, text=True)
            return r.stdout.strip()
        out.append("\n## Persisted aggregates (from the benchmark database)\n")
        hits, miss, lookups, busy, fails = (int(x) for x in q("select sum(auth_cache_hits), sum(auth_cache_misses), sum(auth_db_lookups), sum(auth_busy), sum(auth_storage_failures) from magnetico_stats.agg_hour").split("|"))
        recs, subs, exr, exh = (int(x) for x in q("select sum(records_received), sum(submit_requests_validated), sum(exists_requests), sum(exists_hashes) from magnetico_stats.agg_hour").split("|"))
        chit, cmiss = (int(x) for x in q("select sum(cache_hit_positive + cache_hit_negative), sum(cache_miss) from magnetico_stats.agg_hour").split("|"))
        out.append(table(["figure", "value"], [
            ("authentication cache hit rate", f"{100 * hits / max(hits + miss, 1):.3f} % ({hits} hits, {miss} misses)"),
            ("authentication database lookups", f"{lookups} for {hits + miss} presented credentials"),
            ("authentication busy refusals / storage failures", f"{busy} / {fails}"),
            ("records per submit request (batch utilisation)", f"{recs / max(subs, 1):.1f} ({recs} records in {subs} requests)"),
            ("hashes per existence request", f"{exh / max(exr, 1):.1f} ({exh} hashes in {exr} requests)"),
            ("existence cache hit rate", f"{100 * chit / max(chit + cmiss, 1):.1f} %"),
        ]))
        hourly = q("select to_char(bucket_start,'MM-DD HH24:MI'), records_received, records_acknowledged, records_enqueued, settled_stored_new, bit_count(covered_mask::bit(64)) from magnetico_stats.agg_hour order by 1")
        rows_h = [l.split("|") for l in hourly.splitlines()]
        out.append("\n### Hourly aggregates\n")
        out.append(table(["hour (UTC)", "received", "acknowledged", "enqueued", "settled new", "covered minutes"], rows_h))
        sizes = q("select relname, pg_size_pretty(pg_total_relation_size(c.oid)) from pg_class c join pg_namespace n on n.oid=c.relnamespace where n.nspname='magnetico_stats' and relkind='r' order by pg_total_relation_size(c.oid) desc limit 6")
        out.append("\n### Statistics tables after the run\n")
        out.append(table(["table", "size"], [l.split("|") for l in sizes.splitlines()]))
    print("\n".join(out))


if __name__ == "__main__":
    main()
