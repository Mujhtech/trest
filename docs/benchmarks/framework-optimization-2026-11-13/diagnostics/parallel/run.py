import datetime,hashlib,json,os,subprocess
from pathlib import Path
root=Path('/tmp/trest-framework-next'); out=root/'parallel-results'; out.mkdir(exist_ok=False)
binary=root/'parallel.test'
frameworks=['trest_app','gin','echo']
workloads=['ParallelHelloJSON','ParallelDuplicateInvoice','ParallelCancelableHelloJSON','ParallelCancelableDuplicateInvoice']
meta={'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'binary_sha256':hashlib.sha256(binary.read_bytes()).hexdigest(),'count':6,'benchtime':'300ms','order':[]}
with (out/'samples.txt').open('w') as f:
 for repeat in range(6):
  shifted=frameworks[repeat%3:]+frameworks[:repeat%3]
  if repeat>=3: shifted.reverse()
  for cpu in ([2,4] if repeat%2==0 else [4,2]):
   for name in shifted:
    for work in (workloads if repeat%2==0 else workloads[::-1]):
     env=dict(os.environ,GOMAXPROCS=str(cpu),GIN_MODE='release')
     text=subprocess.check_output([str(binary),'-test.run=^$','-test.bench=^Benchmark'+work+'$/^framework='+name+'$','-test.benchtime=300ms','-test.benchmem','-test.count=1'],env=env,text=True)
     f.write(text);f.flush();meta['order'].append([repeat+1,cpu,name,work])
  print('Completed parallel round',repeat+1,'of 6',flush=True)
meta['completed_at']=datetime.datetime.now(datetime.timezone.utc).isoformat()
(out/'run.json').write_text(json.dumps(meta,indent=2)+'\n')
with (out/'benchstat.txt').open('w') as f: subprocess.run(['benchstat','-row','.name,/gomaxprocs','-col','/framework@(gin echo trest_app)',str(out/'samples.txt')],stdout=f,check=True)
print((out/'benchstat.txt').read_text())
