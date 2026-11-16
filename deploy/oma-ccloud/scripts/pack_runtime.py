#!/usr/bin/env python3
"""Offline, allowlisted package bundle. Never includes HOME/auth/config files."""
import argparse,hashlib,io,json,os,re,tarfile
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--package-dir',required=True);p.add_argument('--output',required=True);a=p.parse_args()
root=Path(a.package_dir).resolve();pkg=json.loads((root/'package.json').read_text())
if pkg.get('name')!='@cvte/ccloud-cli' or pkg.get('version')!='2.2.1':raise SystemExit('expected @cvte/ccloud-cli 2.2.1')
selected=[]
for f in sorted(root.rglob('*')):
 rel=f.relative_to(root)
 if f.is_symlink() or not f.is_file() or any(part.startswith('.') for part in rel.parts):continue
 if rel.parts[0] not in {'dist','node_modules','package.json'}:continue
 if f.suffix=='.node':raise SystemExit('native addon present: require architecture-specific audit')
 if f.name.lower() in {'credentials.json','.env','config.json'}:raise SystemExit('unexpected configuration file in runtime')
 data=f.read_bytes()
 # Report only failure, never a candidate value or line.
 if re.search(rb'(sk-ant-[A-Za-z0-9_-]{16,}|-----BEGIN (RSA |EC )?PRIVATE KEY)',data):raise SystemExit('credential literal candidate detected')
 selected.append((f,rel,data))
manifest={'package':pkg['name'],'version':pkg['version'],'node_requirement':pkg.get('engines',{}).get('node'),'credential_files_included':False,'native_addons':False,'files':[{'path':str(rel),'sha256':hashlib.sha256(data).hexdigest()} for _,rel,data in selected]}
out=Path(a.output);out.parent.mkdir(parents=True,exist_ok=True)
with tarfile.open(out,'w:gz') as tar:
 for f,rel,data in selected:
  info=tarfile.TarInfo('ccloud-cli/'+str(rel));info.size=len(data);info.mode=0o755 if os.access(f,os.X_OK) else 0o644;info.mtime=0;tar.addfile(info,io.BytesIO(data))
 data=(json.dumps(manifest,sort_keys=True,indent=2)+'\n').encode();info=tarfile.TarInfo('ccloud-cli/BUNDLE.json');info.size=len(data);info.mode=0o644;tar.addfile(info,io.BytesIO(data))
os.chmod(out,0o600)
print(json.dumps({'package':pkg['name'],'version':pkg['version'],'archive_sha256':hashlib.sha256(out.read_bytes()).hexdigest(),'archive_bytes':out.stat().st_size,'file_count':len(selected),'credential_files_included':False,'native_addons':False}))
