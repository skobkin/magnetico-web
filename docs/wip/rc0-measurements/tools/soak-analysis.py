"""RC0 soak final analysis: memory trend (10-s samples), heap after GC per process, process
reports, conservation, drain. Reads the soak JSONL; prints Markdown. Read-only."""
import json
import statistics
import sys


def theil_sen(xs, ys):
    n = len(xs)
    step = max(1, n // 400)  # pairwise slopes on a subsample: O(400^2)
    xs, ys = xs[::step], ys[::step]
    sl = [(ys[j] - ys[i]) / (xs[j] - xs[i]) for i in range(len(xs)) for j in range(i + 1, len(xs)) if xs[j] != xs[i]]
    return statistics.median(sl) if sl else float("nan")


def lsq(xs, ys):
    mx, my = statistics.fmean(xs), statistics.fmean(ys)
    den = sum((x - mx) ** 2 for x in xs)
    return sum((x - mx) * (y - my) for x, y in zip(xs, ys)) / den if den else float("nan")


recs = [json.loads(l) for l in open(sys.argv[1])]
by = {}
for r in recs:
    by.setdefault(r["kind"], []).append(r["data"])

start = by["start"][0]
print("## start\n")
print({k: start[k] for k in ("image", "web_gomemlimit", "web_mem", "machine") if k in start})

samples = by.get("samples", [{}])[-1].get("samples", [])
print(f"\n## 10-s samples: {len(samples)} kept\n")
# soak minute = sampler t / 60 (the sampler starts with the soak clock, within seconds)
SEG = {"web": [(0, 100), (100, 900), (900, 1440), (1440, 1600)], "ingest": [(0, 600), (600, 1440), (1440, 1600)]}
for proc in ("web", "ingest"):
    s = [x for x in samples if x["proc"] == proc]
    if not s:
        continue
    print(f"### {proc}\n")
    print("| window (min) | n | RSS MiB median | RSS max | cgroup MiB median | cgroup max | threads max | fds max |")
    print("|---|---|---|---|---|---|---|---|")
    for a in range(0, 1600, 60):
        w = [x for x in s if a <= x["t"] / 60 < a + 60]
        if w:
            print(f"| {a}-{a + 60} | {len(w)} | {statistics.median(x['rss_mib'] for x in w):.0f} | {max(x['rss_mib'] for x in w):.0f} | "
                  f"{statistics.median(x['cgroup_mib'] for x in w):.0f} | {max(x['cgroup_mib'] for x in w):.0f} | "
                  f"{max(x['threads'] for x in w)} | {max(x['fds'] for x in w)} |")
    print()
    for a, b in SEG[proc]:
        # skip 10 min of warm-up after each (re)start and the load transitions
        w = [x for x in s if a + 10 <= x["t"] / 60 < b]
        if len(w) < 20:
            continue
        hrs = [x["t"] / 3600 for x in w]
        for key in ("rss_mib", "cgroup_mib"):
            ys = [x[key] for x in w]
            med = statistics.median(ys)
            ts, ls = theil_sen(hrs, ys), lsq(hrs, ys)
            print(f"- {proc} {key} segment {a + 10}-{b} min: median {med:.0f} MiB, Theil-Sen {ts:+.2f} MiB/h ({100 * ts / med:+.2f} %/h), "
                  f"least squares {ls:+.2f} MiB/h ({100 * ls / med:+.2f} %/h)")
    print()

print("## process reports\n")
for p in by.get("process", []):
    series = p.pop("heap_after_gc_mib_series", [])
    print(json.dumps(p, sort_keys=True))
    if series:
        n = len(series)
        thirds = [series[: n // 3], series[n // 3: 2 * n // 3], series[2 * n // 3:]]
        print(f"  heap after GC series ({n} points, thinned, no timestamps): thirds median "
              + " / ".join(f"{statistics.median(t):.0f}" for t in thirds) + f" MiB, max {max(series):.0f}")
        # restarts show as a drop to a small heap
        resets = [i for i in range(1, n) if series[i] < 40 and series[i - 1] >= 60]
        print(f"  apparent restarts at indices {resets}")
        bounds = [0] + resets + [n]
        for i in range(len(bounds) - 1):
            seg = series[bounds[i]:bounds[i + 1]]
            seg = seg[len(seg) // 20:]  # drop the warm-up 5 %
            if len(seg) >= 9:
                m = len(seg)
                f, l = statistics.median(seg[: m // 3]), statistics.median(seg[2 * m // 3:])
                print(f"  segment {i}: {m} points, first third median {f:.0f}, last third {l:.0f}, max {max(seg):.0f} MiB")
print()
for k in ("check", "drain", "queue_samples", "heap_checkpoint", "result", "gates"):
    for d in by.get(k, []):
        print(k, json.dumps(d, sort_keys=True)[:1500])
print("\nkinds:", {k: len(v) for k, v in by.items()})
