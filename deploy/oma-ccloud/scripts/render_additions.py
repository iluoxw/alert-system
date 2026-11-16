#!/usr/bin/env python3
"""Render an operator-reviewed deployment update, never apply or print credentials."""
import argparse,copy,json,re
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--deployment',required=True);p.add_argument('--output',required=True);p.add_argument('--agent-version',type=int,required=True);p.add_argument('--runtime-file-id',required=True);p.add_argument('--guard-file-id',required=True);p.add_argument('--installer-file-id',required=True);p.add_argument('--validator-file-id',required=True);p.add_argument('--proxy-vault-id');a=p.parse_args()
old=json.loads(Path(a.deployment).read_text());old=old.get('data',old)
if old.get('id')!='depl_laadNaS7VtUk6B3pGSWLltxT':raise SystemExit('unexpected deployment')
if old.get('environment_id')!='env_pwlE8ULfRTexjMcqEAL1zI3Z':raise SystemExit('unexpected environment: review manually')
body={k:copy.deepcopy(old[k]) for k in ['name','description','agent','environment_id','schedule','vault_ids','resources','initial_events','metadata','budget'] if k in old}
if not isinstance(body.get('agent'),dict) or body['agent'].get('id')!='agent_cmJ2nCWWiB8yNKuSgThWlkYc':raise SystemExit('unexpected agent')
if a.agent_version<2:raise SystemExit('new version must be pinned')
body['agent']['version']=a.agent_version
existing=copy.deepcopy(body['resources']);required={'memstore_jY01BppNLmo9X7ADMc318fFf','memstore_gDk1aBFVpBpialS6WBlavUBC','file_F2RcaRBCaXP0XNJWquxSmQY3'}
if not required.issubset({x.get('memory_store_id') or x.get('file_id') for x in existing}):raise SystemExit('original resources missing')
for id,path in [(a.runtime_file_id,'/uploads/ccloud-cli-2.2.1-runtime.tgz'),(a.guard_file_id,'/uploads/ccloud-cli-guard'),(a.installer_file_id,'/uploads/install_ccloud_runtime.py'),(a.validator_file_id,'/uploads/l4_validate.py')]:
 if not re.fullmatch(r'file_[A-Za-z0-9]+',id):raise SystemExit('invalid file ID')
 if any(x.get('mount_path')==path for x in existing):raise SystemExit('mount already exists; inspect before retry')
 body['resources'].append({'type':'file','file_id':id,'mount_path':path})
if a.proxy_vault_id:
 if not re.fullmatch(r'vlt_[A-Za-z0-9]+',a.proxy_vault_id):raise SystemExit('invalid vault ID')
 if a.proxy_vault_id not in body['vault_ids']:body['vault_ids'].append(a.proxy_vault_id)
# API response-only run timestamps are not deployment update inputs.
if body.get('schedule',{}).get('type')!='cron':raise SystemExit('unexpected schedule')
body['schedule']={k:copy.deepcopy(old['schedule'][k]) for k in ['type','expression','timezone']}
Path(a.output).write_text(json.dumps(body,ensure_ascii=False,indent=2)+'\n');Path(a.output).chmod(0o600)
assert body['resources'][:len(existing)]==existing and all(body['schedule'][k]==old['schedule'][k] for k in ['type','expression','timezone']) and all(x in body['vault_ids'] for x in old['vault_ids'])
print(json.dumps({'rendered':True,'original_resources_preserved':True,'original_vaults_preserved':True,'cron_preserved':True,'new_file_count':4,'agent_version':a.agent_version,'applied':False}))
