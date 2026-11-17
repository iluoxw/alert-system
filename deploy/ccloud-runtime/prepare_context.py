#!/usr/bin/env python3
"""制作仅含已审计归档的 BuildKit named context，不读取配置或凭据。"""
import argparse,hashlib,json,shutil
from pathlib import Path
SHA='df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87'
def prepare(archive,output):
    if hashlib.sha256(archive.read_bytes()).hexdigest()!=SHA:
        raise ValueError('archive SHA mismatch')
    if output.exists():
        raise ValueError('output must be a new directory')
    output.mkdir(mode=0o700,parents=True)
    target=output/'runtime.tgz'
    shutil.copyfile(archive,target);target.chmod(0o600)
    if hashlib.sha256(target.read_bytes()).hexdigest()!=SHA:
        target.unlink();raise ValueError('copied archive SHA mismatch')
    return {'prepared':True,'archive_sha256':SHA,'context_files':['runtime.tgz']}
def check(output):
    if not output.is_dir() or output.is_symlink() or {p.name for p in output.iterdir()}!={'runtime.tgz'}:
        raise ValueError('context must contain only runtime.tgz')
    archive=output/'runtime.tgz'
    if archive.is_symlink() or not archive.is_file() or hashlib.sha256(archive.read_bytes()).hexdigest()!=SHA:
        raise ValueError('context archive SHA mismatch')
    return {'verified':True,'archive_sha256':SHA}
if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--archive',type=Path);p.add_argument('--output',type=Path,required=True);p.add_argument('--check',action='store_true');a=p.parse_args()
    try:
        if not a.check and a.archive is None:raise ValueError('archive required')
        print(json.dumps(check(a.output) if a.check else prepare(a.archive,a.output)))
    except (OSError,ValueError):raise SystemExit('Offline context unavailable or invalid; run make ccloud-runtime-prepare CCLOUD_RUNTIME_ARCHIVE=/path/to/audited.tgz (use a new context directory)') from None
