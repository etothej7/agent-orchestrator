#!/usr/bin/env python3
"""Local-only ED7 auto-resume acceptance. No third-party Python dependencies.

Builds real session state in a confined scratch AO (coordinator, repo managers,
workers, one session protected with autoResume=never, one stopped by the user,
one terminated), simulates a macOS restart (hosts first, daemon first, kill -9,
a flapping daemon, a resume failure) and asserts what the daemon does BY ITSELF
afterwards. The harness never resumes, nudges or re-sends anything after the
restart; the only post-restart write is the wake probe to a hibernated session.

Reuses acceptance.Harness for confinement, process tracking and credential
purge. Exit 0: every gate passed in a real run; 1: a gate failed or was not
observed; 2: harness/setup error. A mechanics run (fixture ACP provider) never
certifies the feature.
"""
import argparse
import collections
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import time
import urllib.parse

sys.path.insert(0, str(Path(__file__).resolve().parent))
import acceptance as A  # noqa: E402

CONTINUE_PREFIX = 'ao-autoresume-continue:'
CONTINUE_TEXT = re.compile(r'^\s*AO restarted\b', re.I)
NOTICE_TEXT = re.compile(r"couldn.?t resume|could not resume|can.?t resume", re.I)
WAKE_PROBE_TEXT = 'AR wake probe: reply with the single word AWAKE.'
SLEEP_COMMAND = 'sleep 600'

# Stage barrier of the design: coordinator, then managers, then workers.
STAGES = (('coord',), ('mgr_busy',), ('wkr', 'codex_wkr'))
CLASSES = {
    'coord': 'working-idle',  # always actively resumed, even though idle
    'mgr_idle': 'idle', 'mgr_idle2': 'idle', 'mgr_busy': 'working', 'wkr': 'working', 'codex_wkr': 'working',
    'never_orch': 'never', 'stopped': 'user_stopped', 'terminated': 'terminated',
}
FULL_ROLES = ('coord', 'mgr_idle', 'mgr_idle2', 'mgr_busy', 'wkr', 'codex_wkr', 'never_orch', 'stopped', 'terminated')
SMALL_ROLES = ('coord', 'wkr', 'wkr_fail')
FLAP_ROLES = ('coord', 'wkr')
CYCLES = {
    'hosts-first': {'order': 'hosts-first', 'roles': FULL_ROLES},
    'daemon-first': {'order': 'daemon-first', 'roles': FULL_ROLES},
    'kill9': {'order': 'kill9', 'roles': FULL_ROLES},
    # Two extra quick daemon kills make four daemon starts in minutes: past the design's boot-storm guard (more than 3), which delays the resumer by 2 minutes.
    'flap': {'order': 'kill9', 'roles': FLAP_ROLES, 'flap': 2, 'giveup': 240},
    'resume-failure': {'order': 'kill9', 'roles': SMALL_ROLES, 'fail': 'wkr_fail'},
}

FAKE_ACP = r'''#!/usr/bin/env python3
import json, os, sys, time, uuid
LOG = os.environ.get("AO_SCRATCH_ACP_LOG", "")
ROOT = os.environ.get("AO_SCRATCH_ROOT", "")
def rec(**kw):
    if LOG:
        kw.update(t=time.time(), pid=os.getpid())
        with open(LOG, "a") as f: f.write(json.dumps(kw) + "\n")
rec(method="start")
for line in sys.stdin:
    try: r = json.loads(line)
    except Exception: continue
    m, i, p = r.get("method"), r.get("id"), r.get("params") or {}
    if i is None: continue
    out = lambda res: print(json.dumps({"jsonrpc": "2.0", "id": i, "result": res}), flush=True)
    sid = p.get("sessionId") or ""
    text = ""
    for part in p.get("prompt") or []:
        text += part.get("text", "") if isinstance(part, dict) else ""
    rec(method=m, cwd=p.get("cwd", ""), sid=sid, text=text[:400])
    if m == "initialize":
        out({"protocolVersion": 1, "agentCapabilities": {"loadSession": True, "sessionCapabilities": {"resume": {}}}, "authMethods": [], "agentInfo": {"name": "OpenCode", "version": "2.0.0"}})
    elif m in ("session/new", "session/load", "session/resume"):
        if m != "session/new": time.sleep(float(os.environ.get("AO_SCRATCH_LOAD_DELAY", "0")))
        if m != "session/new" and os.path.exists(os.path.join(ROOT, "fail-load", sid)):
            print(json.dumps({"jsonrpc": "2.0", "id": i, "error": {"code": -32002, "message": "session not found"}}), flush=True)
            continue
        sid = sid if m != "session/new" else "ar-" + uuid.uuid4().hex[:12]
        out({"sessionId": sid, "configOptions": [
            {"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "provider/default", "options": [{"value": "provider/default", "name": "Default"}]},
            {"id": "effort", "name": "Effort", "category": "thought_level", "type": "select", "currentValue": "default", "options": [{"value": "default", "name": "Default"}]},
            {"id": "mode", "name": "Session Mode", "category": "mode", "type": "select", "currentValue": "build", "options": [{"value": "build", "name": "Build"}, {"value": "plan", "name": "Plan"}]}]})
    elif m == "session/prompt":
        if "sleep 600" in text:  # models a long command: the turn stays running
            deadline = time.time() + 900
            while time.time() < deadline: time.sleep(0.5)
        print(json.dumps({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": sid, "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "ok"}}}}), flush=True)
        out({"stopReason": "end_turn"})
    else:
        out({})
'''

SHIM = '#!/bin/sh\nif [ "$1" = "--version" ]; then echo 2.0.0; exit 0; fi\nexec "$AO_SCRATCH_PYTHON" "$AO_SCRATCH_FAKE_ACP" "$@"\n'


def tree_fingerprint(path):
    """Content, mode, symlink targets, names, inode and birth time of the worktree root."""
    path = Path(path)
    if not path.is_dir():
        return {'exists': False}
    h, count = hashlib.sha256(), 0
    for base, dirs, files in os.walk(path, topdown=True):
        dirs.sort()
        for name in sorted(files) + sorted(dirs):
            full = os.path.join(base, name)
            rel = os.path.relpath(full, path)
            info = os.lstat(full)
            count += 1
            h.update(rel.encode() + b'\0' + oct(stat.S_IMODE(info.st_mode)).encode() + b'\0')
            if stat.S_ISLNK(info.st_mode):
                h.update(os.readlink(full).encode())
            elif stat.S_ISREG(info.st_mode):
                h.update(Path(full).read_bytes())
            h.update(b'\1')
    root = path.stat()
    return {'exists': True, 'digest': h.hexdigest(), 'entries': count, 'rootInode': root.st_ino,
            'rootBirth': getattr(root, 'st_birthtime', None)}


# ---------------------------------------------------------------------------------------------------------------------
# Pure gate functions. Inputs are plain dicts so they can be unit-tested with synthetic evidence.
#   snap:      {'sessions': {sid: {activity, hibernated, terminated, convId, workspace, stopReason, policy, fingerprint}},
#               'turns': {sid: [{id, state, providerTurnId}]}, 'messages': {sid: [{id, origin, text, key, createdAt}]}}
#   timeline:  [{'t': float, 'sessions': {sid: {'activity', 'hibernated', 'terminated'}}, 'hosts': {sid: [pid, ...]}}]
#   shim:      [{'t', 'pid', 'method', 'cwd', 'sid', 'text'}]   (mechanics runs only; [] otherwise)
# ---------------------------------------------------------------------------------------------------------------------

def gate(passed, **detail):
    return {'passed': bool(passed), **detail}


def provider_sid(conversation):
    """AO stores 'ao-harness:<harness>:<provider id>'; the provider only sees the last segment."""
    return (conversation or '').rsplit(':', 1)[-1]


def mentions(text, names):
    """True if any name (a display name such as 'wkr' or a session id) appears as a whole token.
    'wkr' must not match 'wkr_fail', nor 'ar-work-1' 'ar-work-10'."""
    return any(re.search(r'(?<![\w-])' + re.escape(name) + r'(?![\w-])', text) for name in names if name)


def first_live(timeline, sid):
    """First sample at which the session had a live host and was not exited."""
    for sample in timeline:
        view = sample['sessions'].get(sid, {})
        if sample['hosts'].get(sid) and view.get('activity') not in (None, 'exited') and not view.get('terminated'):
            return sample['t']
    return None


def ever_live(timeline, sid):
    return first_live(timeline, sid) is not None or any(sample['hosts'].get(sid) for sample in timeline)


def stage_order_gate(timeline, stages, slack=0.75):
    """Each stage is first observed live no earlier than every session of the previous stage (barrier)."""
    firsts = {sid: first_live(timeline, sid) for stage in stages for sid in stage}
    missing = sorted(sid for sid, t in firsts.items() if t is None)
    violations = []
    for earlier, later in zip(stages, stages[1:]):
        done = [firsts[s] for s in earlier if firsts[s] is not None]
        starts = [(s, firsts[s]) for s in later if firsts[s] is not None]
        if done and starts:
            barrier = max(done)
            violations += [{'session': s, 'liveAt': t, 'previousStageLiveAt': barrier} for s, t in starts if t + slack < barrier]
    return gate(not missing and not violations, firstLive=firsts, missing=missing, violations=violations)


def active_resume_gate(timeline, before, after, sessions):
    """Working sessions are live again, in the SAME provider conversation and worktree."""
    problems = {}
    for sid in sessions:
        old, new = before['sessions'].get(sid, {}), after['sessions'].get(sid, {})
        issues = []
        if first_live(timeline, sid) is None:
            issues.append('never_live')
        if new.get('activity') == 'exited' or new.get('terminated'):
            issues.append('not_live_at_end')
        if old.get('convId') and old.get('providerConversation') != new.get('providerConversation'):
            issues.append('provider_conversation_changed')
        if issues:
            problems[sid] = issues
    return gate(sessions and not problems, checked=sorted(sessions), problems=problems)


def hibernate_gate(timeline, after, sessions):
    """Idle sessions are parked (hibernated, not exited), with no provider launched for them."""
    problems = {}
    for sid in sessions:
        view = after['sessions'].get(sid, {})
        issues = []
        if not view.get('hibernated'):
            issues.append('not_hibernated')
        if view.get('activity') == 'exited':
            issues.append('exited')
        if ever_live(timeline, sid):
            issues.append('provider_launched')
        if issues:
            problems[sid] = issues
    return gate(sessions and not problems, checked=sorted(sessions), problems=problems)


def wake_gate(results):
    """results: {sid: {'status': http, 'turnStarted': bool, 'live': bool, 'sameConversation': bool}}"""
    problems = {sid: r for sid, r in results.items()
                if not (200 <= r.get('status', 0) < 300 and r.get('turnStarted') and r.get('live') and r.get('sameConversation'))}
    return gate(results and not problems, probes=results, problems=sorted(problems))


def stays_down_gate(timeline, before, after, sessions, require_policy=None, intents=None):
    """intents: {sid: state} read from the daemon. A daemon that captured nothing proves nothing about exclusions."""
    problems = {}
    for sid in sessions:
        issues = []
        if intents is not None and not intents.get('_observed'):
            issues.append('no_intents_recorded_so_nothing_to_judge')
        if intents is not None and sid in intents:
            issues.append('captured_for_resume')
        if ever_live(timeline, sid):
            issues.append('launched')
        view = after['sessions'].get(sid, {})
        if not (view.get('activity') == 'exited' or view.get('terminated')):
            issues.append('not_down_at_end')
        new_turns = len(after['turns'].get(sid, [])) - len(before['turns'].get(sid, []))
        if new_turns > 0:
            issues.append('new_turns')
        if require_policy and view.get('policy') != require_policy:
            issues.append('policy_not_' + require_policy)
        if issues:
            problems[sid] = issues
    return gate(sessions and not problems, checked=sorted(sessions), problems=problems)


WORKTREE_KEYS = ('exists', 'digest', 'entries', 'rootInode', 'rootBirth')


def worktree_gate(before, pre_wake, final, strict, resumed, timeline):
    """Byte-for-byte identity (content, modes, names, inode, birth) for sessions that run no agent: excluded
    ones and the parked manager, measured BEFORE the wake probe. A resumed agent legitimately writes, so for
    those the worktree must be the same directory (inode, birth), keep the dirty marker and stay on the same HEAD."""
    problems, compared = {}, {'strict': [], 'resumed': []}
    for sid in strict:
        old = (before['sessions'].get(sid, {}).get('fingerprint')) or {}
        if not old.get('exists'):
            continue
        compared['strict'].append(sid)
        new = (pre_wake['sessions'].get(sid, {}).get('fingerprint')) or {}
        changed = [k for k in WORKTREE_KEYS if old.get(k) != new.get(k)]
        if changed:
            problems[sid] = changed
    for sid in resumed:
        old = (before['sessions'].get(sid, {}).get('fingerprint')) or {}
        if not old.get('exists'):
            continue
        compared['resumed'].append(sid)
        new = (final['sessions'].get(sid, {}).get('fingerprint')) or {}
        changed = [k for k in ('exists', 'rootInode', 'rootBirth', 'marker', 'head') if old.get(k) != new.get(k)]
        if first_live(timeline, sid) is None:
            changed.append('not_resumed_so_nothing_to_judge')
        if changed:
            problems[sid] = changed
    return gate(compared['resumed'] and (compared['strict'] or not any(
        (before['sessions'].get(sid, {}).get('fingerprint') or {}).get('exists') for sid in strict)) and not problems,
        compared=compared, problems=problems)


def intent_barrier_gate(timeline, stages):
    """Durable-state view of the stage barrier: a later-stage session is never `resuming` while an earlier-stage
    one is still pending/resuming/missing. Needs the daemon's intent rows; no rows is no evidence."""
    settled = ('resumed', 'failed', 'abandoned', 'hibernated')
    seen, violations = set(), []
    for item in timeline:
        intents = item.get('intents', {})
        for index, stage in enumerate(stages):
            for sid in stage:
                if intents.get(sid):
                    seen.add(sid)
                if intents.get(sid) == 'resuming':
                    unsettled = [e for earlier in stages[:index] for e in earlier if intents.get(e) not in settled]
                    if unsettled:
                        violations.append({'t': item['t'], 'session': sid, 'earlierUnsettled': unsettled})
    expected = {sid for stage in stages for sid in stage}
    missing = sorted(expected - seen)
    return gate(not missing and not violations, intentsSeen=sorted(seen), missing=missing, violations=violations[:5])


def intent_expectation_gate(intents, resumed, parked, excluded, failed=()):
    """Final intent rows: resumed sessions `resumed`, parked ones `hibernated`, a failed one `failed`, excluded none."""
    problems = {}
    for sid in resumed:
        if intents.get(sid) != 'resumed':
            problems[sid] = f'expected resumed, got {intents.get(sid)}'
    for sid in parked:
        if intents.get(sid) != 'hibernated':
            problems[sid] = f'expected hibernated, got {intents.get(sid)}'
    for sid in failed:
        if intents.get(sid) != 'failed':
            problems[sid] = f'expected failed, got {intents.get(sid)}'
    for sid in excluded:
        if sid in intents:
            problems[sid] = f'excluded session has an intent ({intents[sid]})'
    return gate(intents.get('_observed') and (resumed or parked) and not problems, problems=problems)


def continue_messages(after, sid):
    keyed = [m for m in after['messages'].get(sid, []) if m['key'].startswith(CONTINUE_PREFIX)]
    texted = [m for m in after['messages'].get(sid, []) if CONTINUE_TEXT.match(m['text'])]
    return keyed, texted


def continue_turn_gate(before, after, expect, none):
    """Exactly one keyed, hidden-automation continue turn for each interrupted session; none elsewhere."""
    problems = {}
    for sid in expect:
        keyed, texted = continue_messages(after, sid)
        issues = []
        if len(keyed) != 1:
            issues.append(f'keyed={len(keyed)}')
        if len(texted) != 1:
            issues.append(f'text={len(texted)}')
        if keyed and keyed[0]['origin'] != 'automation':
            issues.append('origin_not_automation')
        if keyed and not keyed[0].get('turnId'):
            issues.append('no_turn')
        if issues:
            problems[sid] = issues
    for sid in none:
        keyed, texted = continue_messages(after, sid)
        if keyed or texted:
            problems[sid] = [f'unexpected_continue={len(keyed) + len(texted)}']
    return gate((expect or none) and not problems, expected=sorted(expect), none=sorted(none), problems=problems)


def no_fresh_relaunch_gate(before, after, shim, restart_at, sessions, tokens, timeline):
    """Same provider conversation, the original prompt exists once, and (fixture runs) no session/new or re-sent prompt."""
    problems = {}
    cwd_of = {sid: before['sessions'].get(sid, {}).get('workspace') for sid in sessions}
    for sid in sessions:
        issues = []
        old, new = before['sessions'].get(sid, {}), after['sessions'].get(sid, {})
        if first_live(timeline, sid) is None:
            issues.append('not_resumed_so_nothing_to_judge')  # never a vacuous pass
        if old.get('providerConversation') != new.get('providerConversation'):
            issues.append('conversation_changed')
        if old.get('prompt') != new.get('prompt'):
            issues.append('prompt_changed')
        token = tokens.get(sid)
        sent = [m for m in after['messages'].get(sid, []) if token and token in m['text'] and m['role'] == 'user']
        if token and len(sent) > 1:
            issues.append(f'original_prompt_messages={len(sent)}')
        for call in shim:
            relevant = call.get('cwd') == cwd_of[sid] or (call.get('sid') and call.get('sid') == provider_sid(old.get('providerConversation')))
            if call['t'] < restart_at or not relevant:
                continue
            if call['method'] == 'session/new':
                issues.append('fresh_session_new')
            if call['method'] == 'session/prompt' and token and token in call.get('text', ''):
                issues.append('original_prompt_resent')
        if issues:
            problems[sid] = sorted(set(issues))
    return gate(sessions and not problems, checked=sorted(sessions), problems=problems, fixtureChecks=bool(shim))


def duplicate_gate(timeline, after, sessions):
    """At most one host per session at any sample, and one continue turn."""
    problems = {}
    for sid in sessions:
        peak = max((len(sample['hosts'].get(sid, [])) for sample in timeline), default=0)
        keyed, _ = continue_messages(after, sid)
        issues = [] if peak else ['never_live_so_nothing_to_judge']
        if peak > 1:
            issues.append(f'simultaneous_hosts={peak}')
        if len(keyed) > 1:
            issues.append(f'continue_turns={len(keyed)}')
        if issues:
            problems[sid] = issues
    return gate(sessions and not problems, checked=sorted(sessions), problems=problems)


def failure_notice_gate(notices, failed, others, after, timeline):
    """One stopped-work coordinator notice for the failed session only; it stays down; the rest resume.
    failed/others are NAME LISTS ([display name, session id]): the product writes the display name."""
    mine = [n for n in notices if mentions(n['text'], failed['names'])]
    stray = [n for n in notices if any(mentions(n['text'], o) for o in others)]
    # A host may exist briefly while the failing resume attempt runs; what matters is that it ends exited with no host.
    down = (after['sessions'].get(failed['sid'], {}).get('activity') == 'exited') and not (timeline and timeline[-1]['hosts'].get(failed['sid']))
    return gate(len(mine) == 1 and not stray and down, noticesForFailed=len(mine), strayNotices=len(stray), failedStaysExited=down)


def codex_settle_gate(timeline, after, notices, sid, restart_at, bound, names=()):
    live = first_live(timeline, sid)
    noticed = any(mentions(n['text'], [sid, *names]) for n in notices)
    if live is not None and live - restart_at <= bound:
        outcome = 'resumed'
    elif noticed and live is None:
        outcome = 'clean_stopped_work_notice'
    else:
        outcome = 'neither' if live is None else 'resumed_late'
    return gate(outcome in ('resumed', 'clean_stopped_work_notice'), outcome=outcome, bound=bound,
                secondsToLive=None if live is None else round(live - restart_at, 2))


# ---------------------------------------------------------------------------------------------------------------------

class Cycle(A.Harness):
    """One confined scratch AO with its own root, port, daemon and teardown."""

    def __init__(self, args, name):
        super().__init__(args)
        self.cycle = name
        self.spec = CYCLES[name]
        self.sessions = {}
        self.tokens = {}
        self.timeline = []
        self.shim_log = self.root / 'acp-calls.jsonl'
        self.gates = {}
        self.detail = {'cycle': name, 'order': self.spec['order'], 'roles': list(self.spec['roles'])}
        self.wake = {}
        self.starts = 0
        self.install_shim()

    def emit(self):  # the aggregator writes the artifacts
        pass

    def start(self, seed=False):
        self.starts += 1
        super().start(seed)

    def install_shim(self):
        if self.args.mode != 'mechanics':
            return
        fake = self.root / 'fake_acp.py'
        fake.write_text(FAKE_ACP)
        shim = self.root / 'bin/opencode'
        shim.write_text(SHIM)
        shim.chmod(0o755)
        (self.root / 'fail-load').mkdir()
        self.env.update(AO_SCRATCH_FAKE_ACP=str(fake), AO_SCRATCH_ACP_LOG=str(self.shim_log),
                        AO_SCRATCH_ROOT=str(self.root), AO_SCRATCH_PYTHON=sys.executable,
                        AO_SCRATCH_LOAD_DELAY=str(self.args.load_delay))

    # -- helpers -------------------------------------------------------------------------------------------------
    @property
    def db(self):
        return self.data / 'ao.db'

    def harness_for(self, role):
        if self.args.mode == 'mechanics':
            return 'opencode-v2'
        return self.args.harness_map.get(role, self.args.default_harness)

    def shim_calls(self):
        if not self.shim_log.exists():
            return []
        out = []
        for line in self.shim_log.read_text().splitlines():
            try:
                out.append(json.loads(line))
            except ValueError:
                pass
        return out

    def read_snapshot(self, with_fingerprints=False):
        with A.db_open(self.db) as db:
            sessions = {r['id']: r for r in A.rows(db, 'sessions')}
            conversations = {r['current_session_id']: r['id'] for r in A.rows(db, 'conversations') if r.get('current_session_id')}
            turns, messages = A.rows(db, 'conversation_turns'), A.rows(db, 'conversation_messages')
            policy = {r['session_id']: r for r in A.rows(db, 'session_auto_resume')}
            runs = A.rows(db, 'daemon_runs')
            intents = A.rows(db, 'session_resume_intents')
        by_id = {}
        for role, sid in self.sessions.items():
            row = sessions.get(sid, {})
            conv = conversations.get(sid, '')
            by_id[sid] = {
                'role': role, 'activity': row.get('activity_state'), 'terminated': bool(row.get('is_terminated')),
                'hibernated': bool(row.get('hibernated_at')), 'workspace': row.get('workspace_path', ''), 'convId': conv,
                'providerConversation': row.get('provider_conversation_id') or row.get('agent_session_id') or '',
                'prompt': A.digest(row.get('prompt', '')),
                'policy': (policy.get(sid) or {}).get('policy', 'allowed' if policy is not None else None),
                'stopReason': (policy.get(sid) or {}).get('stop_reason', ''),
                'fingerprint': self.fingerprint(row['workspace_path']) if with_fingerprints and row.get('workspace_path') else None}
        return {
            'sessions': by_id,
            'turns': {sid: [{'id': t['id'], 'state': t['state'], 'providerTurnId': t['provider_turn_id']}
                            for t in turns if t['handled_by_session_id'] == sid] for sid in by_id},
            'messages': {sid: [{'id': m['id'], 'origin': m['origin'], 'role': m['role'], 'text': m['text'], 'key': m['client_message_id'],
                                'turnId': m['turn_id'], 'createdAt': m['created_at']}
                               for m in messages if conversations.get(sid) == m['conversation_id']] for sid in by_id},
            'daemonRuns': len(runs), 'intents': len(intents), 'intentStates': self.intent_states(intents),
            'hasPolicyTable': bool(policy) or self.has_table('session_auto_resume')}

    @staticmethod
    def intent_states(rows):
        """{session id: state} from session_resume_intents; '_observed' says whether the daemon wrote any row."""
        states = {'_observed': bool(rows)}
        for row in rows:
            try:
                states[row['session_id']] = json.loads(row['record']).get('state') or 'unknown'
            except (ValueError, KeyError):
                states[row.get('session_id', '?')] = 'unreadable'
        return states

    def fingerprint(self, path):
        fp = tree_fingerprint(path)
        if fp.get('exists'):
            marker = Path(path) / 'ar-dirty.txt'
            fp['marker'] = hashlib.sha256(marker.read_bytes()).hexdigest() if marker.is_file() else None
            try:
                fp['head'] = self.git(path, 'rev-parse', 'HEAD')
            except (subprocess.CalledProcessError, OSError):
                fp['head'] = None
        return fp

    def has_table(self, name):
        with A.db_open(self.db) as db:
            return bool(db.execute('select 1 from sqlite_master where type=? and name=?', ('table', name)).fetchone())

    def hosts(self, current=None):
        """{session id: [host pid]} for chat hosts of this run (marker env or ancestry)."""
        current = current or self.track()
        found = collections.defaultdict(list)
        for pid, (_, _, command) in current.items():
            if pid in self.owned or f'AO_ACCEPTANCE_RUN_ID={self.token}' in command:
                match = re.search(r'\bchat-host\s+(\S+)\s', command)
                if match:
                    found[match.group(1)].append(pid)
        return dict(found)

    def descendants(self, roots, current):
        out, changed = set(roots), True
        while changed:
            changed = False
            for pid, (parent, _, _) in current.items():
                if parent in out and pid not in out:
                    out.add(pid)
                    changed = True
        return out

    def sample(self):
        current = self.track()
        with A.db_open(self.db) as db:
            rows = {r['id']: r for r in A.rows(db, 'sessions')}
        view = {sid: {'activity': rows.get(sid, {}).get('activity_state'), 'terminated': bool(rows.get(sid, {}).get('is_terminated')),
                      'hibernated': bool(rows.get(sid, {}).get('hibernated_at'))} for sid in self.sessions.values()}
        with A.db_open(self.db) as db:
            intents = self.intent_states(A.rows(db, 'session_resume_intents'))
        item = {'t': time.time(), 'sessions': view, 'hosts': self.hosts(current), 'intents': intents}
        self.timeline.append(item)
        return item

    # -- setup ---------------------------------------------------------------------------------------------------
    def make_repo(self, name):
        repo = self.root / 'repos' / name
        repo.mkdir(parents=True)
        (repo / 'README.md').write_text(f'# {name}\n')
        (repo / 'AGENTS.md').write_text('LOCAL ONLY. No push, network or work outside this scratch root.\n')
        self.git(repo, 'init', '-b', 'main')
        self.git(repo, 'add', '.')
        self.git(repo, 'commit', '-m', 'test: seed fixture repo')
        return repo

    def register(self, project, harness):
        repo = self.make_repo(project)
        role = {'agent': harness, 'agentConfig': {'permissions': 'default'}}
        config = {'defaultBranch': 'main', 'worker': role, 'orchestrator': role,
                  'agentRules': 'Local-only acceptance. No push. Follow instructions literally.'}
        self.require('POST', '/api/v1/projects', {'path': str(repo), 'projectId': project, 'config': config})

    def spawn(self, role, project, kind, prompt, bypass=False):
        body = {'projectId': project, 'kind': kind, 'harness': self.harness_for(role), 'mode': 'chat',
                'prompt': prompt, 'displayName': role}
        if bypass and self.args.mode == 'real':
            body['approvalMode'] = self.args.approval_mode
        sid = self.require('POST', '/api/v1/sessions', body)['session']['id']
        self.sessions[role] = sid
        return sid

    def build_state(self):
        roles = self.spec['roles']
        harness = self.harness_for('coord')
        for project in ('ar-coord', 'ar-repo', 'ar-repo2', 'ar-busy', 'ar-never', 'ar-work'):
            self.register(project, harness)
        coordinator = self.require('POST', '/api/v1/settings/coordination/project', {'agent': harness})['projectId']
        self.require('PUT', '/api/v1/settings/coordination', {'coordinatorProjectId': coordinator,
                                                              'managedProjectIds': ['ar-repo', 'ar-repo2', 'ar-busy', 'ar-never', 'ar-work']})
        existing = [s for s in self.require('GET', '/api/v1/sessions')['sessions']
                    if s.get('projectId') == coordinator and s.get('kind') == 'orchestrator']
        self.sessions['coord'] = existing[0]['id'] if existing else self.require('POST', '/api/v1/sessions', {
            'projectId': coordinator, 'kind': 'orchestrator', 'harness': harness, 'mode': 'chat'})['session']['id']
        token = lambda role: f'AR_ORIG_{role.upper()}_{self.token[:8]}'  # noqa: E731
        busy = lambda role: (f'{token(role)} Run exactly {SLEEP_COMMAND} as a single standalone shell command, '  # noqa: E731
                             'wait for it to finish, then reply with the single word DONE.')
        quiet = lambda role: f'{token(role)} Reply with the single word READY and do nothing else.'  # noqa: E731
        plan = {'mgr_idle': ('ar-repo', 'orchestrator', quiet, False), 'mgr_idle2': ('ar-repo2', 'orchestrator', quiet, False), 'mgr_busy': ('ar-busy', 'orchestrator', busy, True),
                'wkr': ('ar-work', 'worker', busy, True), 'wkr_fail': ('ar-work', 'worker', busy, True),
                'codex_wkr': ('ar-work', 'worker', busy, True), 'never_orch': ('ar-never', 'orchestrator', quiet, False),
                'stopped': ('ar-work', 'worker', quiet, False), 'terminated': ('ar-work', 'worker', quiet, False)}
        for role in roles:
            if role == 'coord':
                continue
            project, kind, make, bypass = plan[role]
            self.tokens[self.spawn(role, project, kind, make(role), bypass)] = token(role)
        self.detail['sessions'] = dict(self.sessions)

    def wait_state(self, role, predicate, timeout, what):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            snap = self.read_snapshot()
            sid = self.sessions[role]
            if predicate(snap['sessions'][sid], snap['turns'][sid]):
                return snap
            time.sleep(1)
        raise RuntimeError(f'precondition not reached: {role} {what}')

    def reach_preconditions(self):
        t = self.args.setup_timeout
        for role in self.spec['roles']:
            if role in ('wkr', 'wkr_fail', 'codex_wkr', 'mgr_busy'):
                self.wait_state(role, lambda s, turns: any(x['state'] == 'running' and x['providerTurnId'] for x in turns)
                                and s['activity'] in ('active', 'waiting_input', 'blocked'), t, 'mid-turn')
            elif role in ('mgr_idle', 'mgr_idle2', 'never_orch', 'stopped', 'terminated'):
                self.wait_state(role, lambda s, turns: s['activity'] == 'idle' and any(x['state'] == 'completed' for x in turns), t, 'idle after a turn')
                if role in ('mgr_idle', 'mgr_idle2') and self.read_snapshot()['sessions'][self.sessions[role]]['hibernated']:
                    # The idle sweeper got there first: boot parking would not be exercised.
                    raise RuntimeError(f'precondition invalid: {role} was already hibernated before the restart')
        if 'coord' in self.spec['roles']:
            # Nothing is asked of the coordinator: it must be idle (and live) so it is the always-resumed idle case.
            self.wait_state('coord', lambda s, turns: s['activity'] == 'idle' and not s['terminated'], t, 'idle')
        if 'stopped' in self.sessions:
            status, _ = self.api('POST', f'/api/v1/sessions/{self.sessions["stopped"]}/exit-agent', {})
            self.detail['stopStatus'] = status
            self.wait_state('stopped', lambda s, turns: s['activity'] == 'exited', 30, 'exited by the user')
        if 'terminated' in self.sessions:
            status, _ = self.api('POST', f'/api/v1/sessions/{self.sessions["terminated"]}/kill', {})
            self.detail['killStatus'] = status
            self.wait_state('terminated', lambda s, turns: s['terminated'], 30, 'terminated')
        if 'never_orch' in self.sessions:
            status, _ = self.api('PUT', f'/api/v1/sessions/{self.sessions["never_orch"]}/auto-resume', {'policy': 'never'})
            self.detail['neverPolicyStatus'] = status
        probe_status, _ = self.api('PUT', f'/api/v1/sessions/{self.sessions["coord"]}/auto-resume', {'policy': 'allowed'})
        self.detail['autoResumeEndpointStatus'] = probe_status
        for sid in self.sessions.values():  # real state in the worktrees: an untracked file per live session
            row = self.read_snapshot()['sessions'][sid]
            workspace = Path(row['workspace']) if row['workspace'] else None
            if workspace and workspace.is_dir() and workspace.resolve().is_relative_to(self.root):
                (workspace / 'ar-dirty.txt').write_text(f'dirty worktree marker for {sid}\n')

    def quarantine_history(self, role):
        """Make the saved provider conversation of one session unresumable, inside the scratch root only."""
        sid = self.sessions[role]
        conv = self.read_snapshot()['sessions'][sid]['providerConversation']
        moved = []
        if self.args.mode == 'mechanics':
            (self.root / 'fail-load' / provider_sid(conv)).write_text('missing\n')
            moved.append('fixture')
        else:
            quarantine = self.root / 'quarantine'
            quarantine.mkdir(exist_ok=True)
            for path in list(self.home.rglob('*')):
                if conv and provider_sid(conv) in path.name and path.is_file() and not path.is_symlink() and path.resolve().is_relative_to(self.root):
                    target = quarantine / path.name
                    path.rename(target)
                    moved.append(path.name)
        self.detail['quarantined'] = {'role': role, 'items': len(moved)}
        if not moved:
            raise RuntimeError('no saved provider history found to quarantine')

    # -- restart simulation --------------------------------------------------------------------------------------
    def snapshot_role(self, sid):
        return next((role for role, value in self.sessions.items() if value == sid), sid)

    def signal_pids(self, pids, sig):
        current = self.process_rows()
        for pid in pids:
            if pid in current and current[pid][1] == self.owned.get(pid):
                try:
                    os.kill(pid, sig)
                except ProcessLookupError:
                    pass

    def wait_gone(self, pids, timeout):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            current = self.track()
            if not [p for p in pids if p in current and current[p][1] == self.owned.get(p)]:
                return True
            time.sleep(.25)
        return False

    def simulate_restart(self):
        current = self.track()
        daemon = self.daemon.pid
        hosts = {pid for pids in self.hosts(current).values() for pid in pids}
        group = self.descendants(hosts, current) - {daemon}
        by_role = {self.snapshot_role(sid): len(pids) for sid, pids in self.hosts(current).items()}
        self.detail['restartInputs'] = {'hostPids': len(hosts), 'hostAndProviderPids': len(group), 'hostsByRole': by_role}
        order = self.spec['order']
        started = time.time()
        if order == 'kill9':
            self.signal_pids([daemon, *group], signal.SIGKILL)
        elif order == 'hosts-first':
            self.signal_pids(group, signal.SIGTERM)
            time.sleep(self.args.hosts_first_gap)
            self.signal_pids([daemon], signal.SIGTERM)
        else:  # daemon-first
            self.signal_pids([daemon], signal.SIGTERM)
            if not self.wait_gone([daemon], self.args.shutdown_wait):
                self.signal_pids([daemon], signal.SIGKILL)
                self.detail['daemonNeededKill'] = True
            self.signal_pids(group, signal.SIGTERM)
        if not self.wait_gone([daemon, *group], self.args.shutdown_wait):
            self.detail['forcedAfterTerm'] = True
            self.signal_pids([daemon, *group], signal.SIGKILL)
        gone = self.wait_gone([daemon, *group], 10)
        self.detail['restartSimulation'] = {'order': order, 'seconds': round(time.time() - started, 2), 'allProcessesGone': gone}
        self.check('hosts_and_daemon_down_before_restart', gone)
        return started

    # -- run -----------------------------------------------------------------------------------------------------
    def observe_resume(self, restart_at):
        """Sample until the daemon has settled by itself, with no writes except the wake probe afterwards."""
        began = time.monotonic()
        last_change, previous = began, None
        while time.monotonic() - began < self.args.settle_timeout:
            item = self.sample()
            signature = json.dumps([item['sessions'], {k: len(v) for k, v in item['hosts'].items()}], sort_keys=True)
            if signature != previous:
                last_change, previous = time.monotonic(), signature
            expected_live = [self.sessions[r] for r in self.spec['roles'] if CLASSES.get(r, 'working') in ('working', 'working-idle')]
            live_now = all(item['hosts'].get(s) for s in expected_live)
            if live_now and time.monotonic() - last_change >= self.args.quiet:
                break
            if time.monotonic() - last_change >= max(self.args.giveup, self.spec.get('giveup', 0)):
                self.detail['gaveUpWaiting'] = True  # nothing changes by itself: the feature is absent or stuck
                break
            time.sleep(self.args.poll)
        time.sleep(self.args.final_dwell)  # catch late duplicates before the verdict
        self.sample()

    def simulated_resumer(self):
        """POSITIVE CONTROL, fixture runs only: stands in for the daemon's resumer through the public
        resume API, in stage order, so the process-level gates are shown able to PASS. It is a manual
        intervention by construction, so no_manual_intervention_after_restart fails and the run never certifies."""
        failed = self.spec.get('fail')
        for stage in STAGES:
            for role in stage:
                if role in self.sessions and role != failed:
                    sid = self.sessions[role]
                    for _attempt in range(4):  # a boot-time retry of the daemon's own may briefly hold the resume
                        status, body = self.api('POST', f'/api/v1/sessions/{sid}/resume-agent', {})
                        if status != 409:
                            break
                        time.sleep(2)
                    self.detail.setdefault('simulatedResumes', {})[role] = {'status': status, 'errorCode': body.get('errorCode', '')}
                    deadline = time.monotonic() + 40
                    while time.monotonic() < deadline and not self.hosts().get(sid):
                        self.sample()
                        time.sleep(.5)
                    self.sample()

    def probe_wake(self, snap):
        """One probe per delivery path against a PARKED manager (a woken session is no longer parked):
        a human conversation message to mgr_idle, and an `ao send` from the coordinator to mgr_idle2."""
        probes = {'mgr_idle': ('conversation message', lambda sid: ('POST', f'/api/v1/sessions/{sid}/conversation/messages',
                                                                    {'text': WAKE_PROBE_TEXT, 'clientMessageId': f'ar-wake-{self.token[:8]}'})),
                  'mgr_idle2': ('coordinator ao send', lambda sid: ('POST', f'/api/v1/sessions/{sid}/send',
                                                                    {'message': WAKE_PROBE_TEXT, 'senderSessionId': self.sessions['coord']}))}
        for role, (path, request) in probes.items():
            if role not in self.sessions:
                continue
            sid = self.sessions[role]
            before_turns = {t['id'] for t in snap['turns'][sid]}
            status, _ = self.api(*request(sid))
            result = {'path': path, 'status': status, 'turnStarted': False, 'live': False, 'sameConversation': False}
            deadline = time.monotonic() + self.args.wake_timeout
            while time.monotonic() < deadline and 200 <= status < 300:
                after = self.read_snapshot()
                new = [t for t in after['turns'][sid] if t['id'] not in before_turns and t['state'] in ('running', 'completed')]
                result['turnStarted'] = bool(new)
                result['live'] = bool(self.hosts().get(sid))
                result['sameConversation'] = after['sessions'][sid]['providerConversation'] == snap['sessions'][sid]['providerConversation']
                if result['turnStarted'] and result['live']:
                    break
                time.sleep(1)
            self.wake[sid] = result

    def run_cycle(self):
        self.build_state()
        self.reach_preconditions()
        if self.spec.get('fail'):
            self.quarantine_history(self.spec['fail'])
        before = self.read_snapshot(with_fingerprints=True)
        self.check('preconditions', True)
        self.detail['before'] = self.public(before)
        restart_at = self.simulate_restart()
        mutations_before = len(self.mutations)
        self.start()
        daemon_ready_at = time.time()
        for kill in range(int(self.spec.get('flap') or 0)):
            time.sleep(self.args.flap_gap)
            self.signal_pids([self.daemon.pid], signal.SIGKILL)
            self.wait_gone([self.daemon.pid], 10)
            self.detail['flapKills'] = kill + 1
            self.start()
        if self.args.simulate_resumer:
            self.simulated_resumer()
        self.observe_resume(restart_at)
        after = self.read_snapshot(with_fingerprints=True)
        self.probe_wake(after)
        self.sample()
        final = self.read_snapshot(with_fingerprints=True)
        self.evaluate(before, after, final, restart_at, daemon_ready_at, mutations_before)

    def public(self, snap):
        """IDs, states and counts only: no message text and no raw provider data."""
        return {'sessions': {sid: {k: v for k, v in view.items() if k != 'fingerprint'} | {
                    'worktree': {k: v for k, v in (view.get('fingerprint') or {}).items() if k in ('exists', 'entries', 'digest')}}
                             for sid, view in snap['sessions'].items()},
                'turnCounts': {sid: collections.Counter(t['state'] for t in turns) for sid, turns in snap['turns'].items()},
                'messageCounts': {sid: len(m) for sid, m in snap['messages'].items()},
                'daemonRuns': snap['daemonRuns'], 'resumeIntents': snap['intents'], 'intentStates': snap['intentStates']}

    def notices(self, after):
        coord = self.sessions['coord']
        return [{'id': m['id'], 'text': m['text'], 'createdAt': m['createdAt']} for m in after['messages'].get(coord, [])
                if m['origin'] in ('daemon', 'automation') and NOTICE_TEXT.search(m['text'])]

    def evaluate(self, before, pre_wake, after, restart_at, daemon_ready_at, mutations_before):
        r = self.sessions
        role = lambda *names: [r[n] for n in names if n in r]  # noqa: E731
        names = lambda n: [n, r[n]]  # noqa: E731  the product writes the DISPLAY NAME (= role here), falling back to the id
        failed = self.spec.get('fail')
        working = [n for n in ('mgr_busy', 'wkr', 'codex_wkr') if n in r and n != failed]
        parked = role('mgr_idle', 'mgr_idle2')
        excluded = role('never_orch', 'stopped', 'terminated')
        resumed = role('coord', *working)
        shim = [c for c in self.shim_calls() if c['t'] >= restart_at]
        notices = self.notices(after)
        intents = after['intentStates']
        gates = self.gates
        gates['auto_resume_endpoint_present'] = gate(self.detail.get('autoResumeEndpointStatus') not in (None, 404, 405, 501),
                                                     status=self.detail.get('autoResumeEndpointStatus'))
        if 'never_orch' in r:
            gates['never_policy_applied'] = gate(after['sessions'][r['never_orch']]['policy'] == 'never',
                                                 policyStatus=self.detail.get('neverPolicyStatus'))
        gates['coordinator_resumed_actively'] = active_resume_gate(self.timeline, before, after, role('coord'))
        if working:
            gates['working_sessions_resumed_actively'] = active_resume_gate(self.timeline, before, after, role(*working))
        stages = [[r[n] for n in stage if n in r and n != failed] for stage in STAGES]
        stages = [stage for stage in stages if stage]
        if len(stages) > 1:
            gates['stage_barrier_order'] = stage_order_gate(self.timeline, stages)
            gates['stage_barrier_by_intent_state'] = intent_barrier_gate(self.timeline, stages)
        if parked:
            gates['idle_sessions_hibernated'] = hibernate_gate(self.timeline, pre_wake, parked)
            gates['idle_sessions_hibernated']['intentStates'] = {sid: intents.get(sid) for sid in parked}
            if self.args.mode == 'mechanics':
                gates['idle_sessions_hibernated']['fixtureNote'] = ('parking needs the adapter history probe to prove the saved conversation exists; '
                                                                    'the fixture provider leaves no saved state, so a daemon that resumes instead is behaving per design')
            gates['hibernated_sessions_wake_on_message'] = wake_gate(self.wake)
        if excluded:
            gates['excluded_sessions_stay_down'] = stays_down_gate(self.timeline, before, after, role('stopped', 'terminated'), intents=intents)
            if 'never_orch' in r:
                gates['never_session_stays_down_with_policy'] = stays_down_gate(
                    self.timeline, before, after, role('never_orch'), 'never', intents=intents)
        if 'stopped' in r:
            gates['user_stop_reason_recorded'] = gate(after['sessions'][r['stopped']]['stopReason'] == 'user_stop',
                                                      stopReason=after['sessions'][r['stopped']]['stopReason'])
        gates['intents_match_expectations'] = intent_expectation_gate(
            intents, resumed, parked, excluded, role(failed) if failed else ())
        gates['worktrees_preserved'] = worktree_gate(before, pre_wake, after, [*excluded, *parked, *role(failed or '')], resumed, self.timeline)
        if working:
            gates['one_keyed_continue_turn'] = continue_turn_gate(
                before, after, role(*working), role('coord', 'mgr_idle', 'mgr_idle2', 'never_orch', 'stopped', 'terminated'))
        gates['no_fresh_relaunch_or_prompt_replay'] = no_fresh_relaunch_gate(
            before, after, shim, restart_at, resumed, self.tokens, self.timeline)
        if 'codex_wkr' in r:
            gates['codex_midturn_settles_or_clean_notice'] = codex_settle_gate(
                self.timeline, after, notices, r['codex_wkr'], restart_at, self.args.codex_settle, [names('codex_wkr')[0]])
        if failed:
            gates['one_stopped_work_notice_on_resume_failure'] = failure_notice_gate(
                notices, {'sid': r[failed], 'names': names(failed)}, [names('wkr'), [r['coord']]], after, self.timeline)
            gates['other_sessions_still_resume'] = active_resume_gate(self.timeline, before, after, role('coord', 'wkr'))
        if self.spec.get('flap'):
            gates['flap_exceeded_boot_storm_threshold'] = gate(self.starts > 3, daemonStarts=self.starts)  # harness precondition
            gates['flap_no_duplicate_resumes'] = duplicate_gate(self.timeline, after, resumed)
        else:
            gates['no_duplicate_hosts_or_continue'] = duplicate_gate(self.timeline, after, resumed)
        writes = [m for m in self.mutations[mutations_before:]
                  if not (m['method'] == 'POST' and (m['route'].endswith('/conversation/messages') or m['route'].endswith('/send')))]
        gates['no_manual_intervention_after_restart'] = gate(not writes, writes=len(writes))
        gates['daemon_log_clean_of_panics'] = gate(not self.log_has_panic(), log='daemon.log')
        self.detail['after'] = self.public(after)
        self.detail['timeline'] = {'samples': len(self.timeline), 'seconds': round(self.timeline[-1]['t'] - restart_at, 2),
                                   'daemonReadyAfter': round(daemon_ready_at - restart_at, 2), 'daemonStarts': self.starts}
        self.detail['continueKeys'] = {sid: [m['key'] for m in continue_messages(after, sid)[0]] for sid in resumed}
        self.detail['wake'] = self.wake
        self.detail['noticeCount'] = len(notices)
        self.detail['fixtureProviderCalls'] = collections.Counter(c['method'] for c in shim)

    def log_has_panic(self):
        log = self.root / 'daemon.log'
        return log.exists() and re.search(r'^(panic:|fatal error:)', log.read_text(errors='replace'), re.M) is not None


# ---------------------------------------------------------------------------------------------------------------------

def run_cycle(args, name):
    cycle = None
    try:
        cycle = Cycle(args, name)
        signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
        signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(RuntimeError('cycle wall-clock timeout')))
        signal.alarm(int(args.cycle_timeout))
        cycle.result['binary'] = {'path': str(args.daemon), 'sha256': A.hashlib.sha256(args.daemon.read_bytes()).hexdigest()}
        cycle.start()
        cycle.check('isolated_daemon_ready', True)
        cycle.run_cycle()
    except (Exception, KeyboardInterrupt) as error:
        if cycle is None:
            return {'cycle': name, 'errors': [type(error).__name__ + ': harness initialization failed'], 'gates': {}, 'safety': {}}
        cycle.result['errors'].append(type(error).__name__ + (': ' + str(error) if isinstance(error, RuntimeError) else ''))
    finally:
        signal.alarm(0)
        if cycle:
            cycle.finalize()
    safety = {k: v for k, v in cycle.result['assertions'].items()}
    return {'cycle': name, 'scratchRoot': str(cycle.root), 'errors': cycle.result['errors'], 'gates': cycle.gates, 'safety': safety,
            'detail': cycle.detail, 'mutationAudit': cycle.mutations, 'binary': cycle.result.get('binary')}


def emit(args, results, started):
    output = Path(args.output).resolve()
    output.mkdir(parents=True, exist_ok=True)
    output.chmod(0o700)
    flat = {}
    for item in results:
        for name, g in {**item['gates'], **item['safety']}.items():
            flat[f"{item['cycle']}.{name}"] = g['passed']
        if item['errors']:
            flat[f"{item['cycle']}.no_errors"] = False
    ok = bool(flat) and all(flat.values()) and args.mode == 'real' and not args.simulate_resumer
    document = {'schemaVersion': 1, 'kind': 'ed7-auto-resume-acceptance', 'mode': args.mode, 'passed': ok,
                'mechanicsNeverCertifies': args.mode == 'mechanics', 'simulatedResumer': bool(args.simulate_resumer), 'seconds': round(time.time() - started, 1),
                'daemon': {'path': str(args.daemon), 'sha256': hashlib.sha256(args.daemon.read_bytes()).hexdigest()},
                'cycles': results, 'flatGates': flat,
                'harnessSourceSha256': hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                'harnessSha': subprocess.run(['/usr/bin/git', '-C', str(Path(__file__).resolve().parents[2]), 'rev-parse', 'HEAD'],
                                             capture_output=True, text=True).stdout.strip()}
    path = output / 'result.json'
    path.write_text(json.dumps(document, indent=2, default=str) + '\n')
    path.chmod(0o600)
    lines = ['# ED7 auto-resume acceptance', '',
             '**' + ('PASS' if ok else 'NOT ACCEPTED') + '**' + (' (fixture-provider mechanics run: never certifies the feature)' if args.mode == 'mechanics' else ''), '',
             f'Daemon SHA256: `{document["daemon"]["sha256"]}`.', '', '| Cycle.gate | Result |', '|---|---|']
    lines += [f'| {k} | {"PASS" if v else "FAIL / NOT OBSERVED"} |' for k, v in flat.items()]
    (output / 'summary.md').write_text('\n'.join(lines) + '\n')
    print(json.dumps({'result': str(path), 'summary': str(output / 'summary.md'), 'passed': ok,
                      'failedGates': [k for k, v in flat.items() if not v]}))
    return 0 if ok else 1


def parse(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    binary = parser.add_mutually_exclusive_group(required=True)
    binary.add_argument('--daemon', type=Path)
    binary.add_argument('--app', type=Path, help='Resolve bundled daemon only; never launch the GUI')
    parser.add_argument('--mode', choices=('mechanics', 'real'), default='mechanics')
    parser.add_argument('--cycles', default='hosts-first,daemon-first,kill9,flap,resume-failure',
                        help='comma separated subset of ' + ','.join(CYCLES))
    parser.add_argument('--default-harness', default='claude-code')
    parser.add_argument('--harness-map', type=lambda s: dict(i.split('=', 1) for i in s.split(',') if i), default={'codex_wkr': 'codex'},
                        help='role=harness overrides for real mode, e.g. codex_wkr=codex')
    parser.add_argument('--approval-mode', default='bypass-permissions', help='real mid-turn sessions only; confined to the scratch root')
    parser.add_argument('--agent-bin-dir', action='append', default=[])
    parser.add_argument('--pass-env', action='append', default=[])
    parser.add_argument('--auth-readonly', action='append', default=[], metavar='SOURCE=HOME_RELATIVE_DEST')
    parser.add_argument('--load-delay', type=float, default=1.5, help='fixture provider: seconds session/load takes, so stage ordering is observable')
    parser.add_argument('--simulate-resumer', action='store_true', help='mechanics only: positive control that resumes through the public API; never certifies')
    parser.add_argument('--cycle-timeout', type=float, default=1800, help='hard wall-clock limit per cycle; teardown still runs')
    parser.add_argument('--setup-timeout', type=float, default=240)
    parser.add_argument('--settle-timeout', type=float, default=300)
    parser.add_argument('--quiet', type=float, default=20, help='seconds without change before the verdict')
    parser.add_argument('--giveup', type=float, default=60, help='seconds without any change before concluding nothing will resume')
    parser.add_argument('--final-dwell', type=float, default=15)
    parser.add_argument('--poll', type=float, default=.5)
    parser.add_argument('--wake-timeout', type=float, default=90)
    parser.add_argument('--codex-settle', type=float, default=120)
    parser.add_argument('--hosts-first-gap', type=float, default=1.0)
    parser.add_argument('--shutdown-wait', type=float, default=15)
    parser.add_argument('--flap-gap', type=float, default=3)
    parser.add_argument('--scratch-parent', type=Path, default=Path('/private/tmp'))
    parser.add_argument('--output', type=Path, default=A.ARTIFACTS / 'ed7-auto-resume-latest')
    args = parser.parse_args(argv)
    args.daemon = (args.app / 'Contents/Resources/daemon/ao' if args.app else args.daemon).resolve()
    if not args.daemon.is_file() or not os.access(args.daemon, os.X_OK):
        parser.error('daemon must be an executable file')
    unknown = [c for c in args.cycles.split(',') if c not in CYCLES]
    if unknown:
        parser.error('unknown cycles: ' + ','.join(unknown))
    if args.simulate_resumer and args.mode != 'mechanics':
        parser.error('--simulate-resumer is a fixture-only positive control')
    if args.mode == 'mechanics' and (args.pass_env or args.auth_readonly or args.agent_bin_dir):
        parser.error('provider inputs require explicit --mode real')
    # Harness.__init__ reads these.
    args.scenario, args.answers, args.agent = 'full', {}, args.default_harness
    return args


def main(argv=None):
    args = parse(argv)
    started = time.time()
    results = [run_cycle(args, name) for name in args.cycles.split(',')]
    return emit(args, results, started)


if __name__ == '__main__':
    sys.exit(main())
