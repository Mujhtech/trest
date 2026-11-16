import argparse,datetime,hashlib,json,os,re,subprocess
from pathlib import Path
p=argparse.ArgumentParser()
p.add_argument('before',type=Path)
p.add_argument('after',type=Path)
p.add_argument('output',type=Path)
p.add_argument('--count',type=int,default=6)
p.add_argument('--time',default='300ms')
p.add_argument('--frameworks',default='trest_app,gin,echo')
p.add_argument('--workloads',default='HelloJSON,DuplicateInvoice')
a=p.parse_args()
a.output.mkdir(parents=True,exist_ok=False)
env=dict(os.environ,GIN_MODE='release',GOMAXPROCS='1')
workloads=a.workloads.split(',')
frameworks=a.frameworks.split(',')
meta={'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'count':a.count,'benchtime':a.time,'gomaxprocs':1,'binaries':{x:{'path':str(y),'sha256':hashlib.sha256(y.read_bytes()).hexdigest()} for x,y in [('before',a.before),('after',a.after)]},'order':[]}
outputs={name:(a.output/(name+'.txt')).open('w') for name in ['before','after']}
for repeat in range(a.count):
    shift=repeat%len(frameworks)
    order=frameworks[shift:]+frameworks[:shift]
    if (repeat//len(frameworks))%2: order.reverse()
    for framework in order:
        for workload in (workloads if repeat%2==0 else workloads[::-1]):
            pair=['before','after'] if repeat%2==0 else ['after','before']
            for variant in pair:
                binary=getattr(a,variant)
                result=subprocess.check_output([str(binary),'-test.run=^$','-test.bench=^Benchmark'+workload+'$/^framework='+framework+'$','-test.benchtime='+a.time,'-test.benchmem','-test.count=1'],text=True,env=env)
                if len(re.findall(r'^Benchmark\w+/framework=',result,re.M))!=1:raise RuntimeError(result)
                outputs[variant].write(result)
                outputs[variant].flush()
                meta['order'].append([repeat+1,framework,workload,variant])
    print('Finished pair round',repeat+1,'of',a.count,flush=True)
for out in outputs.values():out.close()
meta['completed_at']=datetime.datetime.now(datetime.timezone.utc).isoformat()
(a.output/'run.json').write_text(json.dumps(meta,indent=2)+'\n')
subprocess.run(['benchstat',str(a.output/'before.txt'),str(a.output/'after.txt')],stdout=(a.output/'benchstat.txt').open('w'),check=True)
print((a.output/'benchstat.txt').read_text(),flush=True)
