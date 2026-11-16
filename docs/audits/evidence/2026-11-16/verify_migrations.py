"""验证迁移 32 的真 ledger 升级及 down/up；只创建和清理独立 Docker 容器。"""
import argparse, json, pathlib, shutil, subprocess, tempfile, time, uuid
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('repository', type=pathlib.Path)
parser.add_argument('--image', default='migrate/migrate:v4.18.3')
parser.add_argument('--platform', default='linux/amd64')
parser.add_argument('--output', type=pathlib.Path, required=True)
options = parser.parse_args()
src = options.repository.resolve()
image = options.image
name='alert-migration-audit-'+uuid.uuid4().hex[:10]
results=[]
def run(args):
 r=subprocess.run(args,capture_output=True,text=True)
 if r.returncode:
  raise RuntimeError('command failed: '+str(args[:4])+'\n'+r.stderr[-2500:]+r.stdout[-1500:])
 return r.stdout.strip()
def sql(db,q):
 return run(['docker','exec',name,'psql','-U','postgres','-d',db,'-v','ON_ERROR_STOP=1','-Atc',q])
with tempfile.TemporaryDirectory(prefix='alert-migrations-') as td:
 target=pathlib.Path(td)/'migrations';shutil.copytree(src/'migrations',target)
 def migrate(db,*args):
  return run(['docker','run','--rm','--platform',options.platform,'--network','container:'+name,'-v',str(target)+':/migrations:ro',image,'-path=/migrations','-database=postgres://postgres:audit-test-only@localhost:5432/'+db+'?sslmode=disable',*args])
 try:
  run(['docker','run','-d','--name',name,'-e','POSTGRES_PASSWORD=audit-test-only','postgres:16-alpine'])
  for _ in range(100):
   r=subprocess.run(['docker','exec',name,'pg_isready','-U','postgres'],capture_output=True)
   if r.returncode==0:break
   time.sleep(.2)
  else:raise RuntimeError('isolated PostgreSQL startup timeout')
  latest=max(int(p.name.split('_')[0]) for p in target.glob('*.up.sql'))
  assert latest==32,latest
  for start in (None,29,31):
   db='audit_'+str(start or 'empty')
   sql('postgres','CREATE DATABASE '+db)
   if start is not None:
    migrate(db,'goto',str(start))
    assert sql(db,'SELECT version::text || \'/\' || dirty::text FROM schema_migrations')==str(start)+'/false'
    sql(db,"INSERT INTO teams(name) VALUES('migration-preserve-sentinel')")
    sql(db,"INSERT INTO faults(correlation_key,scope,status,severity,kind,title,confidence,first_detected_at,last_evaluated_at,owner_notified_at) VALUES('migration-sentinel','capability','active','critical','audit','sentinel',1,now(),now(),'2026-01-01T00:00:00Z')")
   migrate(db,'up')
   assert sql(db,'SELECT version::text || \'/\' || dirty::text FROM schema_migrations')=='32/false'
   assert sql(db,"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('fault_notification_watches','fault_notification_deliveries')")=='2'
   if start is not None:
    assert sql(db,"SELECT count(*) FROM teams WHERE name='migration-preserve-sentinel'")=='1'
    assert sql(db,"SELECT count(*) FROM faults WHERE correlation_key='migration-sentinel' AND status='active' AND activation_version=1 AND owner_notified_at='2026-01-01T00:00:00Z'")=='1'
    assert sql(db,"SELECT count(*) FROM fault_notification_deliveries")=='0'
    assert sql(db,"SELECT count(*) FROM fault_notification_watches")=='0'
    sql(db,"UPDATE faults SET title='updated' WHERE correlation_key='migration-sentinel'")
    assert sql(db,"SELECT activation_version FROM faults WHERE correlation_key='migration-sentinel'")=='1'
    sql(db,"UPDATE faults SET status='monitoring' WHERE correlation_key='migration-sentinel'")
    sql(db,"UPDATE faults SET status='active' WHERE correlation_key='migration-sentinel'")
    assert sql(db,"SELECT count(*) FROM faults WHERE correlation_key='migration-sentinel' AND activation_version=2 AND owner_notified_at IS NULL")=='1'
   migrate(db,'down','1')
   assert sql(db,'SELECT version::text || \'/\' || dirty::text FROM schema_migrations')=='31/false'
   assert sql(db,"SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('fault_notification_watches','fault_notification_deliveries')")=='0'
   assert sql(db,"SELECT count(*) FROM information_schema.columns WHERE table_name='faults' AND column_name='activation_version'")=='0'
   assert sql(db,"SELECT count(*) FROM pg_trigger WHERE tgname='faults_activation_version'")=='0'
   migrate(db,'up')
   assert sql(db,'SELECT version::text || \'/\' || dirty::text FROM schema_migrations')=='32/false'
   if start is not None:
    assert sql(db,"SELECT count(*) FROM teams WHERE name='migration-preserve-sentinel'")=='1'
    assert sql(db,"SELECT count(*) FROM faults WHERE correlation_key='migration-sentinel' AND status='active' AND title='updated' AND activation_version=1")=='1'
   row={'from':start or 'empty','to':32,'upgrade':'passed','down_up':'passed','existing_data':'preserved' if start else 'n/a'}
   results.append(row);print(json.dumps(row),flush=True)
 finally:
  subprocess.run(['docker','rm','-f',name],capture_output=True)
options.output.write_text(json.dumps(results,indent=2)+'\n')
