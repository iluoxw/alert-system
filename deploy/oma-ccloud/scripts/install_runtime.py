#!/usr/bin/env python3
"""Verify a mounted bundle and install to writable session tmp. No network/auth."""
import argparse,hashlib,json,os,shutil,subprocess,tarfile,tempfile
from pathlib import Path,PurePosixPath
p=argparse.ArgumentParser();p.add_argument('--archive',required=True);p.add_argument('--sha256',required=True);p.add_argument('--guard',required=True);p.add_argument('--destination',default='/tmp/oma-ccloud');a=p.parse_args()
archive=Path(a.archive)
if hashlib.sha256(archive.read_bytes()).hexdigest()!=a.sha256:raise SystemExit('archive sha256 mismatch')
version=subprocess.run(['node','--version'],capture_output=True,text=True,check=True).stdout.strip()
parts=version.lstrip('v').split('.')
if tuple(map(int,parts[:2]))<(18,15):raise SystemExit('Node >=18.15 required')
dest=Path(a.destination)
if str(dest)!='/tmp/oma-ccloud':raise SystemExit('fixed destination required')
if dest.is_symlink():raise SystemExit('symlink destination denied')
dest.mkdir(mode=0o700,parents=True,exist_ok=True)
with tarfile.open(archive,'r:gz') as tar:
 members=tar.getmembers()
 for m in members:
  rel=PurePosixPath(m.name)
  if rel.is_absolute() or '..' in rel.parts or not m.isfile() or rel.parts[0]!='ccloud-cli':raise SystemExit('unsafe archive member')
 for m in members:
  target=dest/m.name
  if any(x.is_symlink() for x in [target,*target.parents] if x!=Path('/tmp')):raise SystemExit('symlink destination denied')
  target.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
  if target.is_symlink():raise SystemExit('symlink destination denied')
  raw=tar.extractfile(m).read();target.write_bytes(raw);target.chmod(m.mode&0o777)
manifest=json.loads((dest/'ccloud-cli/BUNDLE.json').read_text())
for f in manifest['files']:
 target=dest/'ccloud-cli'/f['path']
 if hashlib.sha256(target.read_bytes()).hexdigest()!=f['sha256']:raise SystemExit('file hash mismatch')
bin_dir=dest/'bin'
if bin_dir.is_symlink():raise SystemExit('symlink destination denied')
bin_dir.mkdir(mode=0o700,exist_ok=True)
if (bin_dir/'ccloud-cli').is_symlink():raise SystemExit('symlink destination denied')
with tempfile.NamedTemporaryFile(dir=bin_dir,delete=False) as f:
 f.write(Path(a.guard).read_bytes());tmp_guard=Path(f.name)
tmp_guard.chmod(0o500);os.replace(tmp_guard,bin_dir/'ccloud-cli')
print(json.dumps({'installed':True,'version':manifest['version'],'node':version,'path_prefix':str(bin_dir),'file_hashes_verified':True}))
