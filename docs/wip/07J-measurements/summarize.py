#!/usr/bin/env python3
"""Summarise a W9 load run (JSONL written by TestW9Load) as Markdown.

usage: summarize.py <run.jsonl> [--warmup-min N] [--block-min N]

Segments: outcomes per class (never merged), throughput (successful requests
per second) and latency percentiles of the successful ones. Processes: peaks,
GC, exit codes. For a soak, the plan's memory windows: the warm-up phase is
excluded, the steady window is reduced to block medians (10 minutes unless
--block-min says otherwise) and the slope of heap-after-GC and RSS over the
steady window is reported with its share of the plateau per hour. The slope
thresholds (2 % per hour for heap-after-GC, 5 % per hour for RSS) are screening
heuristics, not leak verdicts.
"""
import json, sys, statistics as st

def load(path):
    rows = [json.loads(l) for l in open(path)]
    return rows

def slope(xs, ys):
    n = len(xs)
    if n < 2: return 0.0
    mx, my = sum(xs)/n, sum(ys)/n
    den = sum((x-mx)**2 for x in xs)
    return sum((x-mx)*(y-my) for x, y in zip(xs, ys))/den if den else 0.0

def blocks(points, t0, t1, width):
    """Median of values per block of `width` seconds in [t0, t1)."""
    out = []
    t = t0
    while t + width <= t1 + 1e-9:
        vs = [v for (tt, v) in points if t <= tt < t + width]
        if vs: out.append((t + width/2, st.median(vs)))
        t += width
    return out

def main():
    path = sys.argv[1]
    block_min = 10.0
    if '--block-min' in sys.argv: block_min = float(sys.argv[sys.argv.index('--block-min')+1])
    rows = load(path)
    start = next((r['data'] for r in rows if r['kind'] == 'start'), {})
    print(f"# Run `{rows[0]['label']}`\n")
    print(f"- git: `{start.get('git','?')}`; machine: {start.get('machine','?')}")
    print(f"- web cgroup memory.max {start.get('web_mem')}, GOMEMLIMIT {start.get('web_gomemlimit')}; pools {start.get('pools')}\n")
    segs = [r['data'] for r in rows if r['kind'] == 'segment']
    print("## Segments\n")
    print("| segment | users/workers | think | s | ok/s | ok | 429 | busy503 | timeout503 | other5xx | unexpected4xx | transport | search p50/p95 (ms) | details p95 | dirs p95 | tree p95 | api p95 | rss p95 |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for d in segs:
        o = d['outcomes']; c = d['classes']
        def p(cl, k): return '%.0f' % c[cl][k] if cl in c else '-'
        print(f"| {d['segment']} | {d['concurrency']} | {d.get('think_ms',0)} ms | {d['seconds']:.0f} | {d.get('rps_ok', d['rps']):.0f} | {o.get('ok',0)} | {o.get('limited429',0)} | {o.get('busy503',0)} | {o.get('timeout503',0)} | {o.get('other5xx',0)} | {o.get('unexpected4xx',0)} | {o.get('transport',0)} | {p('search','p50_ms')}/{p('search','p95_ms')} | {p('details','p95_ms')} | {p('dirs','p95_ms')} | {p('tree','p95_ms')} | {p('api','p95_ms')} | {p('rss','p95_ms')} |")
    ing = [d for d in segs if d.get('ingest_sent') is not None]
    for d in ing:
        print(f"\nIngest during `{d['segment']}`: sent {d['ingest_sent']}, acknowledged (202) {d['ingest_acked']}, refused {d['ingest_refused']}.")
    for r in rows:
        if r['kind'] == 'drain': print(f"\nDrain: {r['data']}")
    print("\n## Processes\n")
    print("| process | exit | VmHWM MiB | cgroup peak MiB | OOM events | GC count | GC pause total ms | max heap-after-GC MiB |")
    print("|---|---|---|---|---|---|---|---|")
    procs = [r['data'] for r in rows if r['kind'] == 'process']
    for d in procs:
        ev = d.get('memory_events', {})
        print(f"| {d['proc']} | {d['exit_code']} | {d['vmhwm_mib']:.0f} | {d['cgroup_peak_mib']:.0f} | {ev.get('oom_kill',0)}/{ev.get('oom',0)} (high {ev.get('high',0)}, max {ev.get('max',0)}) | {d['gc_count']} | {d['gc_pause_ms_total']:.0f} | {d.get('heap_after_gc_mib_max','-')} |")
        if d.get('shutdown_stats'): print(f"\n  `{d['proc']}` shutdown stats: {d['shutdown_stats'][:400]}")
        if d.get('warn_error_lines') is not None: print(f"\n  `{d['proc']}` WARN/ERROR log lines: {d['warn_error_lines'] or 'none'}")
    # soak windows
    steady = next((s for s in segs if s['segment'] == 'steady'), None)
    if steady:
        warm = next((s for s in segs if s['segment'] == 'warmup'), None)
        idle = next((s for s in segs if s['segment'] == 'idle'), None)
        t0, t1 = steady['t_start'], steady['t_end']
        sm = next(r['data'] for r in rows if r['kind'] == 'samples')['samples']
        web = [x for x in sm if x['proc'] == 'web']
        print(f"\n## Memory windows (web), steady window {t0/60:.0f}–{t1/60:.0f} min\n")
        wd = block_min*60
        for label, key in (("RSS MiB", 'rss_mib'), ("cgroup MiB", 'cgroup_mib')):
            pts = [(x['t'], x[key]) for x in web]
            bl = blocks(pts, t0, t1, wd)
            sl = slope([b[0] for b in bl], [b[1] for b in bl]) * 3600
            plateau = st.median([b[1] for b in bl]) if bl else 0
            print(f"- {label}: block medians {[round(b[1]) for b in bl]}; slope {sl:+.1f} MiB/h = {100*sl/plateau if plateau else 0:+.1f} % of the plateau per hour")
        age = start.get('web_age_at_sampler_start_s', 0)
        gc = next((d for d in procs if d['proc'] == 'web'), {}).get('gc_series', [])
        pts = [(g['T'] - age, g['LiveMB']) for g in gc]
        bl = blocks(pts, t0, t1, wd)
        if bl:
            sl = slope([b[0] for b in bl], [b[1] for b in bl]) * 3600
            plateau = st.median([b[1] for b in bl])
            print(f"- heap-after-GC MiB: block medians {[round(b[1]) for b in bl]}; slope {sl:+.1f} MiB/h = {100*sl/plateau if plateau else 0:+.1f} % of the plateau per hour")
        fds = [x['fds'] for x in web]; th = [x['threads'] for x in web]
        print(f"- file descriptors {min(fds)}–{max(fds)}, threads {min(th)}–{max(th)} (whole run)")
        if idle:
            ip = [x for x in web if idle['t_start'] <= x['t'] <= idle['t_end']]
            if ip:
                print(f"- idle phase: RSS {ip[0]['rss_mib']:.0f} → {ip[-1]['rss_mib']:.0f} MiB, cgroup {ip[0]['cgroup_mib']:.0f} → {ip[-1]['cgroup_mib']:.0f} MiB, fds {ip[0]['fds']} → {ip[-1]['fds']}, threads {ip[0]['threads']} → {ip[-1]['threads']}")
        q = next(r['data'] for r in rows if r['kind'] == 'samples')['ingest_queue']
        qs = [(x['t'], x['v']) for x in q if t0 <= x['t'] <= t1]
        if qs:
            print(f"- ingest queue depth in the steady window: max {max(v for _, v in qs):.0f}, median {st.median(v for _, v in qs):.0f}, slope {slope([t for t,_ in qs],[v for _,v in qs])*3600:+.1f} rows/h")
    diags = [r['data'] for r in rows if r['kind'] == 'diag']
    if diags:
        print("\n## Diagnostic readings (one process)\n")
        print("| label | t (min) | goroutines | heap in use MiB | heap alloc MiB | file cache bytes MiB / entries | pool acquired/idle/total |")
        print("|---|---|---|---|---|---|---|")
        for d in diags:
            fc = d.get('file_cache', {}); pool = d.get('pool', {})
            print(f"| {d['label']} | {d['t']/60:.1f} | {d['goroutines']} | {d['heap_inuse']/2**20:.0f} | {d['heap_alloc']/2**20:.0f} | {fc.get('Bytes',0)/2**20:.0f} / {fc.get('Entries','-')} | {pool.get('acquired')}/{pool.get('idle')}/{pool.get('total')} |")
        first, last = diags[0], diags[-1]
        print(f"\nGoroutines by creation site, first reading: {[(g['where'], g['n']) for g in first['goroutine_groups'][:8]]}")
        print(f"\nGoroutines by creation site, last reading: {[(g['where'], g['n']) for g in last['goroutine_groups'][:8]]}")

main()
