"""Live heap after GC per hour (UTC) from `docker logs -t` output of a service run with GODEBUG=gctrace=1.

Usage: python3 -I gctrace-hourly.py <log>...  (prints n, min, median, p90, max in MB per hour)."""
import sys,re,statistics,collections
rx=re.compile(r'^(\S+) gc \d+ @[\d.]+s .*?, (\d+)->(\d+)->(\d+) MB, (\d+) MB goal')
for path in sys.argv[1:]:
    w=collections.defaultdict(list)
    for l in open(path):
        m=rx.match(l)
        if m: w[m.group(1)[:13]].append(int(m.group(4)))
    print('==',path.rsplit('/',1)[1],'(hour UTC: n, live-after-GC MB min/median/p90/max)')
    for h in sorted(w):
        v=sorted(w[h]); print(h, len(v), v[0], statistics.median(v), v[int(len(v)*.9)], v[-1])
