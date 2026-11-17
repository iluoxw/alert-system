#!/usr/bin/env python3
"""Verify installed offline runtime. No official CLI entrypoint, credentials, or network."""
import json
import subprocess
import sys
import tarfile

sys.dont_write_bytecode = True

try:
    from install_runtime import arguments, verify_runtime
except ModuleNotFoundError as error:
    if error.name != 'install_runtime':
        raise
    # The existing OMA resource deliberately mounts the same installer under this name.
    from install_ccloud_runtime import arguments, verify_runtime

if __name__ == '__main__':
    args = arguments(__doc__, verification=True)
    try:
        print(json.dumps(verify_runtime(args.archive, args.sha256, args.guard)))
    except (ValueError, OSError, KeyError, TypeError, tarfile.TarError, subprocess.SubprocessError):
        raise SystemExit('Offline verification failed; integrity/runtime check rejected (details suppressed)') from None
