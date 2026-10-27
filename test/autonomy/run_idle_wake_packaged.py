#!/usr/bin/env python3
"""Targeted idle-manager wake run on a packaged candidate's bundled daemon + ACP runtime (headless, scratch only).
Same harness and confinement as the plain CLI; adds AO_ACP_RUNTIME_DIR from the package (the CLI cannot).
usage: run_idle_wake_packaged.py --app <candidate .app> --output DIR [--no-fixture-end-turn-rule]
"""
import argparse, signal, sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import acceptance as a  # noqa: E402

p = argparse.ArgumentParser()
p.add_argument('--app', type=Path, required=True)
p.add_argument('--output', type=Path, required=True)
p.add_argument('--no-fixture-end-turn-rule', action='store_true')
args = p.parse_args()
args.daemon = (args.app / 'Contents/Resources/daemon/ao').resolve()
args.scenario = 'idle-wake'; args.mode = 'real'; args.agent = 'codex'
args.agent_bin_dir = ['/opt/homebrew/bin']; args.pass_env = []; args.auth_readonly = ['/Users/edstockman/.codex/auth.json=.codex/auth.json']
args.seed_db = None; args.answers = {}; args.duration = 900; args.restart_after = 5; args.max_dispatch_latency = 30; args.stall_seconds = 120; args.poll = 0.5
args.scratch_parent = Path('/private/tmp')
h = None
try:
    h = a.Harness(args)
    h.env['AO_ACP_RUNTIME_DIR'] = str((args.app / 'Contents/Resources/acp-runtime').resolve())
    signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
    h.run()
except (Exception, KeyboardInterrupt) as e:
    if h is None:
        print(type(e).__name__ + ': init failed', file=sys.stderr); sys.exit(2)
    h.result['errors'].append(type(e).__name__ + ': ' + str(e)[:200])
finally:
    if h:
        h.finalize()
sys.exit(0 if h.result.get('targetedIdleWakePassed', h.result['passed']) else 1)
