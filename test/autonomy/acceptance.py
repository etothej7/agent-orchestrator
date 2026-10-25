#!/usr/bin/env python3
"""Local-only one-request acceptance. No third-party Python dependencies.

Exit 0: all acceptance assertions passed; 1: observed failure/unsupported;
2: harness setup/error. Never turn a timeout or missing observation into PASS.
"""
import argparse
import collections
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

ARTIFACTS = Path('/Users/edstockman/.ao/data/artifacts/agent-orchestrator-4')
PRIVATE = 'AO_PRIVATE_REASONING_SENTINEL_6'
PURPOSES = {'scope_question', 'blocking_decision', 'completion_decision'}
SHA = re.compile(r'^[0-9a-f]{40}$')


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, default=str).encode()).hexdigest()


def stamp(value):
    try:
        return dt.datetime.fromisoformat(value.replace('Z', '+00:00')).timestamp()
    except (ValueError, TypeError, AttributeError):
        return None


def db_open(path):
    db = sqlite3.connect(path.resolve().as_uri() + '?mode=ro', uri=True, timeout=5)
    db.row_factory = sqlite3.Row
    return db


def rows(db, table, columns='*'):
    if not db.execute('select 1 from sqlite_master where type=? and name=?', ('table', table)).fetchone():
        return []
    return [dict(r) for r in db.execute(f'SELECT {columns} FROM "{table}"')]


def read_state(path):
    with db_open(path) as db:
        return {name: rows(db, name) for name in (
            'sessions', 'conversation_turns', 'conversation_messages',
            'conversation_activities', 'reports', 'workspace_story_events',
            'story_continuations', 'notifications')}


def records(state, table):
    return [json.loads(r['record']) for r in state.get(table, [])]


def privacy_violations(payload, handles=()):
    """Inspect public payloads in memory; return reasons, never leaked values."""
    bad = set()
    forbidden = {'providerPayload', 'providerItemId', 'providerTurnId', 'deliveryId',
                 'claimToken', 'rawCommand', 'threadState', 'reasoning', 'privateReasoning'}
    def walk(value):
        if isinstance(value, dict):
            for k, v in value.items():
                if k in forbidden and v:
                    bad.add('private_field:' + k)
                if k == 'activityKind' and v == 'reasoning':
                    bad.add('reasoning_activity')
                walk(v)
        elif isinstance(value, list):
            for v in value:
                walk(v)
        elif isinstance(value, str):
            if PRIVATE in value:
                bad.add('private_sentinel')
            if any(h and h in value for h in handles):
                bad.add('delivery_handle')
            if re.search(r'<(?:analysis|thinking)>|chain.of.thought', value, re.I):
                bad.add('private_reasoning_marker')
    walk(payload)
    return sorted(bad)


def classify_message(message):
    if message.get('role') == 'user':
        return 'human_input'
    purpose = message.get('purpose')
    if purpose in PURPOSES and message.get('audience') == 'human':
        text = message.get('text', '')
        if 'Why your input is needed:' in text and text.split('Why your input is needed:')[0].strip():
            return purpose
    return 'non_actionable_or_unclassified'


def workflow_classification(story, continuation, active, elapsed, stall_seconds):
    if story.get('status') == 'done':
        return 'completed_no_ready_work'  # evidence validity checked separately
    if story.get('question') or story.get('blockedReason'):
        return 'blocked_no_ready_work'
    if active:
        return 'active'
    if continuation.get('state') in ('exhausted', 'blocked') or elapsed >= stall_seconds:
        return 'stalled_ready_work'
    return 'idle_agent_pending_work'


def latest_evidence(story, events):
    """Require ordered, same-SHA independent evidence; later failures revoke it."""
    impl = test = review = author = reviewer = ''
    for e in sorted(events, key=lambda e: (e.get('createdAt', ''), e.get('id', ''))):
        kind = e.get('kind')
        if kind == 'implementation':
            impl, author = e.get('reference', ''), e.get('senderSessionId', '')
            test = review = reviewer = ''
        elif kind == 'testing':
            test = e.get('reference', '') if e.get('result') == 'passed' else ''
        elif kind == 'review_passed':
            review, reviewer = e.get('reference', ''), e.get('senderSessionId', '')
        elif kind in ('review_failed', 'native_review'):
            # Native PR reviews do not substitute for this local actor review trial.
            review = reviewer = ''
    valid = bool(SHA.fullmatch(impl) and impl == test == review and author and reviewer and author != reviewer)
    valid = valid and not story.get('question') and not story.get('blockedReason')
    return {'valid': valid, 'sha': impl, 'author': author, 'reviewer': reviewer}


def duplicate_dispatches(messages):
    seen = collections.defaultdict(set)
    for m in messages:
        key = m.get('client_message_id')
        if key:
            seen[(m.get('conversation_id'), key)].add(m.get('turn_id'))
    return [digest(k) for k, turns in seen.items() if len(turns) > 1]


class Harness:
    def __init__(self, args):
        self.args = args
        self.root = Path(tempfile.mkdtemp(prefix='ao-acceptance-', dir=args.scratch_parent)).resolve()
        self.root.chmod(0o700)
        self.home = self.root / 'home'
        self.data = self.home / '.ao/data'
        self.data.mkdir(parents=True)
        self.runfile = self.home / '.ao/running.json'
        self.port = self.free_port()
        self.url = f'http://127.0.0.1:{self.port}'
        self.token = uuid.uuid4().hex
        self.daemon = None
        self.owned = {}
        self.project_ids = ['acceptance-labels', 'acceptance-totals']
        self.repos = []
        self.main = ''
        self.mutations = []
        self.observed_messages = {}
        self.observed_notifications = {}
        self.public_errors = set()
        self.samples = []
        self.observed_dispatches = {}
        self.report_times = {}
        self.continuation_history = {}
        self.logical_deliveries = collections.defaultdict(set)
        self.answer_ids = set()
        self.result = {'schemaVersion': 1, 'runId': self.token, 'mode': args.mode,
                       'scratchRoot': str(self.root), 'assertions': {}, 'errors': [],
                       'autoAnswerPolicy': args.answers, 'operatorNudges': 0,
                       'deferred': ['native UI acceptance', 'installation approval'],
                       'scope': 'real runtime evidence only; mechanics mode never certifies autonomy'}
        self.env = self.environment()
        self.profile = self.sandbox_profile(False)

    @staticmethod
    def free_port():
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]

    def environment(self):
        env = {'HOME': str(self.home), 'PATH': str(self.root / 'bin') + ':/usr/bin:/bin:/usr/sbin:/sbin',
               'TMPDIR': str(self.root), 'AO_DATA_DIR': str(self.data), 'AO_RUN_FILE': str(self.runfile),
               'AO_PORT': str(self.port), 'AO_TELEMETRY_REMOTE': 'off', 'AO_HARNESS_UPDATE_CHECKS': 'off',
               'AO_UPDATE_WHILE_RUNNING': 'off', 'AO_OWNER': 'persistent', 'AO_ACCEPTANCE_RUN_ID': self.token,
               'GOMAXPROCS': '4', 'GOFLAGS': '-p=4', 'VITEST_MAX_THREADS': '4',
               'GIT_AUTHOR_NAME': 'AO acceptance', 'GIT_AUTHOR_EMAIL': 'acceptance@localhost',
               'GIT_COMMITTER_NAME': 'AO acceptance', 'GIT_COMMITTER_EMAIL': 'acceptance@localhost'}
        (self.root / 'bin').mkdir()
        (self.root / 'bin/ao').symlink_to(self.args.daemon)
        if self.args.mode == 'real':
            for directory in self.args.agent_bin_dir:
                env['PATH'] += ':' + str(Path(directory).resolve())
            for name in self.args.pass_env:
                if name not in {'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'GEMINI_API_KEY'}:
                    raise ValueError('only explicit provider API keys may be forwarded')
                env[name] = os.environ[name]
            for item in self.args.credential:
                source, relative = item.split('=', 1)
                dest = (self.home / relative).resolve()
                if not dest.is_relative_to(self.home) or dest == self.home:
                    raise ValueError('credential destination must be within isolated HOME')
                dest.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(Path(source).resolve(), dest)
                dest.chmod(0o600)
        return env

    def sandbox_profile(self, seed):
        # macOS confinement is mandatory: no fallback to unconfined execution.
        if sys.platform != 'darwin' or not Path('/usr/bin/sandbox-exec').exists():
            raise RuntimeError('this local-Mac harness requires sandbox-exec')
        q = json.dumps
        live = Path.home() / '.ao'
        text = f'''(version 1)
(allow default)
(deny file-write*)
(allow file-write* (subpath {q(str(self.root))}))
(allow file-write* (literal "/dev/null"))
(deny file-read* (subpath {q(str(live))}))
(deny network*)
(allow network-bind network-inbound (local ip "localhost:{self.port}"))
(allow network-outbound (remote ip "localhost:{self.port}"))
(allow network-bind network-outbound (subpath {q(str(self.root))}))
'''
        if seed:
            text += f'(deny process-exec)\n(allow process-exec (literal {q(str(self.args.daemon))}))\n(deny signal)\n'
        elif self.args.mode == 'real':
            text += '(allow network-outbound (remote tcp "*:443"))\n'
        path = self.root / ('seed.sb' if seed else 'runtime.sb')
        path.write_text(text)
        return path

    def api(self, method, route, body=None, timeout=15):
        if method != 'GET':
            self.mutations.append({'method': method, 'route': route, 'at': time.time(),
                                   'bodyHash': digest(body)})
        data = None if body is None else json.dumps(body).encode()
        request = urllib.request.Request(self.url + route, data=data, method=method,
                                         headers={'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                return response.status, json.loads(response.read() or '{}')
        except urllib.error.HTTPError as e:
            # Never persist raw server error messages (may contain provider output).
            return e.code, {'errorCode': self.error_code(e.read())}

    @staticmethod
    def error_code(raw):
        try:
            obj = json.loads(raw)
            if obj.get('code'):
                return obj['code']
            error = obj.get('error', {})
            return error.get('code', 'HTTP_ERROR') if isinstance(error, dict) else 'HTTP_ERROR'
        except (ValueError, AttributeError):
            return 'HTTP_ERROR'

    def require(self, method, path, body=None):
        status, value = self.api(method, path, body)
        if status >= 400:
            raise RuntimeError(f'{method} {path}: HTTP {status} {value.get("errorCode", "")}')
        return value

    def process_rows(self):
        # Match inherited unique run marker as well as ancestry, including detached
        # runtime hosts. Read in memory only: ps environment may contain secrets.
        output = subprocess.check_output(['/bin/ps', 'eww', '-axo', 'pid=,ppid=,lstart=,command='], text=True)
        result = {}
        for line in output.splitlines():
            parts = line.split(None, 7)
            if len(parts) == 8:
                pid, parent = int(parts[0]), int(parts[1])
                identity = ' '.join(parts[2:7])
                result[pid] = (parent, identity, parts[7])
        return result

    def track(self):
        current = self.process_rows()
        owned = set(self.owned)
        if self.daemon:
            owned.add(self.daemon.pid)
        changed = True
        while changed:
            changed = False
            for pid, (parent, identity, command) in current.items():
                if pid != os.getpid() and (parent in owned or f'AO_ACCEPTANCE_RUN_ID={self.token}' in command):
                    if pid not in owned:
                        owned.add(pid)
                        changed = True
                    self.owned[pid] = identity
        return current

    def stop(self, daemon_only=False, force=False):
        current = self.track()
        selected = [self.daemon.pid] if daemon_only and self.daemon else list(self.owned)
        for sig, delay in [(signal.SIGKILL if force else signal.SIGTERM, 2), (signal.SIGKILL, 1)]:
            current = self.track()
            if not daemon_only:
                selected = list(self.owned)
            for pid in selected:
                if pid in current and current[pid][1] == self.owned.get(pid):
                    try:
                        os.kill(pid, sig)
                    except ProcessLookupError:
                        pass
            time.sleep(delay)
            if self.daemon:
                self.daemon.poll()
        if not daemon_only:
            current = self.track()
            remaining = [pid for pid, identity in self.owned.items()
                         if pid in current and current[pid][1] == identity]
            self.result['cleanup'] = {'remainingPids': remaining, 'ownedPidCount': len(self.owned)}
            self.check('cleanup', not remaining)

    def start(self, seed=False):
        log = open(self.root / ('seed-daemon.log' if seed else 'daemon.log'), 'ab')
        self.daemon = subprocess.Popen(['/usr/bin/sandbox-exec', '-f', str(self.sandbox_profile(seed)),
                                        str(self.args.daemon), 'daemon'], env=self.env,
                                       cwd=self.root, stdout=log, stderr=log, start_new_session=True)
        log.close()
        self.track()
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            if self.daemon.poll() is not None:
                raise RuntimeError('isolated daemon exited before readiness; see private scratch log')
            try:
                code, _ = self.api('GET', '/api/v1/projects', timeout=1)
                if code == 200:
                    return
            except (OSError, TimeoutError):
                pass
            time.sleep(.2)
        raise RuntimeError('isolated daemon readiness timeout')

    def check(self, name, passed, **detail):
        self.result['assertions'][name] = {'passed': bool(passed), **detail}

    def seed_check(self):
        if not self.args.seed_db:
            self.result['seed'] = {'requested': False}
            return
        target = self.data / 'ao.db'
        source = Path(self.args.seed_db).resolve()
        with db_open(source) as src, sqlite3.connect(target) as dest:
            src.backup(dest)
        target.chmod(0o600)
        def preserved():
            with db_open(target) as db:
                return {t: digest(sorted(rows(db, t), key=lambda r: json.dumps(r, sort_keys=True, default=str)))
                        for t in ('conversation_messages', 'projects', 'app_settings')}
        before = preserved()
        self.start(seed=True)
        _, projects = self.api('GET', '/api/v1/projects')
        with db_open(target) as db:
            integrity = db.execute('pragma integrity_check').fetchone()[0]
            foreign = len(db.execute('pragma foreign_key_check').fetchall())
            conversations = rows(db, 'conversations', 'session_id')
        statuses = []
        for row in conversations:
            if row['session_id']:
                statuses.append(self.api('GET', f'/api/v1/sessions/{urllib.parse.quote(row["session_id"], safe="")}/history')[0])
        self.stop()
        after = preserved()
        self.result['seed'] = {'requested': True, 'consistentReadOnlyBackup': True,
                               'tableHashesUnchanged': before == after, 'integrity': integrity,
                               'foreignKeyViolations': foreign, 'historyReadStatuses': dict(collections.Counter(statuses)),
                               'projectCount': len(projects.get('projects', []))}
        self.check('seed_preservation', before == after and integrity == 'ok' and foreign == 0 and bool(statuses) and all(s == 200 for s in statuses))
        # Keep seed state protected, but never let copied actors into the real run.
        seed_home = self.root / 'seed-home'
        self.home.rename(seed_home)
        self.data.mkdir(parents=True)
        for item in self.args.credential:
            _, relative = item.split('=', 1)
            dest = self.home / relative
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(seed_home / relative, dest)
            dest.chmod(0o600)
        self.daemon = None

    def git(self, repo, *args):
        return subprocess.check_output(['/usr/bin/git', '-C', str(repo), *args], env=self.env,
                                       stderr=subprocess.DEVNULL, text=True).strip()

    def fixtures(self):
        for project, function, broken, contract in [
            ('acceptance-labels', 'normalize', 'return value', "assert normalize('  HeLLo World  ') == 'hello-world'\nassert normalize('') == ''"),
            ('acceptance-totals', 'total', 'return value', 'assert total(100) == 105\nassert total(0) == 0\nassert total(-4) == 0')]:
            repo = self.root / 'repos' / project
            repo.mkdir(parents=True)
            (repo / 'feature.py').write_text(f'def {function}(value):\n    {broken}\n')
            (repo / 'check.py').write_text(f'from feature import {function}\n{contract}\nprint("contract passed")\n')
            (repo / 'AGENTS.md').write_text('LOCAL ONLY. No push, network, installation, or work outside this scratch root. '
                'Work on a dedicated local branch/worktree. Run python3 check.py before editing and after the fix. '
                'Do not weaken or modify check.py. Commit implementation and ask a separate reviewer to inspect the exact SHA. '
                'Record actual check exit codes, SHA, review findings and remediation through AO story commands. '
                'Never invent evidence. Cap parallelism at four.\n')
            self.git(repo, 'init', '-b', 'main')
            self.git(repo, 'add', '.')
            self.git(repo, 'commit', '-m', 'test: seed intentionally failing acceptance contract')
            self.repos.append({'project': project, 'path': str(repo), 'base': self.git(repo, 'rev-parse', 'HEAD'),
                               'contract': contract, 'function': function})
        self.result['fixtures'] = [{k: v for k, v in r.items() if k not in ('contract', 'function')} for r in self.repos]

    def bootstrap(self):
        for fixture in self.repos:
            role = {'agent': self.args.agent, 'agentConfig': {'permissions': 'default'}}
            config = {'defaultBranch': 'main', 'worker': role, 'orchestrator': role,
                      'agentRules': 'Local-only acceptance. No push. Use real commands and truthful evidence. Max parallelism 4.'}
            self.require('POST', '/api/v1/projects', {'path': fixture['path'], 'projectId': fixture['project'], 'config': config})
        coordinator = self.require('POST', '/api/v1/settings/coordination/project', {'agent': self.args.agent})['projectId']
        self.require('PUT', '/api/v1/settings/coordination', {'coordinatorProjectId': coordinator, 'managedProjectIds': self.project_ids})
        sessions = self.require('GET', '/api/v1/sessions')['sessions']
        existing = [s for s in sessions if s.get('projectId') == coordinator and s.get('kind') == 'orchestrator']
        if existing:
            self.main = existing[0]['id']
        else:
            response = self.require('POST', '/api/v1/sessions', {'projectId': coordinator, 'kind': 'orchestrator',
                'harness': self.args.agent, 'mode': 'chat'})
            self.main = response['session']['id']
        self.result['mainSessionId'] = self.main

    def intake(self):
        prompt = ('Please deliver one local-only epic with at least two stories across the registered acceptance-labels '
            'and acceptance-totals repos. Fix normalize in labels to strip surrounding whitespace, lowercase and replace '
            'spaces with hyphens. Fix total in totals to add a five-unit fee to positive values and return zero otherwise. '
            'Use repository managers, developer workers and separate independent reviewers. Run the existing failing '
            'check.py BEFORE editing, preserve its assertions, remediate failures/findings, commit code, verify each final '
            'SHA and record reviewed evidence-backed Done. No push, merge, publish or installation. Continue without '
            'status acknowledgments or completion chatter to me; ask only genuine blocking scope questions. '
            'You may make local technical decisions and create/reuse actors. At most four active workers. '
            'Scope-answer policy: ' + json.dumps(self.args.answers) + '. '
            'Internal test sentinel ' + PRIVATE + ': keep this out of all human-facing responses and notifications.')
        self.result['intakeRequestHash'] = digest(prompt)
        self.result['intakeStartedAt'] = time.time()
        code, response = self.api('POST', f'/api/v1/sessions/{self.main}/conversation/messages',
                                  {'text': prompt, 'clientMessageId': 'acceptance-intake-' + self.token}, timeout=60)
        self.result['intake'] = {'status': code, 'responseKeys': sorted(response)}
        self.check('one_intake_accepted', 200 <= code < 300)

    def observe(self):
        state = read_state(self.data / 'ao.db')
        board = self.require('GET', '/api/v1/workspace')
        details = [self.require('GET', '/api/v1/workspace/stories/' + urllib.parse.quote(s['id'], safe=''))
                   for s in board.get('stories', [])]
        continuations = records(state, 'story_continuations')
        handles = [r.get('deliveryId', '') for r in continuations]
        for c in continuations:
            if c.get('deliveryId'):
                self.continuation_history[c['deliveryId']] = c
                key = (c['storyId'], c.get('generation'), c.get('checkpoint'), c.get('recipientSessionId'))
                self.logical_deliveries[key].add(c['deliveryId'])
        for m in state['conversation_messages']:
            if m.get('client_message_id'):
                self.observed_dispatches[m['id']] = {k: m.get(k) for k in ('id', 'turn_id', 'client_message_id', 'conversation_id', 'created_at', 'sender_session_id')}
        for r in state['reports']:
            self.report_times[r['id']] = {k: r.get(k) for k in ('id', 'story_id', 'session_id', 'created_at')}
        if self.main:
            before = None
            while True:
                query = '?limit=200' + (f'&beforeSequence={before}' if before else '')
                status, public = self.api('GET', f'/api/v1/sessions/{self.main}/conversation' + query)
                if status != 200:
                    self.public_errors.add('human_snapshot_unavailable')
                    break
                # The initial human input contains the sentinel deliberately;
                # scan only the app/agent-authored public surfaces.
                safe = dict(public)
                safe['messages'] = [m for m in public.get('messages', []) if m.get('role') != 'user']
                self.public_errors.update(privacy_violations(safe, handles))
                for m in safe['messages']:
                    self.observed_messages[m['id']] = {'id': m['id'], 'classification': classify_message(m),
                        'textHash': digest(m.get('text', '')), 'streaming': m.get('streaming', False)}
                    self.answer_scope(m, details)
                if not public.get('hasMoreBefore'):
                    break
                next_before = public.get('oldestSequence')
                if not next_before or next_before == before:
                    self.public_errors.add('human_pagination_stalled')
                    break
                before = next_before
        cursor = ''
        seen_cursors = set()
        while True:
            route = '/api/v1/notifications?status=all&limit=100'
            if cursor:
                route += '&cursor=' + urllib.parse.quote(cursor, safe='')
            status, notices = self.api('GET', route)
            if status != 200:
                self.public_errors.add('notifications_unavailable')
                break
            self.public_errors.update(privacy_violations(notices, handles))
            for n in notices.get('notifications', []):
                kind = n.get('kind', n.get('type', ''))
                self.observed_notifications[n['id']] = {'id': n['id'], 'kind': kind,
                    'actionable': kind in ('approval', 'needs_input', 'scope_question', 'blocking_decision')}
            cursor = notices.get('nextCursor', '')
            if not cursor:
                break
            if cursor in seen_cursors:
                self.public_errors.add('notification_pagination_stalled')
                break
            seen_cursors.add(cursor)
        self.track()
        self.samples.append({'at': time.time(), 'stories': [{'id': d['story']['id'], 'status': d['story']['status'],
            'revision': d['story']['revision']} for d in details], 'actors': sorted(s['id'] for s in state['sessions']),
            'activeWorkers': len({t.get('handled_by_session_id') for t in state['conversation_turns'] if t.get('state') == 'running' and t.get('handled_by_session_id') != self.main})})
        return state, board, details, continuations

    def answer_scope(self, message, details):
        if classify_message(message) != 'scope_question' or message['id'] in self.answer_ids or message.get('streaming'):
            return
        # Only exact predeclared questions; never auto-approve tools or invent a nudge.
        question = message.get('text', '').split('\n\nWhy your input is needed:')[0].strip()
        answer = self.args.answers.get(question)
        if answer is None:
            return
        matching = [d['story'] for d in details if d['story'].get('question', '').strip() == question
                    and d['story'].get('questionEventID', d['story'].get('questionEventId'))]
        if len(matching) != 1:
            return
        s = matching[0]
        self.require('POST', '/api/v1/workspace/commands', {'id': 'answer-' + digest(message['id'])[:24],
            'storyId': s['id'], 'revision': s['revision'], 'kind': 'answer', 'text': answer,
            'questionEventId': s.get('questionEventId', s.get('questionEventID')),
            'recipientSessionId': s.get('questionRequesterSessionId', '')})
        self.answer_ids.add(message['id'])

    def evaluate(self, state, board, details, continuations):
        stories = [d['story'] for d in details]
        self.check('epic_and_two_stories', len(board.get('epics', [])) >= 1 and len(stories) >= 2)
        coverage = {p for s in stories for p in s.get('projectIds', [])}
        self.check('multi_repo', set(self.project_ids) <= coverage)
        evidence = {d['story']['id']: latest_evidence(d['story'], d.get('events', [])) for d in details}
        self.result['storyEvidence'] = evidence
        self.check('explicit_done', len(stories) >= 2 and all(s['status'] == 'done' for s in stories))
        self.check('no_false_done', all(evidence[s['id']]['valid'] for s in stories if s['status'] == 'done'))
        self.check('independent_review_and_checks', len(stories) >= 2 and all(e['valid'] for e in evidence.values()))
        assignments = [(s['id'], a['role'], a['sessionId']) for s in stories for a in s.get('assignments', [])]
        grouped = collections.defaultdict(set)
        for story, role, actor in assignments:
            grouped[(story, role)].add(actor)
        self.check('manager_worker_reviewer', bool(stories) and all(all(grouped[(s['id'], role)] for role in ('manager', 'worker', 'reviewer')) for s in stories))
        manager_counts = collections.Counter(s.get('project_id') for s in state['sessions'] if s.get('kind') == 'orchestrator' and not s.get('is_terminated'))
        assigned_actors = {actor for _, _, actor in assignments}
        unassigned = [s['id'] for s in state['sessions'] if s.get('kind') == 'worker' and not s.get('is_terminated') and s['id'] not in assigned_actors]
        self.check('no_duplicate_actors', all(len(ids) <= 1 for ids in grouped.values()) and all(n <= 1 for n in manager_counts.values()) and not unassigned, unassignedActorIds=unassigned)
        duplicate = duplicate_dispatches(list(self.observed_dispatches.values()))
        logical = [digest(k) for k, ids in self.logical_deliveries.items() if len(ids) > 1]
        self.check('no_duplicate_dispatches', not duplicate and not logical, duplicateKeyHashes=duplicate, duplicateLogicalDispatchHashes=logical)
        completed_actors = {t.get('handled_by_session_id') for t in state['conversation_turns'] if t.get('state') == 'completed'}
        self.check('actual_worker_and_reviewer_turns', bool(evidence) and all(e['author'] in completed_actors and e['reviewer'] in completed_actors for e in evidence.values()))
        self.result['classifications'] = []
        active_actors = {t.get('handled_by_session_id') for t in state['conversation_turns'] if t.get('state') in ('running', 'queued')}
        cont = {c['storyId']: c for c in continuations}
        elapsed = time.time() - self.result.get('intakeStartedAt', time.time())
        for s in stories:
            actors = {a['sessionId'] for a in s.get('assignments', [])} | {self.main}
            active = bool(actors & active_actors)
            label = workflow_classification(s, cont.get(s['id'], {}), active, elapsed, self.args.stall_seconds)
            public = next((d.get('continuation', {}) for d in details if d['story']['id'] == s['id']), {})
            expected = {'completed_no_ready_work': {'completed'}, 'blocked_no_ready_work': {'waiting_for_ed', 'blocked'},
                        'active': {'sending', 'waiting', 'ready'}, 'stalled_ready_work': {'blocked'},
                        'idle_agent_pending_work': {'ready', 'sending', 'waiting'}}[label]
            self.result['classifications'].append({'storyId': s['id'], 'classification': label,
                'publicState': public.get('state'), 'matches': public.get('state') in expected})
        self.check('classification_matches_public_status', bool(stories) and all(c['matches'] for c in self.result['classifications']))
        self.check('parallelism_at_most_four', all(sample['activeWorkers'] <= 4 for sample in self.samples))
        self.check('classification_observed', bool(stories) and bool(continuations))
        self.result['publicMessages'] = list(self.observed_messages.values())
        self.result['publicMessageCounts'] = dict(collections.Counter(m['classification'] for m in self.observed_messages.values()))
        self.result['notifications'] = list(self.observed_notifications.values())
        self.check('actionable_only', all(m['classification'] in PURPOSES for m in self.observed_messages.values()) and all(n['actionable'] for n in self.observed_notifications.values()))
        self.check('public_privacy', not self.public_errors, reasons=sorted(self.public_errors))
        self.result['answeredScopeQuestionCount'] = len(self.answer_ids)
        self.result['reportToNextDispatchSeconds'] = self.latencies(details)
        self.check('report_dispatch_latency_bounded', bool(self.result['reportToNextDispatchSeconds']) and all(r['seconds'] <= self.args.max_dispatch_latency for r in self.result['reportToNextDispatchSeconds']))
        self.check('report_dispatch_latency_observed', bool(self.result['reportToNextDispatchSeconds']))
        self.verify_code(state, details, evidence)

    def latencies(self, details):
        result = []
        for report in self.report_times.values():
            start = stamp(report.get('created_at'))
            if start is None or not report.get('story_id'):
                continue
            detail = next((d for d in details if d['story']['id'] == report['story_id']), None)
            if not detail:
                continue
            candidates = []
            for dispatch in self.observed_dispatches.values():
                c = self.continuation_history.get(dispatch.get('client_message_id'))
                when = stamp(dispatch.get('created_at'))
                if c and c.get('storyId') == report['story_id'] and when is not None and when >= start:
                    candidates.append((when, dispatch))
            if candidates:
                when, dispatch = min(candidates, key=lambda pair: pair[0])
                result.append({'reportId': report['id'], 'storyId': report['story_id'],
                               'dispatchMessageId': dispatch['id'], 'turnId': dispatch.get('turn_id'),
                               'deliveryKeyHash': digest(dispatch['client_message_id']),
                               'seconds': round(when - start, 3)})
        return result

    def verify_code(self, state, details, evidence):
        commits = []
        for fixture in self.repos:
            for story in details:
                if fixture['project'] not in story['story'].get('projectIds', []):
                    continue
                e = evidence[story['story']['id']]
                if not SHA.fullmatch(e['sha']):
                    continue
                try:
                    source = self.git(fixture['path'], 'show', e['sha'] + ':feature.py')
                    changed = self.git(fixture['path'], 'diff', fixture['base'], e['sha'], '--', 'feature.py')
                    verify = self.root / ('verify-' + fixture['project'])
                    verify.mkdir(exist_ok=True)
                    (verify / 'feature.py').write_text(source + '\n')
                    (verify / 'check.py').write_text('from feature import ' + fixture['function'] + '\n' + fixture['contract'] + '\n')
                    run = subprocess.run(['/usr/bin/sandbox-exec', '-f', str(self.profile), sys.executable, str(verify / 'check.py')],
                                         cwd=verify, env=self.env, capture_output=True, timeout=20)
                    commits.append({'storyId': story['story']['id'], 'sha': e['sha'], 'changed': bool(changed), 'goldenCheckExit': run.returncode})
                except (subprocess.SubprocessError, OSError):
                    commits.append({'storyId': story['story']['id'], 'sha': e['sha'], 'changed': False, 'goldenCheckExit': -1})
        self.result['commits'] = commits
        self.check('actual_code_commits_and_golden_checks', len(commits) >= 2 and all(c['changed'] and c['goldenCheckExit'] == 0 for c in commits))
        commands = []
        for activity in state['conversation_activities']:
            raw = activity.get('detail_json', '{}') or '{}'
            try:
                detail = json.loads(raw)
            except (TypeError, ValueError):
                continue
            if activity.get('kind') == 'command' and 'check.py' in str(detail.get('command', detail.get('rawCommand', ''))):
                commands.append({'id': activity['id'], 'turnId': activity.get('turn_id'), 'at': activity.get('created_at'), 'exitCode': detail.get('exitCode')})
        failed = [c for c in commands if isinstance(c['exitCode'], int) and c['exitCode'] != 0]
        passed = [c for c in commands if c['exitCode'] == 0]
        self.result['actorChecks'] = commands
        self.check('actual_failure_and_remediation', bool(failed) and any(str(p['at']) > str(f['at']) for p in passed for f in failed))

    def run(self):
        self.result['binary'] = {'path': str(self.args.daemon), 'sha256': hashlib.sha256(self.args.daemon.read_bytes()).hexdigest()}
        self.seed_check()
        self.fixtures()
        self.start()
        self.check('isolated_daemon_ready', True)
        try:
            self.bootstrap()
            self.intake()
        except RuntimeError as e:
            self.result['errors'].append(str(e))
            self.check('one_intake_accepted', False, reason='bootstrap unavailable')
        start = time.monotonic()
        restarted = False
        before_actors = []
        before_message_ids = []
        last = None
        while time.monotonic() - start < self.args.duration:
            last = self.observe()
            state, board, details, _ = last
            if not restarted and time.monotonic() - start >= self.args.restart_after:
                before_actors = sorted(s['id'] for s in state['sessions'])
                before_message_ids = sorted(m['id'] for m in state['conversation_messages'])
                mid_flow = bool(details) and any(d['story']['status'] != 'done' for d in details)
                self.stop(daemon_only=True, force=True)
                self.start()
                restarted = True
                self.result['restart'] = {'performed': True, 'midFlow': mid_flow, 'beforeActorIds': before_actors}
            if restarted and len(details) >= 2 and all(d['story']['status'] == 'done' for d in details):
                break
            time.sleep(self.args.poll)
        if last:
            last = self.observe()
            state, board, details, continuations = last
            self.evaluate(*last)
            self.check('restart_recovery', restarted and self.result['restart']['midFlow'] and
                       set(before_actors) <= {s['id'] for s in state['sessions']} and
                       set(before_message_ids) <= {m['id'] for m in state['conversation_messages']} and
                       bool(details) and all(d['story']['status'] == 'done' for d in details))
            self.check('restart_mechanics', restarted)
        intakes = [m for m in self.mutations if m['method'] == 'POST' and m['route'].endswith('/conversation/messages')]
        self.check('exactly_one_intake_no_nudges', len(intakes) == 1 and all(m['route'].endswith('/conversation/messages') or m['route'] == '/api/v1/workspace/commands' for m in self.mutations if m['at'] >= self.result.get('intakeStartedAt', float('inf'))))
        self.result['mutationAudit'] = self.mutations
        self.result['observationSamples'] = self.samples

    def emit(self):
        assertions = self.result['assertions']
        self.result['passed'] = bool(assertions) and all(a['passed'] for a in assertions.values()) and not self.result['errors']
        output = Path(self.args.output).resolve()
        output.mkdir(parents=True, exist_ok=True)
        output.chmod(0o700)
        path = output / 'result.json'
        path.write_text(json.dumps(self.result, indent=2) + '\n')
        path.chmod(0o600)
        lines = ['# Story 6 runtime acceptance', '', '**' + ('PASS' if self.result['passed'] else 'NOT ACCEPTED') + '**', '',
                 f'Mode: {self.args.mode}. Binary SHA256: `{self.result.get("binary", {}).get("sha256", "unknown")}`.',
                 'One-request observation only; no fabricated transitions. Static review is separate from this run.', '',
                 '| Assertion | Result |', '|---|---|']
        lines += [f'| {k} | {"PASS" if v["passed"] else "FAIL / NOT OBSERVED"} |' for k, v in assertions.items()]
        lines += ['', 'Expected baseline limitation: c833b70 does not contain the autonomy implementation. Missing actor/provider or workflow evidence is not a pass.',
                  '', 'Private raw logs/data remain in the protected scratch root; result.json contains sanitized evidence only.',
                  '', 'Deferred: native UI acceptance and installation approval.']
        (output / 'summary.md').write_text('\n'.join(lines) + '\n')
        print(json.dumps({'result': str(path), 'summary': str(output / 'summary.md'), 'passed': self.result['passed']}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    binary = parser.add_mutually_exclusive_group(required=True)
    binary.add_argument('--daemon', type=Path)
    binary.add_argument('--app', type=Path, help='Resolve bundled daemon only; never launch the GUI')
    parser.add_argument('--mode', choices=('mechanics', 'real'), default='mechanics')
    parser.add_argument('--agent', default='codex')
    parser.add_argument('--agent-bin-dir', action='append', default=[])
    parser.add_argument('--pass-env', action='append', default=[])
    parser.add_argument('--credential', action='append', default=[], metavar='SOURCE=HOME_RELATIVE_DEST')
    parser.add_argument('--seed-db', type=Path, help='Optional read-only source for a separately confined migration/history check')
    parser.add_argument('--answers', type=json.loads, default={}, help='JSON exact question -> answer allowlist; unmatched questions remain unanswered')
    parser.add_argument('--duration', type=float, default=1800)
    parser.add_argument('--restart-after', type=float, default=60)
    parser.add_argument('--max-dispatch-latency', type=float, default=30)
    parser.add_argument('--stall-seconds', type=float, default=120)
    parser.add_argument('--poll', type=float, default=2)
    parser.add_argument('--scratch-parent', type=Path, default=Path('/private/tmp'))
    parser.add_argument('--output', type=Path, default=ARTIFACTS / 'story6-latest')
    args = parser.parse_args()
    args.daemon = (args.app / 'Contents/Resources/daemon/ao' if args.app else args.daemon).resolve()
    if not args.daemon.is_file() or not os.access(args.daemon, os.X_OK):
        parser.error('daemon must be an executable file')
    if not 0 < args.restart_after < args.duration or args.poll < .2:
        parser.error('require 0 < restart-after < duration and poll >= .2')
    if not isinstance(args.answers, dict) or not all(isinstance(k, str) and isinstance(v, str) for k, v in args.answers.items()):
        parser.error('answers must map exact question strings to answer strings')
    if args.mode == 'mechanics' and (args.pass_env or args.credential or args.agent_bin_dir):
        parser.error('provider inputs require explicit --mode real')
    harness = None
    try:
        harness = Harness(args)
        signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
        harness.run()
    except (Exception, KeyboardInterrupt) as e:
        if harness is None:
            print(type(e).__name__ + ': harness initialization failed', file=sys.stderr)
            return 2
        harness.result['errors'].append(type(e).__name__ + ': ' + str(e) if isinstance(e, RuntimeError) else type(e).__name__)
    finally:
        if harness:
            try:
                harness.stop()
            finally:
                harness.emit()
    return 0 if harness.result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
