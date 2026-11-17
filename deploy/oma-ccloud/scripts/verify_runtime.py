#!/usr/bin/env python3
"""Verify installed offline runtime. No official CLI entrypoint, credentials, or network."""
import json
import subprocess
import tarfile

from install_runtime import arguments, verify_runtime

if __name__ == '__main__':
    args = arguments(__doc__, verification=True)
    try:
        print(json.dumps(verify_runtime(args.archive, args.sha256, args.guard)))
    except (ValueError, OSError, KeyError, TypeError, tarfile.TarError, subprocess.SubprocessError):
        raise SystemExit('Offline verification failed; integrity/runtime check rejected (details suppressed)') from None
