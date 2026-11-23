import json,sys
ms=lambda ns: round(ns/1e6,1)
for l in open(sys.argv[1]):
    r=json.loads(l); s=r['server']; res=r['results']
    print(f"## {r['label']} / {r['scenario']}")
    if isinstance(res,list):
        for c in res: print("  ",c['dataset'],"cold %.0fms warm %.1fms rss %.0f"%(c['ColdMs'],c['WarmMs'],c['RSSAfterMiB']))
    else:
        for k,v in res.items():
            o=v['ok_latency']
            print(f"   {k:10} n={v['n']} codes={v['codes']} err={v['transport_errors']} rps={v['req_per_sec']:.0f} ok p50={ms(o['P50'])} p95={ms(o['P95'])} p99={ms(o['P99'])} max={ms(o['Max'])}")
    print(f"   server rss_peak={s['PeakRSSMiB']:.0f} hwm={s['HWMMiB']:.0f} cg_peak={s['CgroupPeakMiB']:.0f} oom_kill={s['OOMKills']} high={s['High']} gcs={s['GCs']} gcpause={s['GCPauseMs']:.0f}ms maxheap={s['MaxHeapMiB']:.0f} exit={s['ExitCode']} shut={s['ShutdownMs']}ms fds={s['FDsStart']}->{s['FDsEnd']} pg={s['PGStart']}->{s['PGEnd']} thr={s['ThreadsMax']}")
    print("   ",s['CacheLine'].split('msg=')[-1][:330])
