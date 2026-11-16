import argparse,datetime,hashlib,json,os,subprocess
from pathlib import Path
p=argparse.ArgumentParser()
p.add_argument('binary',type=Path);p.add_argument('output',type=Path)
p.add_argument('--count',type=int,default=14);p.add_argument('--time',default='500ms');p.add_argument('--cpu',type=int,default=1)
p.add_argument('--workloads',default='CancelableHelloJSON,CancelableDuplicateInvoice')
a=p.parse_args();a.output.mkdir(exist_ok=False,parents=True)
frameworks=['trest_app','gin','echo'];workloads=a.workloads.split(',')
meta={'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'binary':str(a.binary),'sha256':hashlib.sha256(a.binary.read_bytes()).hexdigest(),'count':a.count,'benchtime':a.time,'gomaxprocs':a.cpu,'order':[]}
env=dict(os.environ,GOMAXPROCS=str(a.cpu),GIN_MODE='release')
with (a.output/'samples.txt').open('w') as output:
 for round in range(a.count):
  shift=round%len(frameworks);order=frameworks[shift:]+frameworks[:shift]
  if (round//len(frameworks))%2:order.reverse()
  for name in order:
   for workload in (workloads if round%2==0 else workloads[::-1]):
    result=subprocess.check_output([str(a.binary),'-test.run=^$','-test.bench=^Benchmark'+workload+'$/^framework='+name+'$','-test.benchtime='+a.time,'-test.benchmem','-test.count=1'],env=env,text=True)
    output.write(result);output.flush();meta['order'].append([round+1,name,workload])
  print('Completed current round',round+1,'of',a.count,flush=True)
meta['completed_at']=datetime.datetime.now(datetime.timezone.utc).isoformat()
(a.output/'run.json').write_text(json.dumps(meta,indent=2)+'\n')
with (a.output/'benchstat.txt').open('w') as output:subprocess.run(['benchstat','-row','.name','-col','/framework@(gin echo trest_app)',str(a.output/'samples.txt')],stdout=output,check=True)
print((a.output/'benchstat.txt').read_text())
