#!/usr/bin/env python3
"""Read-only validation mode: no Feishu, no memory/ledger writes, no expected=5."""
import datetime,json,os,selectors,subprocess,time
APP='80beff42ab0b41819f9c1c3c3ac95413308e70eb'
result={'mode':'l4_validation_only','app_id':APP,'cluster_id':60,'source':'official ccloud-cli via fixed read-only proxy','observed_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'status':'unknown','desired_replicas':None,'expectation_status':'unknown','note':'historical expect_replicas=5 is not current desired evidence'}
proc=None
try:
 # Preserve the host's controlled egress path for the CLI; do not forward unrelated runtime options.
 env={k:v for k,v in os.environ.items() if k in {'PATH','HOME','CCLOUD_API_KEY','CCLOUD_API_BASE_URL','SSL_CERT_FILE','NODE_EXTRA_CA_CERTS','HTTPS_PROXY','HTTP_PROXY','ALL_PROXY','NO_PROXY','https_proxy','http_proxy','all_proxy','no_proxy'}}
 env['CCLOUD_API_MODE']='real';env['CCLOUD_DISABLE_UPDATE_CHECK']='1'
 proc=subprocess.Popen(['/tmp/oma-ccloud/bin/ccloud-cli','app','pods','--app-id',APP,'--cluster-id','60'],stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,env=env)
 sel=selectors.DefaultSelector();sel.register(proc.stdout,selectors.EVENT_READ);raw=bytearray();deadline=time.monotonic()+45
 while True:
  if time.monotonic()>deadline:raise RuntimeError('deadline')
  if not sel.select(0.2):continue
  part=os.read(proc.stdout.fileno(),8192)
  if not part:break
  raw.extend(part)
  if len(raw)>256*1024:raise RuntimeError('output_limit')
 sel.close();code=proc.wait(timeout=max(0.1,deadline-time.monotonic()));result['cli_exit_code']=code
 if code!=0:raise RuntimeError('query_failed')
 payload=json.loads(raw);pods=payload.get('data')
 if not isinstance(pods,list) or any(not isinstance(p,dict) or type(p.get('ready')) is not bool or type(p.get('restartCount')) is not int or p['restartCount']<0 or not p.get('phase') for p in pods):raise RuntimeError('invalid_observation')
 result.update(status='observed',pod_count=len(pods),running=sum(p['phase']=='Running' for p in pods),ready_running=sum(p['phase']=='Running' and p['ready'] for p in pods),restarts_total=sum(p['restartCount'] for p in pods))
except Exception as e:
 result['reason']=str(e) if isinstance(e,RuntimeError) else type(e).__name__
finally:
 if proc is not None and proc.poll() is None:proc.kill();proc.wait(timeout=3)
print(json.dumps(result,ensure_ascii=False))
