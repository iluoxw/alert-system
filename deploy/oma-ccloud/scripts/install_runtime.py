#!/usr/bin/env python3
"""Offline bundle installation and shared verification; never starts the official CLI."""
import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import re
import shutil
import subprocess
import tarfile
import tempfile

DESTINATION = Path('/tmp/oma-ccloud')
ARCHIVE = '/mnt/session/uploads/ccloud-cli-2.2.1-runtime.tgz'
ARCHIVE_SHA256 = 'df4b64b268017e3e5a94fa20741e94ca0d3d26fc591abe5e841f2437c3fb1e87'
GUARD = '/mnt/session/uploads/ccloud-cli-guard'
GUARD_SHA256 = 'a2a908689bae62788a33c7fd723d166b7bffa5d91f19a4894b7fa48da717afdb'
PACKAGE = '@cvte/ccloud-cli'
VERSION = '2.2.1'


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def regular(path):
    # Reject symlink parents too; verification must not follow files outside the installation.
    if any(p.is_symlink() for p in (path, *path.parents)) or not path.is_file():
        raise ValueError('regular file without symlink required')
    return path.read_bytes()


def read_bundle(archive, expected_sha256):
    if not re.fullmatch('[0-9a-f]{64}', expected_sha256):
        raise ValueError('invalid archive sha256')
    # /tmp may be a platform symlink for the mounted source; the digest authenticates the archive.
    if digest(Path(archive).read_bytes()) != expected_sha256:
        raise ValueError('archive sha256 mismatch')
    files = {}
    with tarfile.open(archive, 'r:gz') as tar:
        for member in tar.getmembers():
            path = PurePosixPath(member.name)
            if (not member.isfile() or path.is_absolute() or '..' in path.parts
                    or len(path.parts) < 2 or path.parts[0] != 'ccloud-cli'
                    or member.name != str(path) or member.name in files):
                raise ValueError('unsafe or duplicate archive member')
            files[member.name] = (tar.extractfile(member).read(), member.mode & 0o777)
    manifest = json.loads(files['ccloud-cli/BUNDLE.json'][0])
    package = json.loads(files['ccloud-cli/package.json'][0])
    if (manifest.get('package') != PACKAGE or package.get('name') != PACKAGE
            or manifest.get('version') != VERSION or package.get('version') != VERSION):
        raise ValueError('package identity/version mismatch')
    listed = set()
    for item in manifest['files']:
        rel = PurePosixPath(item['path'])
        name = 'ccloud-cli/' + str(rel)
        if (rel.is_absolute() or '..' in rel.parts or str(rel) != item['path']
                or name in listed or name not in files
                or digest(files[name][0]) != item['sha256']):
            raise ValueError('manifest file hash/path mismatch')
        listed.add(name)
    if listed != set(files) - {'ccloud-cli/BUNDLE.json'} or 'ccloud-cli/dist/ccloud-cli.js' not in listed:
        raise ValueError('incomplete manifest')
    return files


def node_version():
    node = shutil.which('node')
    if node is None:
        raise ValueError('Node >=18.15 required')
    # Do not inherit NODE_OPTIONS, credentials, HOME, or the caller's environment.
    result = subprocess.run([node, '--version'], stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, env={'PATH': os.defpath}, timeout=5)
    version = result.stdout.strip()
    match = re.fullmatch(r'v(\d+)\.(\d+)\.(\d+)', version)
    if result.returncode or not match or tuple(map(int, match.groups()[:2])) < (18, 15):
        raise ValueError('Node >=18.15 required')
    return version


def trusted_guard(guard):
    raw = Path(guard).read_bytes()
    if digest(raw) != GUARD_SHA256:
        raise ValueError('guard sha256 mismatch')
    return raw


def verify_files(files, guard_bytes, destination):
    destination = Path(destination)
    # /tmp -> /private/tmp on macOS is a system alias, not an installation-controlled link.
    destination = destination.parent.resolve() / destination.name
    root = destination / 'ccloud-cli'
    if destination.is_symlink() or root.is_symlink() or not root.is_dir():
        raise ValueError('unsafe installation directory')
    actual = set()
    for path in root.rglob('*'):
        if path.is_symlink() or not (path.is_dir() or path.is_file()):
            raise ValueError('unsafe installed file')
        if path.is_file():
            actual.add(str(path.relative_to(destination)))
    if actual != set(files):
        raise ValueError('installed file inventory mismatch')
    for name, (raw, _) in files.items():
        if regular(destination / name) != raw:
            raise ValueError('installed file hash mismatch')
    guard = destination / 'bin/ccloud-cli'
    if regular(guard) != guard_bytes or not os.access(guard, os.X_OK):
        raise ValueError('installed guard mismatch or not executable')
    # Empty argv must be denied before token lookup or the official Node entrypoint.
    denied = subprocess.run([str(guard)], stdin=subprocess.DEVNULL, capture_output=True,
                            env={'PATH': os.defpath}, timeout=5)
    if denied.returncode != 64:
        raise ValueError('guard rejection self-check failed')


def verify_runtime(archive, expected_sha256, guard, destination=DESTINATION):
    files = read_bundle(archive, expected_sha256)
    guard_bytes = trusted_guard(guard)
    verify_files(files, guard_bytes, destination)
    version = node_version()
    return {'installed': True, 'package': PACKAGE, 'version': VERSION, 'node': version,
            'path_prefix': str(Path(destination) / 'bin'), 'file_hashes_verified': True,
            'guard_verified': True, 'official_cli_executed': False, 'network_used': False}


def install_runtime(archive, expected_sha256, guard, destination=DESTINATION):
    files = read_bundle(archive, expected_sha256)
    guard_bytes = trusted_guard(guard)
    version = node_version()
    dest = Path(destination)
    # Resolve only the system parent alias; never an installation-controlled path.
    dest = dest.parent.resolve() / dest.name
    if dest.is_symlink():
        raise ValueError('symlink destination denied')
    dest.mkdir(mode=0o700, parents=True, exist_ok=True)
    for name, (raw, mode) in files.items():
        target = dest / name
        if any(p.is_symlink() for p in (target, *target.parents)):
            raise ValueError('symlink destination denied')
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        target.write_bytes(raw)
        target.chmod(mode & 0o755)
    bin_dir = dest / 'bin'
    if bin_dir.is_symlink() or (bin_dir / 'ccloud-cli').is_symlink():
        raise ValueError('symlink destination denied')
    bin_dir.mkdir(mode=0o700, exist_ok=True)
    with tempfile.NamedTemporaryFile(dir=bin_dir, delete=False) as f:
        f.write(guard_bytes)
        tmp_guard = Path(f.name)
    tmp_guard.chmod(0o500)
    os.replace(tmp_guard, bin_dir / 'ccloud-cli')
    verify_files(files, guard_bytes, dest)
    return {'installed': True, 'version': VERSION, 'node': version, 'path_prefix': str(bin_dir),
            'file_hashes_verified': True, 'guard_verified': True,
            'official_cli_executed': False, 'network_used': False}


def arguments(description, verification=False):
    parser = argparse.ArgumentParser(description=description)
    parser.add_argument('--archive', default=ARCHIVE, required=not verification)
    parser.add_argument('--sha256', default=ARCHIVE_SHA256, required=not verification)
    parser.add_argument('--guard', default=GUARD, required=not verification)
    parser.add_argument('--destination', default=str(DESTINATION))
    args = parser.parse_args()
    if args.destination != str(DESTINATION):
        parser.error('fixed destination required')
    return args


if __name__ == '__main__':
    args = arguments(__doc__)
    try:
        print(json.dumps(install_runtime(args.archive, args.sha256, args.guard)))
    except (ValueError, OSError, KeyError, TypeError, tarfile.TarError, subprocess.SubprocessError):
        raise SystemExit('Offline installation failed; integrity/runtime check rejected (details suppressed)') from None
