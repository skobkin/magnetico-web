import json,sys,statistics,collections
MiB=1<<20
def load(p):
    rows=[json.loads(l) for l in open(p)]
    g=collections.defaultdict(list)
    for r in rows: g[(r['Dataset'],r['Blob'],r['Mode'],r['Conc'])].append(r)
    out={}
    for k,v in g.items():
        v.sort(key=lambda r:r['ColdWallMs']); out[k]=v[len(v)//2]; out[k]['_n']=len(v)
        out[k]['_min']=v[0]['ColdWallMs']; out[k]['_max']=v[-1]['ColdWallMs']
        out[k]['_hwm_max']=max(x['HWM']-x['BaseRSS'] for x in v)
        out[k]['_peak_max']=max(x['PeakHeap']-x['BaseHeap'] for x in v)
    return out
def W0(r):
    n=r['Files']; return r['Stored']+r['Declared']+24*n+min(n*65536,64*MiB)
mode=sys.argv[2]
d=load(sys.argv[1])
if mode=='weight':
    print("| dataset | blob | files | stored | declared | P actual | P plan(min(n*64K,64MiB)) | W0 MiB | decode-only peak heap | decode+arena peak heap | HWM-Δ+stored (arena) | peak/W0 | RSS/W0 | alloc(decode)/declared | gate admits |")
    print("|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for (ds,b,m,c),r in sorted(d.items()):
        if m!='arena' or c!=1: continue
        dec=d.get((ds,b,'decode',1))
        w=W0(r); n=r['Files']
        peak=r['_peak_max']; rss=r['_hwm_max']+r['Stored']
        print(f"| {ds} | {b} | {n} | {r['Stored']/MiB:.1f} | {r['Declared']/MiB:.1f} | {r['PathBytes']/MiB:.1f} | {min(n*65536,64*MiB)/MiB:.1f} | {w/MiB:.1f} | {(dec['_peak_max'] if dec else 0)/MiB:.1f} | {peak/MiB:.1f} | {rss/MiB:.1f} | {peak/w:.2f} | {rss/w:.2f} | {r['DecodeAlloc']/max(r['Declared'],1):.1f}x | {int(256*MiB//w)} |")
