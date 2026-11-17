#!/usr/bin/env python3
"""Offline checks: renderer preserves state and fixed guard never broadens scope."""
import json,os,stat,subprocess,sys,tempfile,unittest
from pathlib import Path
ROOT=Path(__file__).resolve().parents[1]
class PackageTests(unittest.TestCase):
 def test_render_preserves_resources_vault_cron_and_budget(self):
  with tempfile.TemporaryDirectory() as d:
   old={'id':'depl_laadNaS7VtUk6B3pGSWLltxT','environment_id':'env_pwlE8ULfRTexjMcqEAL1zI3Z','agent':{'type':'agent','id':'agent_cmJ2nCWWiB8yNKuSgThWlkYc','version':1},'schedule':{'type':'cron','expression':'30 8 * * 1-5','timezone':'Asia/Shanghai','last_run_at':'past','upcoming_runs_at':['future']},'vault_ids':['vlt_original'],'resources':[{'type':'memory_store','memory_store_id':i,'access':a} for i,a in [('memstore_jY01BppNLmo9X7ADMc318fFf','read_only'),('memstore_gDk1aBFVpBpialS6WBlavUBC','read_write')]]+[{'type':'file','file_id':'file_F2RcaRBCaXP0XNJWquxSmQY3','mount_path':'/uploads/iacctl-go'},{'type':'file','file_id':'file_extra','mount_path':'/uploads/extra'}],'budget':{'max_tokens':123},'initial_events':[{'type':'user.message','content':[]}],'metadata':{'retain':'yes'}}
   src=Path(d)/'old.json';out=Path(d)/'out.json';src.write_text(json.dumps(old))
   args=[sys.executable,str(ROOT/'scripts/render_additions.py'),'--deployment',str(src),'--output',str(out),'--agent-version','2','--runtime-file-id','file_runtime','--guard-file-id','file_guard','--installer-file-id','file_installer','--validator-file-id','file_validator','--proxy-vault-id','vlt_proxy']
   subprocess.run(args,check=True,capture_output=True);new=json.loads(out.read_text())
   self.assertEqual(new['resources'][:4],old['resources']);self.assertEqual(new['vault_ids'],['vlt_original','vlt_proxy']);self.assertEqual(new['budget'],old['budget']);self.assertEqual(new['initial_events'],old['initial_events']);self.assertEqual(new['metadata'],old['metadata']);self.assertEqual(new['schedule'],{k:old['schedule'][k] for k in ['type','expression','timezone']});self.assertEqual(stat.S_IMODE(out.stat().st_mode),0o600)
 def test_guard_rejects_other_scope_and_missing_token_without_network(self):
  env={'PATH':os.environ['PATH']}
  args=['sh',str(ROOT/'ccloud-cli'),'app','pods','--app-id','other','--cluster-id','60']
  self.assertEqual(subprocess.run(args,env=env,capture_output=True).returncode,64)
  args[5]='80beff42ab0b41819f9c1c3c3ac95413308e70eb'
  self.assertEqual(subprocess.run(args,env=env,capture_output=True).returncode,2)
 def test_installer_rejects_tampered_bundle(self):
  with tempfile.NamedTemporaryFile() as f:
   f.write(b'not-an-archive');f.flush()
   result=subprocess.run([sys.executable,str(ROOT/'scripts/install_runtime.py'),'--archive',f.name,'--sha256','0'*64,'--guard',str(ROOT/'ccloud-cli')],capture_output=True,text=True)
   self.assertNotEqual(result.returncode,0);self.assertIn('Offline installation failed',result.stderr)
if __name__=='__main__':unittest.main()
