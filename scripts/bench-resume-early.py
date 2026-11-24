#!/usr/bin/env python3
import pathlib,json,subprocess,time,os,threading,signal,statistics,sys
import argparse,secrets
parser=argparse.ArgumentParser(description="Validate and time ordinary local/remote live snapshots; requires root/KVM and an already configured Manifest Store.")
parser.add_argument('--bin',required=True,type=pathlib.Path)
parser.add_argument('--config',required=True,type=pathlib.Path)
parser.add_argument('--manifest-config',required=True,type=pathlib.Path)
parser.add_argument('--output',required=True,type=pathlib.Path)
parser.add_argument('--run-root',default='/run/kuasar-resume-early-bench')
parser.add_argument('--iterations',type=int,default=50)
args=parser.parse_args()
prefix="fo-"+secrets.token_hex(3)
if args.iterations<1:parser.error('--iterations must be positive')
B=args.bin.resolve();W=args.output.resolve();W.mkdir(parents=True,exist_ok=False)
run=args.run_root;pathlib.Path(run).mkdir(parents=True,exist_ok=True);manifest=args.manifest_config.resolve()

def call(*a,timeout=60):
 p=subprocess.run([str(x)for x in a],capture_output=True,text=True,timeout=timeout)
 if p.returncode:raise RuntimeError(str(a[:3])+': '+p.stderr[-4000:])
 return p.stdout
class VM:
 def __init__(self,sid,cfg,ref=None):
  self.sid=sid;self.ticks=[];self.lines=[];self.log=(W/(sid+'.log')).open('w');cp=W/(sid+'.json');cp.write_text(json.dumps(cfg));args=[str(B/'sandbox-ctl'),'run','--config',str(cp),'--manifest-config',str(manifest),'--sandbox-id',sid,'--run-root',run,'--base-root',str(W/'base'),'--ch-binary',str(B/'cloud-hypervisor'),'--enable-checkpoint','--stats-interval','0']
  if ref:args+=['--restore',ref]
  self.p=subprocess.Popen(args,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True,bufsize=1)
  def read():
   for line in self.p.stdout:
    now=time.monotonic_ns();self.log.write(line);self.log.flush();self.lines.append((now,line.rstrip()))
    if line.startswith('TICK '):
     a=line.split();self.ticks.append((now,a[1],int(a[2])))
  self.t=threading.Thread(target=read,daemon=True);self.t.start()
  try:self.wait_ticks(3)
  except BaseException:
   self.close();raise
 def wait_ticks(self,n):
  end=time.monotonic()+30
  while len(self.ticks)<n:
   if self.p.poll()!=None:raise RuntimeError(self.sid+' exited: '+''.join(l+'\n'for _,l in self.lines[-20:]))
   if time.monotonic()>end:raise TimeoutError(self.sid+' ticks')
   time.sleep(.005)
 def py(self,code):return call(B/'sandbox-ctl','exec','--sandbox-id',self.sid,'--run-root',run,'--','/usr/local/bin/python3','-c',code).strip()
 def close(self):
  if self.p.poll()==None:self.p.terminate()
  try:self.p.wait(timeout=20)
  except subprocess.TimeoutExpired:self.p.kill();self.p.wait();raise
  self.t.join(3);self.log.close()

cold=json.loads(args.config.read_text())
cold['launch']['args']=['-c',"import os,time,secrets\nnonce=secrets.token_hex(8)\ni=0\nwhile True:\n i+=1\n print('TICK',nonce,i,flush=True)\n time.sleep(.02)"]
host={'resources':cold['resources'],'boot':{'kernel':cold['boot']['kernel'],'runtime':cold['boot']['runtime']}}
results=[]
for mode in ['local','remote']:
 parent=VM(prefix+'-'+mode,cold);child=None
 try:
  initial_fd=len(list((pathlib.Path('/proc')/str(parent.p.pid)/'fd').iterdir()))
  for i in range(args.iterations+1):
   before_value=f'{mode}-{i}-before';parent.py("import os; f=open('/snapshot-state','w'); f.write("+repr(before_value)+"); f.flush(); os.fsync(f.fileno()); f.close()")
   last=parent.ticks[-1];start=time.monotonic_ns();outdir=W/'snapshots'/mode/str(i)
   command=[B/'sandbox-ctl','snapshot', '--resume-early','--sandbox-id',parent.sid,'--run-root',run,'--json']
   command+=['--output',outdir]if mode=='local'else['--upload']
   result=json.loads(call(*command,timeout=120));end=time.monotonic_ns()
   assert result['type']=='snapshot_done'
   assert any(start<t<end for t,_,_ in parent.ticks),'parent made no progress during save'
   parent.py("import os; f=open('/snapshot-state','w'); f.write('PARENT-AFTER'); f.flush(); os.fsync(f.fileno()); f.close()")
   ref=result['snapshot_ref']
   if mode=='local':ref='file://'+str(outdir/ref.removeprefix('file://'))
   child=VM(f'{prefix}-{mode}-c{i}',host,ref)
   assert child.ticks[0][1]==last[1]and child.ticks[0][2]>=last[2],('memory continuation',last,child.ticks[:3])
   assert child.py("print(open('/snapshot-state').read())")==before_value
   child.py("import os; f=open('/snapshot-state','w'); f.write('CHILD-AFTER'); f.flush(); os.fsync(f.fileno()); f.close()")
   assert parent.py("print(open('/snapshot-state').read())")=='PARENT-AFTER'
   assert child.py("print(open('/snapshot-state').read())")=='CHILD-AFTER'
   if i==args.iterations:
    parent.close();parent=None
    assert child.py("print(open('/snapshot-state').read())")=='CHILD-AFTER'
   child.close();child=None
   if parent:
    assert parent.py("print(open('/snapshot-state').read())")=='PARENT-AFTER'
    fd=len(list((pathlib.Path('/proc')/str(parent.p.pid)/'fd').iterdir()))
   else:fd=None
   row={'mode':mode,'iteration':i,'warmup':i==0,'pause_ms':result['application_pause_ms'],'save_ms':result['save_complete_ms'],'client_ms':(end-start)/1e6,'parent_progress_during_save':True,'restored_memory_and_file':True,'independent_writes':True,'independent_destroy':True,'parent_fd_start':initial_fd,'parent_fd_after':fd,'snapshot_ref':ref,'sandbox_ref':result['sandbox_ref']}
   results.append(row);(W/'results.json').write_text(json.dumps(results,indent=2));print(json.dumps(row),flush=True)
 finally:
  if child:child.close()
  if parent:parent.close()
summary={}
for mode in ['local','remote']:
 a=[r for r in results if r['mode']==mode and not r['warmup']];summary[mode]={'n':len(a),**{k:{'median':statistics.median(r[k]for r in a),'max':max(r[k]for r in a)}for k in ['pause_ms','save_ms','client_ms']}}
(W/'summary.json').write_text(json.dumps(summary,indent=2));print(json.dumps(summary),flush=True)
