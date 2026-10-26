#!/usr/bin/env python3
"""Real-provider acceptance of coordinator visibility on a COPY of Ed's database.

Reuses the reviewed Story 6 confinement (scratch root, sandbox-exec, owned-process
tracking, credential purge). Reads the live database only through SQLite's backup API
(mode=ro). Never writes outside the scratch root. Stores ids/counts/hashes, never text.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import sys
import time
import urllib.parse
import urllib.request

import acceptance as a

LIVE_DB = Path.home() / '.ao/data/ao.db'
COORD = 'overall-coordinator-1'
STANDING = 'ao-standing-coordinator-'
PURPOSES_ASK = {'scope_question', 'blocking_decision', 'completion_decision'}


def sql(path, query, *params):
    with a.db_open(path) as db:
        return db.execute(query, params).fetchall()


class CoordinatorVisibility(a.Harness):
    def snapshot(self, audience_all=False, limit=200, before=None):
        route = f'/api/v1/sessions/{COORD}/conversation?limit={limit}' + ('&audience=all' if audience_all else '') + (f'&beforeSequence={before}' if before else '')
        status, body = self.api('GET', route, timeout=30)
        return body if status == 200 else {'messages': [], 'activities': [], 'turns': [], 'errorCode': body.get('errorCode')}

    def all_pages(self, audience_all=False, max_pages=12):
        """Every page of one view: messages by id, plus activities and the newest page's envelope."""
        messages, activities, before, first = {}, [], None, None
        for _ in range(max_pages):
            snap = self.snapshot(audience_all, before=before)
            first = first or snap
            for m in snap.get('messages', []):
                messages[m['id']] = m
            activities += snap.get('activities', [])
            nxt = snap.get('oldestSequence')
            if not snap.get('hasMoreBefore') or not nxt or nxt == before:
                break
            before = nxt
        return messages, activities, first

    def conversation_id(self):
        row = sql(self.data / 'ao.db', "select id from conversations where current_session_id=? or session_id=? order by latest_sequence desc limit 1", COORD, COORD)
        return row[0][0]

    def prepare_db(self):
        target = self.data / 'ao.db'
        with a.db_open(LIVE_DB) as src, sqlite3.connect(target) as dest:
            src.backup(dest)
        target.chmod(0o600)
        coord_ws = self.data / 'worktrees/overall-coordinator/orchestrators/overall-coordinator'  # inside the managed root, as in the live layout
        coord_project = self.root / 'coordination'
        coord_project.mkdir(parents=True)
        coord_ws.parent.mkdir(parents=True)
        subprocess.run(['/usr/bin/git', 'init', '-q', '-b', 'main', str(coord_project)], check=True)
        conn = sqlite3.connect(target)
        before = conn.execute("select model, reasoning_effort from conversations where current_session_id=? or session_id=?", (COORD, COORD)).fetchall()
        # Only the coordinator is made runnable in scratch; paths move into the scratch root.
        conn.execute("update projects set path=? where id='overall-coordinator'", (str(coord_project),))
        conn.execute("update sessions set workspace_path=?, workspace_repo_path='' where id=?", (str(coord_ws), COORD))
        conn.execute("update sessions set artifact_dir=? || '/' || id where artifact_dir<>''", (str(self.root / 'artifacts'),))
        # Scratch cannot reach Ed's Claude credentials (and they are not extracted), so the copied
        # coordinator session is switched to the Codex provider authorized for scratch runs. The
        # session id, conversation and history are unchanged; the visibility rules are
        # provider-independent (standing instruction is plain message text).
        if not self.args.keep_harness:
            conn.execute("update sessions set harness='codex', model='', effort='', agent_session_id='', provider_conversation_id='' where id=?", (COORD,))
            conn.execute("update conversations set model=NULL, reasoning_effort=NULL, approval_mode='auto', account_json=NULL, thread_state_json=NULL, opencode_mode='' where current_session_id=? or session_id=?", (COORD, COORD))
        # The copied project-level conversation follows the newest coordinator session. Its branches still
        # name the old Claude thread; clear those provider thread ids (history is untouched) so the new
        # Codex session's handle is not rejected as a mismatch on daemon restart.
        conn.execute("update conversation_branches set provider_conversation_id='' where conversation_id in (select id from conversations where current_session_id=? or session_id=?)", (COORD, COORD))
        # No live host exists in scratch, so the copied coordinator is marked terminated and brought
        # back through the daemon's own `restore` (same session id and conversation, no new session).
        conn.execute("update sessions set is_terminated=1 where id=?", (COORD,))
        # Stale per-session account bindings point at credential stores that exist only in the live HOME.
        conn.execute('delete from session_agent_accounts')
        project_config = json.loads(conn.execute("select config from projects where id='overall-coordinator'").fetchone()[0] or '{}')
        if not self.args.keep_harness:
            project_config['orchestrator'] = {'agent': 'codex', 'agentConfig': {}}
        conn.execute("update projects set config=? where id='overall-coordinator'", (json.dumps(project_config),))
        conn.commit(); conn.close()
        self.result['dbCopy'] = {'source': 'live ao.db via sqlite backup API (mode=ro)', 'sha256': hashlib.sha256(target.read_bytes()).hexdigest(),
                                 'bytes': target.stat().st_size, 'coordinatorProviderOverride': {'from': 'claude-code', 'to': 'codex', 'reason': 'scratch HOME cannot reach the Claude credentials; they are not extracted'},
                                 'originalCoordinatorConversationSettings': [list(r) for r in before]}

    def launch_gui(self):
        self.cdp_port = self.free_port()
        env = dict(self.env)
        env.update(AO_KEEP_DAEMON='1', AO_DISABLE_GPU='1', MallocNanoZone='0', ELECTRON_ENABLE_LOGGING='1')
        env['AO_GUI_LOG'] = str(self.root / 'gui.log')
        env['CV_HOOK_LOG'] = str(self.root / 'main-hook.log')
        env['CV_HOOK_CMD'] = str(self.root / 'hook-cmd.json')
        env['NODE_OPTIONS'] = '--require=' + str(Path(__file__).with_name('cv_main_hook.js').resolve())
        # Detached through a shell so launchd (not this harness) reaps the GUI: a killed direct child would linger as a
        # zombie and be miscounted as a surviving process by the teardown check.
        shell = subprocess.Popen(['/bin/sh', '-c', '"$0" "$@" >> "$AO_GUI_LOG" 2>&1 &', str(self.app_copy / 'Contents/MacOS/agent-orchestrator'),
                                  f'--remote-debugging-port={self.cdp_port}', '--use-mock-keychain'], env=env, cwd=self.root, start_new_session=True)
        shell.wait(timeout=30)
        self.track()
        def up():
            try:
                with urllib.request.urlopen(f'http://127.0.0.1:{self.cdp_port}/json/version', timeout=2) as r:
                    return r.status == 200
            except OSError:
                return False
        ok = self.wait(up, 90, step=1.0)
        time.sleep(6)
        self.result['guiLaunch'] = {'cdpPort': self.cdp_port, 'reachable': bool(ok), 'processStillRunning': any('MacOS/agent-orchestrator' in c for _, (_, _, c) in self.process_rows().items() if str(self.root) in c)}
        return bool(ok)

    def hook_events(self):
        path = self.root / 'main-hook.log'
        out = []
        if path.exists():
            for line in path.read_text(errors='replace').splitlines():
                try:
                    out.append(json.loads(line))
                except ValueError:
                    pass
        return out

    def window(self, cmd):
        ident = f'{cmd}-{time.time()}'
        (self.root / 'hook-cmd.json').write_text(json.dumps({'id': ident, 'cmd': cmd}))
        done = self.wait(lambda: next((e for e in self.hook_events() if e['kind'] == 'window-cmd' and e.get('id') == ident), None), 15, step=0.5)
        return done or {'error': 'no confirmation from main-process hook'}

    def refocus(self):
        # Focus cancels a pending dock bounce; minimise again so the next alert finds the app "not looking".
        f = self.window('focus'); time.sleep(2)
        m = self.window('minimize'); time.sleep(1.5)
        self.window_states = {'afterFocus': f, 'afterMinimize': m}

    def alerts_since(self):
        events = self.hook_events()
        fresh = events[self.hook_mark:]
        self.hook_mark = len(events)
        total = self.ui('sound-count').get('soundInvocations', 0)
        sound = total - self.sound_mark
        self.sound_mark = total
        return {'dockBounceTypes': [e.get('type') for e in fresh if e['kind'] == 'dock-bounce'], 'osToastTitles': [e.get('title') for e in fresh if e['kind'] == 'notification-show'], 'soundInvocations': sound}

    def decision(self, key, purpose, text):
        return self.api('POST', f'/api/v1/sessions/{COORD}/conversation/human-decisions', {'id': key, 'purpose': purpose, 'question': text}, timeout=60)[0]

    def ui(self, action, **kw):
        node = '/Users/edstockman/.local/share/fnm/node-versions/v24.14.0/installation/bin/node'
        run = subprocess.run([node, str(Path(__file__).with_name('coordinator_visibility_ui.cjs')), str(self.args.playwright_dir), str(self.cdp_port), action, json.dumps(kw)],
                             capture_output=True, text=True, timeout=400, env={'PATH': '/usr/bin:/bin'})
        try:
            return json.loads(run.stdout.strip().splitlines()[-1])
        except (ValueError, IndexError):
            return {'ok': False, 'error': 'no json from ui driver'}

    def visible_texts(self, limit=6, role=None, turn=None, minlen=60):
        default, _ = self.visible_ids()
        ids = list(default)
        out = []
        for mid, text, r, t in sql(self.data / 'ao.db', 'select id, text, role, turn_id from conversation_messages where id in (%s) order by sequence desc' % ','.join('?' * len(ids)), *ids) if ids else []:
            if len((text or '').strip()) > minlen and (role is None or r == role) and (turn is None or t == turn):
                out.append(text)
            if len(out) >= limit:
                break
        return out

    hook_mark = 0
    sound_mark = 0

    def use_binary(self, path):
        self.args.daemon = path
        link = self.root / 'bin/ao'
        if link.is_symlink() or link.exists():
            link.unlink()
        link.symlink_to(path)

    def host_identity(self):
        rows = self.process_rows()
        return {pid: ident for pid, (_, ident, command) in rows.items() if 'chat-host ' + COORD in command and str(self.root) in command}

    def wait(self, predicate, seconds, step=2.0):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            value = predicate()
            if value:
                return value
            time.sleep(step)
        return None

    def turn_state(self, turn_id):
        rows = sql(self.data / 'ao.db', 'select state, started_at, completed_at from conversation_turns where id=?', turn_id)
        return rows[0] if rows else None

    def wait_turn(self, turn_id, seconds):
        return self.wait(lambda: (lambda t: t if t and t[0] in ('completed', 'failed', 'interrupted', 'cancelled') else None)(self.turn_state(turn_id)), seconds)

    def notifications(self):
        status, body = self.api('GET', '/api/v1/notifications?status=all&limit=100', timeout=30)
        return body.get('notifications', []) if status == 200 else []

    def visible_ids(self, audience_all=False):
        messages, activities, first = self.all_pages(audience_all)
        return messages, dict(first or {}, activities=activities)

    def send_human(self, text, key):
        status, body = self.api('POST', f'/api/v1/sessions/{COORD}/conversation/messages', {'text': text, 'clientMessageId': key}, timeout=60)
        return status, body

    def send_automation(self, text, sender):
        status, body = self.api('POST', f'/api/v1/sessions/{COORD}/send', {'message': text, 'senderSessionId': sender, 'userAuthored': False}, timeout=60)
        return status, body

    def turn_of_message(self, key):
        rows = sql(self.data / 'ao.db', 'select id, turn_id from conversation_messages where client_message_id=?', key)
        return rows[0] if rows else None

    def newest_turn_after(self, since_iso):
        rows = sql(self.data / 'ao.db', "select t.id from conversation_turns t join conversations c on c.id=t.conversation_id where (c.current_session_id=? or c.session_id=?) and t.started_at > ? order by t.started_at limit 1", COORD, COORD, since_iso)
        return rows[0][0] if rows else None

    def run_cv(self):
        r = self.result
        r['controlledConditions'] = [
            'scratch HOME/AO data under a 0700 scratch root; daemon confined with sandbox-exec (writes only to scratch, live ~/.ao unreadable)',
            'database is a fresh read-only SQLite backup of the live DB; the copied Claude coordinator is marked terminated and a new Codex coordinator is created by the previous (66b) build',
            'coordinator provider is Codex via a read-only auth link (Claude credentials are not reachable from scratch and are not extracted)',
            'packaged GUI (when --app) is launched unconfined with --use-mock-keychain so it never touches the real keychain; scratch only',
            'needs-Ed and routine-report stimuli are synthetic automation messages sent through the daemon send path with a worker sender, not a real worker report',
            'persistent host survival across the daemon swap is reproduced by stopping only the daemon; adoption of the live Claude host is not reproducible here'
        ]
        r.update(scenario='coordinator-visibility', kind='targeted_coordinator_visibility_backend_not_acceptance', autonomyAccepted=False)
        r['harnessSha'] = subprocess.check_output(['/usr/bin/git', '-C', str(Path(__file__).resolve().parents[2]), 'rev-parse', 'HEAD'], text=True).strip()
        r['harnessSourceSha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        if self.args.app:
            self.app_copy = self.root / 'app/Agent Orchestrator.app'
            self.app_copy.parent.mkdir(parents=True)
            subprocess.run(['/usr/bin/ditto', str(self.args.app), str(self.app_copy)], check=True)
            seal = subprocess.run(['/usr/bin/codesign', '--verify', '--deep', '--strict', str(self.app_copy)], capture_output=True)
            self.check('packaged_app_copy_seal_valid', seal.returncode == 0)
            self.args.daemon = self.app_copy / 'Contents/Resources/daemon/ao'
            self.env['AO_ACP_RUNTIME_DIR'] = str(self.app_copy / 'Contents/Resources/acp-runtime')
            self.env['AO_APP_RUN_ID'] = 'cv-' + self.token
        data = self.args.daemon.read_bytes()
        r['binary'] = {'path': str(self.args.daemon), 'sha256': hashlib.sha256(data).hexdigest()}
        meta = subprocess.check_output(['go', 'version', '-m', str(self.args.daemon)], text=True, cwd=self.root)
        for key in ('vcs.revision', 'vcs.modified'):
            m = re.search(r'\b' + re.escape(key) + r'=(\S+)', meta)
            r['binary'][key] = m.group(1) if m else None
        self.check('binary_hash_and_clean_revision', r['binary']['sha256'] == self.args.expect_sha256 and r['binary']['vcs.revision'] == self.args.expect_revision and r['binary']['vcs.modified'] == 'false',
                   expectedSha256=self.args.expect_sha256, expectedRevision=self.args.expect_revision)
        self.prepare_db()
        candidate = self.args.daemon
        # Phase 1: the PREVIOUS build (66b827b9d, old coordinator prompt, no standing instruction) creates
        # and exercises a Codex coordinator in the scratch copy, as the live install did before upgrade.
        prev = self.args.previous_daemon.resolve()
        r['previousBinary'] = {'path': str(prev), 'sha256': hashlib.sha256(prev.read_bytes()).hexdigest()}
        pmeta = subprocess.check_output(['go', 'version', '-m', str(prev)], text=True, cwd=self.root)
        for key in ('vcs.revision', 'vcs.modified'):
            m = re.search(r'\b' + re.escape(key) + r'=(\S+)', pmeta)
            r['previousBinary'][key] = m.group(1) if m else None
        self.check('previous_binary_hash_and_clean_revision', r['previousBinary']['sha256'] == self.args.expect_previous_sha256 and r['previousBinary']['vcs.modified'] == 'false')
        self.use_binary(prev)
        candidate_acp = self.env.get('AO_ACP_RUNTIME_DIR')
        if self.args.previous_acp_runtime_dir:
            self.env['AO_ACP_RUNTIME_DIR'] = str(self.args.previous_acp_runtime_dir.resolve())  # the previous build's own runtime
        self.start()
        self.check('isolated_daemon_ready', True)
        applied_prev = sql(self.data / 'ao.db', 'select count(*), max(version_id) from goose_db_version')[0]
        if self.args.gui_probe:
            self.launch_gui()
            if self.args.hold:
                (self.root / 'hold').write_text(json.dumps({'root': str(self.root), 'port': self.port, 'cdp': self.cdp_port}))
                while not (self.root / 'release').exists():
                    time.sleep(2)
            self.result['guiProbe'] = {'home': self.ui('open-main-from-home', sessionId='none'), 'sidebar': self.ui('open-main-from-sidebar', sessionId='none')}
            return
        if self.args.hold:
            (self.root / 'hold').write_text(json.dumps({'root': str(self.root), 'port': self.port}))
            while not (self.root / 'release').exists():
                time.sleep(2)
            return
        global COORD
        attempts = []
        for _ in range(8):
            status, body = self.api('POST', '/api/v1/sessions', {'projectId': 'overall-coordinator', 'kind': 'orchestrator', 'harness': 'codex', 'mode': 'chat'}, timeout=180)
            attempts.append({'status': status, 'errorCode': body.get('errorCode')})
            # The codex driver compatibility probe can still be warming up right after daemon start on a loaded machine.
            if status < 400 or body.get('errorCode') != 'CHAT_DRIVER_INCOMPATIBLE':
                break
            time.sleep(10)
        r['previousBuildCreateAttempts'] = attempts
        self.check('previous_build_created_coordinator', status < 400 and 'session' in body, status=status, errorCode=body.get('errorCode'), attempts=len(attempts))
        if status >= 400 or 'session' not in body:
            raise RuntimeError('previous build could not create the scratch coordinator')
        COORD = body['session']['id']
        r['coordinatorSessionId'] = COORD
        pre_key = 'cv-pre-' + self.token
        pre_status, _ = self.send_human('Reply with one short sentence acknowledging this test message. Take no other action.', pre_key)
        pm = self.wait(lambda: self.turn_of_message(pre_key), 60)
        pre = {'sendStatus': pre_status, 'accepted': bool(pm)}
        if pm:
            end = self.wait_turn(pm[1], self.args.turn_wait)
            default_prev, _ = self.visible_ids()
            replies = [row[0] for row in sql(self.data / 'ao.db', "select id from conversation_messages where turn_id=? and role='assistant' and length(trim(text))>0", pm[1])]
            pre.update(turnId=pm[1], turnState=end[0] if end else 'timeout', assistantMessages=len(replies), assistantVisibleUnderPreviousBuild=sum(1 for i in replies if i in default_prev), humanMessageId=pm[0])
        r['phase1_previousBuildPreUpgradeTurn'] = pre
        self.check('phase1_previous_build_coordinator_turn_completed', pre.get('turnState') == 'completed' and pre.get('assistantMessages', 0) >= 1)
        self.host_before = self.host_identity()
        before_sessions = sql(self.data / 'ao.db', 'select id, num, created_at, kind, harness, project_id, is_terminated from sessions order by id')
        coord_before = [row for row in before_sessions if row[0] == COORD][0]
        cid = self.conversation_id()
        r['phase1_hostPidsBeforeUpgrade'] = sorted(self.host_before)
        # Phase 2: stop ONLY the daemon (persistent hosts survive by design) and start the candidate on the same data.
        self.stop(daemon_only=True)
        survivors = self.host_identity()
        r['phase2_hostSurvivedDaemonStop'] = {'hostPids': sorted(survivors), 'sameAsBefore': survivors == self.host_before}
        self.use_binary(candidate)
        if candidate_acp:
            self.env['AO_ACP_RUNTIME_DIR'] = candidate_acp
        self.start()
        applied_cand = sql(self.data / 'ao.db', 'select count(*), max(version_id) from goose_db_version')[0]
        r['phase2_upgradeFromPreviousBuild'] = {'migrationRowsBefore': applied_prev[0], 'migrationRowsAfter': applied_cand[0], 'maxVersionBefore': applied_prev[1], 'maxVersionAfter': applied_cand[1],
                                                 'integrity': sql(self.data / 'ao.db', 'pragma integrity_check')[0][0]}
        self.check('phase2_forward_migration_applied_on_66b_migrated_db', applied_cand[1] > applied_prev[1] and r['phase2_upgradeFromPreviousBuild']['integrity'] == 'ok')
        self.check('phase2_host_survived_daemon_swap', bool(survivors) and survivors == self.host_before)
        default0, _ = self.visible_ids()
        r['before'] = {'visibleMessages': len(default0), 'storedMessages': sql(self.data / 'ao.db', 'select count(*) from conversation_messages where conversation_id=?', cid)[0][0],
                       'preUpgradeReplyVisibleAfterUpgrade': pre.get('turnState') == 'completed' and any(row[0] in default0 for row in sql(self.data / 'ao.db', "select id from conversation_messages where turn_id=? and role='assistant'", pre['turnId']))}

        # (d) the existing coordinator receives the standing instruction in place.
        def standing():
            rows = sql(self.data / 'ao.db', "select id, turn_id, origin, client_message_id, created_at from conversation_messages where conversation_id=? and client_message_id like ?", cid, STANDING + '%')
            return rows[0] if rows else None
        found = self.wait(standing, self.args.standing_wait)
        d = {'delivered': bool(found)}
        if found:
            msg_id, turn_id, origin, client_key, created = found
            end = self.wait_turn(turn_id, self.args.turn_wait)
            hidden_default = msg_id not in self.visible_ids()[0]
            all_view, _ = self.visible_ids(True)
            replies = sql(self.data / 'ao.db', "select id from conversation_messages where turn_id=? and role='assistant'", turn_id)
            default_now, _ = self.visible_ids()
            host = self.host_identity()
            d.update(messageId=msg_id, turnId=turn_id, origin=origin, clientKeyRevision=client_key[len(STANDING):], turnState=end[0] if end else 'timeout',
                     hiddenInDefaultView=hidden_default, taggedInternalInAllView=all_view.get(msg_id, {}).get('audience') == 'internal',
                     assistantRepliesInTurn=len(replies), assistantRepliesHiddenInDefault=all(rid[0] not in default_now for rid in replies),
                     hostPids=sorted(host))
        r['d_existingCoordinatorStandingInstruction'] = d
        self.check('d_standing_instruction_delivered_hidden', d['delivered'] and d.get('turnState') == 'completed' and d.get('hiddenInDefaultView') and d.get('taggedInternalInAllView') and d.get('assistantRepliesHiddenInDefault'))

        gui = {}
        if self.args.app:
            gui_up = self.launch_gui()
            self.check('packaged_gui_attached_to_scratch_daemon', gui_up)
            home = self.ui('open-main-from-home', sessionId=COORD)
            side = self.ui('open-main-from-sidebar', sessionId=COORD, coordinatorProjectName='overall-coordinator')
            view = self.ui('default-view', snippets=self.visible_texts(8))
            gui = {'f_openMainFromHome': home, 'f_mainCoordinatorSidebarRow': side, 'defaultView': view}
            r['f_coordinatorProjectFindableInUi'] = {'homeOpenMain': home, 'sidebarMainCoordinatorRow': side}
            r['ui_defaultView'] = view
            self.check('f_coordinator_findable_via_home_open_main_and_sidebar_row', home.get('reachedCoordinatorSession') and side.get('sidebarRowPresent') and side.get('reachedCoordinatorSession'))
            inst = self.ui('instrument')
            self.refocus()
            self.hook_mark = len(self.hook_events())
            self.sound_mark = 0
            r['alertInstrumentation'] = {'rendererSoundRecorder': inst, 'hookEventsAtStart': self.hook_mark, 'windowStates': getattr(self, 'window_states', None), 'note': 'window minimised via the main-process hook so the app is not looking; sound recorded as invoked (not played)'}
            self.check('ui_default_view_shows_human_content_and_hides_internal', view.get('ok') and view.get('composerPresent') and view.get('internalBlocks') == 0 and view.get('visibleSnippetMatches', 0) >= 1)

        # (a) a human message gets a visible reply.
        a_key = 'cv-a-' + self.token
        a_status, _ = self.send_human('Reply with one short sentence acknowledging this test message. Take no other action.', a_key)
        m = self.wait(lambda: self.turn_of_message(a_key), 30)
        a_res = {'sendStatus': a_status, 'accepted': bool(m)}
        if m:
            end = self.wait_turn(m[1], self.args.turn_wait)
            first, _ = self.visible_ids(); second, _ = self.visible_ids()
            replies = [row[0] for row in sql(self.data / 'ao.db', "select id from conversation_messages where turn_id=? and role='assistant' and length(trim(text))>0", m[1])]
            vis = [i for i in replies if i in first]
            purposes = sorted({first[i].get('purpose') for i in vis})
            a_res.update(turnId=m[1], turnState=end[0] if end else 'timeout', assistantMessages=len(replies), visibleAssistantMessages=len(vis), purposes=purposes,
                         refetchConsistent=set(first) == set(second), humanMessageVisible=m[0] in first)
        r['a_humanMessageVisibleReply'] = a_res
        self.check('a_human_message_reply_visible', a_res.get('turnState') == 'completed' and a_res.get('visibleAssistantMessages', 0) >= 1 and a_res.get('refetchConsistent') and a_res.get('humanMessageVisible'))

        # (c) a routine report handled internally leaves no visible reply and no notification.
        n_before = [n for n in self.notifications() if n.get('type') == 'coordinator_message']
        vis_before, _ = self.visible_ids()
        c_mark = time.strftime('%Y-%m-%d %H:%M:%S', time.gmtime(time.time() - 2))
        c_text = ('Routine report from worker agent-orchestrator-4 (state: checkpoint, story routine-demo): tests ran green on the scratch branch; nothing is needed from Ed. '
                  'Record it internally and continue.')
        if self.args.app:
            self.refocus(); self.alerts_since()
        c_status, _ = self.send_automation(c_text, 'agent-orchestrator-4')
        c_turn = self.wait(lambda: self.newest_turn_after(c_mark), 40)
        c_res = {'sendStatus': c_status, 'turnStarted': bool(c_turn)}
        if c_turn:
            end = self.wait_turn(c_turn, self.args.turn_wait)
            vis_after, _ = self.visible_ids(); all_view, all_snap = self.visible_ids(True)
            new_all = [i for i in all_view if i not in vis_before]
            c_res.update(turnId=c_turn, turnState=end[0] if end else 'timeout', newVisibleMessages=len([i for i in vis_after if i not in vis_before]),
                         newStoredMessagesInAllView=len(new_all), newMessagesTaggedInternal=all(all_view[i].get('audience') == 'internal' for i in new_all),
                         newCoordinatorNotifications=len([n for n in self.notifications() if n.get('type') == 'coordinator_message']) - len(n_before))
        if self.args.app:
            time.sleep(3)
            c_res['alerts'] = self.alerts_since()
            self.check('c_routine_report_raises_no_toast_bounce_or_sound', not c_res['alerts']['dockBounceTypes'] and not c_res['alerts']['osToastTitles'] and c_res['alerts']['soundInvocations'] == 0)
        r['c_routineReportHidden'] = c_res
        self.check('c_routine_report_hidden_no_notification', c_res.get('turnState') == 'completed' and c_res.get('newVisibleMessages') == 0 and c_res.get('newStoredMessagesInAllView', 0) >= 1 and c_res.get('newMessagesTaggedInternal') and c_res.get('newCoordinatorNotifications') == 0)

        if self.args.app:
            self.refocus(); self.alerts_since()
            v_status = self.decision('cv-verified-' + self.token, 'verified_result', 'Verified: the scratch acceptance check passed on the candidate build.')
            time.sleep(6)
            v = self.alerts_since()
            r['alerts_verifiedResultInformational'] = dict(v, sendStatus=v_status, via='POST human-decisions (the endpoint `ao tell --kind result` uses)')
            self.check('electron_verified_result_raises_informational_bounce_and_os_toast', v_status < 400 and v['dockBounceTypes'] == ['informational'] and any('Verified result' in (t or '') for t in v['osToastTitles']))
            self.refocus(); self.alerts_since()

        # (b) a report that needs Ed produces a visible message AND a notification.
        n_before = [n['id'] for n in self.notifications() if n.get('type') == 'coordinator_message']
        vis_before, _ = self.visible_ids()
        b_mark = time.strftime('%Y-%m-%d %H:%M:%S', time.gmtime(time.time() - 2))
        b_text = ('Report from worker agent-orchestrator-4 (state: needs_input, story needs-ed-demo): the repository has two valid ways to rename the public setting. '
                  'Option A keeps a deprecated alias for one release; option B removes the old name immediately and breaks existing configs. The agents cannot choose between them without the product owner.')
        b_status, _ = self.send_automation(b_text, 'agent-orchestrator-4')
        b_turn = self.wait(lambda: self.newest_turn_after(b_mark), 40)
        b_res = {'sendStatus': b_status, 'turnStarted': bool(b_turn)}
        if b_turn:
            end = self.wait_turn(b_turn, self.args.turn_wait)
            vis_after, _ = self.visible_ids(); all_view, all_snap = self.visible_ids(True)
            new_visible = [i for i in vis_after if i not in vis_before]
            labels = sql(self.data / 'ao.db', 'select m.id, h.purpose from conversation_messages m join coordinator_human_messages h on h.message_id=m.id where m.turn_id=?', b_turn)
            notes = [n for n in self.notifications() if n.get('type') == 'coordinator_message' and n['id'] not in n_before]
            commands = []
            for act in all_snap.get('activities', []):
                if act.get('turnId') == b_turn:
                    text = json.dumps(act, default=str)
                    for verb in ('ao ask', 'ao tell'):
                        if verb in text:
                            commands.append(verb)
            b_res.update(turnId=b_turn, turnState=end[0] if end else 'timeout', newVisibleMessages=len(new_visible), visiblePurposes=sorted({vis_after[i].get('purpose') for i in new_visible}),
                         labelRows=[{'messageId': i, 'purpose': p} for i, p in labels], newNotifications=[{'id': n['id'], 'type': n['type'], 'purpose': n.get('purpose'), 'title': n.get('title'), 'sessionId': n.get('sessionId')} for n in notes],
                         markingMechanism=sorted(set(commands)) or ['unknown (no ao ask/tell command found in activities)'])
        r['b_reportNeedingEd'] = b_res
        self.check('b_report_needing_ed_visible_with_notification', b_res.get('turnState') == 'completed' and b_res.get('newVisibleMessages', 0) >= 1 and
                   any(p in PURPOSES_ASK | {'stopped_work', 'verified_result'} for p in b_res.get('visiblePurposes', [])) and
                   any(n.get('type') == 'coordinator_message' and n.get('sessionId') == COORD for n in b_res.get('newNotifications', [])))

        if self.args.app:
            note = self.ui('notifications', title='Your coordinator needs a decision', purposeLabel='Scope question')
            r['b_notificationRenderedInUi'] = note
            self.check('b_notification_rendered_in_ui_with_purpose', note.get('ok') and note.get('bellPresent') and note.get('titlePresent') and note.get('purposeLabelPresent'))
            time.sleep(3)
            ask_alerts = self.alerts_since()
            r['b_electronAlertsForAsk'] = dict(ask_alerts, note='main-process hook records (does not display) Notification.show and app.dock.bounce; OS banner and bounce display are not observed')
            self.check('electron_ask_raises_critical_dock_bounce_and_os_toast', ask_alerts['dockBounceTypes'] == ['critical'] and any('needs a decision' in (t or '') for t in ask_alerts['osToastTitles']))
            attention = self.ui('attention-routes')
            r['b_needsYouMarkerWhileAskOpen'] = attention
            self.check('b_needs_you_marker_on_sidebar_row_and_home_while_ask_open', attention.get('ok') and attention.get('coordinatorRoute', 0) >= 1 and attention.get('homeRoute', 0) > attention.get('coordinatorRoute', 0))
            self.ui('open-main-from-sidebar', sessionId=COORD)
            self.refocus(); self.alerts_since()
            st_status = self.decision('cv-stopped-' + self.token, 'stopped_work', 'Authorized scratch work has stopped and needs a decision from Ed before it can continue.')
            time.sleep(6)
            sw = self.alerts_since()
            r['alerts_stoppedWorkCritical'] = dict(sw, sendStatus=st_status, via='POST human-decisions (the endpoint `ao tell --kind stopped` uses)')
            self.check('electron_stopped_work_raises_critical_dock_bounce_and_os_toast', st_status < 400 and sw['dockBounceTypes'] == ['critical'] and any('stopped' in (t or '').lower() for t in sw['osToastTitles']))
            self.refocus(); self.alerts_since()
            ui_mark = time.strftime('%Y-%m-%d %H:%M:%S', time.gmtime(time.time() - 2))
            marker = 'cv-live-' + self.token
            sent = self.ui('send', text='Reply with one short sentence confirming you can see this message in the live chat. Take no other action.', marker=marker)
            ui_turn = self.wait(lambda: self.newest_turn_after(ui_mark), 60)
            live = {'send': sent, 'turnStarted': bool(ui_turn)}
            if ui_turn:
                end = self.wait_turn(ui_turn, self.args.turn_wait)
                snippets = self.visible_texts(3, role='assistant', turn=ui_turn, minlen=12)
                time.sleep(2)
                check = self.ui('check-live', marker=marker, snippets=snippets)
                live.update(turnState=end[0] if end else 'timeout', visibleReplySnippets=len(snippets), check=check)
            r['a_replyRendersLiveWithoutRefreshInUi'] = live
            self.check('a_reply_renders_live_in_ui_without_reload', sent.get('ok') and live.get('turnState') == 'completed' and live.get('check', {}).get('markerStillPresent') and live.get('check', {}).get('replySnippetMatches', 0) >= 1)
            time.sleep(3)
            attention2 = self.ui('attention-routes')
            ask = [n for n in self.notifications() if n.get('type') == 'coordinator_message' and n.get('purpose') in PURPOSES_ASK]
            r['b_askResolvedWhenEdReplies'] = {'needsYouAfterReply': {'coordinatorRoute': attention2.get('coordinatorRoute'), 'homeRoute': attention2.get('homeRoute')}, 'askNotifications': [{'id': n['id'], 'resolvedAt': bool(n.get('resolvedAt'))} for n in ask]}
            self.check('b_ask_resolved_and_marker_cleared_after_ed_reply', attention2.get('ok') and attention2.get('coordinatorRoute') == 0 and attention2.get('homeRoute') == 0 and bool(ask) and all(n.get('resolvedAt') for n in ask))
            self.ui('open-main-from-sidebar', sessionId=COORD)
            tog = self.ui('toggle')
            r['e_hiddenContentOpensViaUiToggle'] = tog
            self.check('e_internal_activity_toggle_reveals_and_hides', tog.get('ok') and tog.get('menuItemPresent') and tog.get('checkedBefore') is False and tog.get('internalBefore') == 0 and tog.get('internalAfterOn', 0) > 0 and tog.get('internalLabelPresent') and tog.get('checkedWhenReopened') and tog.get('internalAfterOff') == 0)

        # (d, continued) the coordinator was not killed or respawned and no replacement session exists.
        after_sessions = sql(self.data / 'ao.db', 'select id, num, created_at, kind, harness, project_id, is_terminated from sessions order by id')
        coord_after = [row for row in after_sessions if row[0] == COORD][0]
        host_after = self.host_identity()
        host_before = getattr(self, 'host_before', {})
        stable = bool(host_before) and set(host_before) <= set(host_after) and all(host_after[p] == host_before[p] for p in host_before)
        r['d_noRespawn'] = {'coordinatorRowSameIdentity': coord_before == coord_after, 'sessionIdsUnchanged': [x[0] for x in before_sessions] == [x[0] for x in after_sessions],
                            'hostBeforePids': sorted(host_before), 'hostAfterPids': sorted(host_after), 'sameHostProcess': stable}
        self.check('d_existing_session_not_respawned', coord_before == coord_after and [x[0] for x in before_sessions] == [x[0] for x in after_sessions] and stable)

        # (e, API half) hidden content stays reachable.
        default_view, _ = self.visible_ids(); all_view, all_snap = self.visible_ids(True)
        hidden = [i for i in all_view if i not in default_view]
        r['e_hiddenContentReachableApi'] = {'defaultMessages': len(default_view), 'allViewMessages': len(all_view), 'hiddenMessagesTaggedInternal': all(all_view[i].get('audience') == 'internal' for i in hidden), 'hiddenCount': len(hidden)}
        self.check('e_hidden_content_reachable_via_audience_all', len(hidden) >= 3 and all(all_view[i].get('audience') == 'internal' for i in hidden))
        status, body = self.api('GET', f'/api/v1/sessions/{COORD}/conversation?audience=bogus', timeout=15)
        self.check('audience_invalid_rejected', status == 400)
        self.check('exactly_the_scripted_mutations', all(m['route'] == '/api/v1/sessions' or m['route'].startswith(f'/api/v1/sessions/{COORD}/') for m in self.mutations))
        self.result['mutationAudit'] = self.mutations
        self.result['providerTurns'] = [{'turnId': t[0], 'state': t[1], 'startedAt': t[2], 'completedAt': t[3]} for t in
                                        sql(self.data / 'ao.db', "select id, state, started_at, completed_at from conversation_turns where conversation_id=? order by started_at desc limit 12", cid)]

    def finalize_cv(self):
        self.finalize()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--daemon', type=Path, required=True)
    parser.add_argument('--previous-daemon', type=Path, required=True)
    parser.add_argument('--expect-previous-sha256', required=True)
    parser.add_argument('--expect-sha256', required=True)
    parser.add_argument('--expect-revision', required=True)
    parser.add_argument('--acp-runtime-dir', type=Path, help='defaults to the packaged app copy when --app is given')
    parser.add_argument('--agent-bin-dir', action='append', default=[])
    parser.add_argument('--auth-readonly', action='append', default=[], metavar='SOURCE=HOME_RELATIVE_DEST')
    parser.add_argument('--previous-acp-runtime-dir', type=Path, help='ACP runtime that matches the previous daemon (phase 1)')
    parser.add_argument('--app', type=Path, help='packaged .app: its bundled daemon, acp-runtime and GUI are used (copied into scratch)')
    parser.add_argument('--playwright-dir', default='/private/tmp/ao-personal-66b-4/frontend/node_modules/playwright')
    parser.add_argument('--keep-harness', action='store_true', help='leave the copied coordinator on its original harness')
    parser.add_argument('--gui-probe', action='store_true', help='debug: start the candidate daemon + GUI only and try the CDP driver')
    parser.add_argument('--hold', action='store_true', help='debug: start the confined scratch daemon and wait for <root>/release')
    parser.add_argument('--standing-wait', type=float, default=150)
    parser.add_argument('--turn-wait', type=float, default=240)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--scratch-parent', type=Path, default=Path('/private/tmp'))
    args = parser.parse_args()
    args.daemon = args.daemon.resolve()
    args.mode = 'real'; args.agent = 'codex'; args.pass_env = []; args.answers = {}
    args.scenario = 'coordinator-visibility'; args.poll = 1.0; args.duration = 900
    harness = None
    try:
        harness = CoordinatorVisibility(args)
        if args.acp_runtime_dir:
            harness.env['AO_ACP_RUNTIME_DIR'] = str(args.acp_runtime_dir.resolve())
        harness.env['PATH'] += ':/Users/edstockman/.local/bin:/opt/homebrew/bin'
        a.signal.signal(a.signal.SIGTERM, lambda *_: (_ for _ in ()).throw(KeyboardInterrupt()))
        harness.run_cv()
    except (Exception, KeyboardInterrupt) as e:
        if harness is None:
            print(type(e).__name__ + ': harness initialization failed', file=sys.stderr)
            return 2
        harness.result['errors'].append(type(e).__name__ + ': ' + str(e) if isinstance(e, RuntimeError) else type(e).__name__)
    finally:
        if harness:
            harness.finalize()
    return 0 if harness.result['passed'] else 1


if __name__ == '__main__':
    sys.exit(main())
