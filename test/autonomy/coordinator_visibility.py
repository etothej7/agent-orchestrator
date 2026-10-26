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

import acceptance as a

LIVE_DB = Path.home() / '.ao/data/ao.db'
COORD = 'overall-coordinator-1'
STANDING = 'ao-standing-coordinator-'
PURPOSES_ASK = {'scope_question', 'blocking_decision', 'completion_decision'}


def sql(path, query, *params):
    with a.db_open(path) as db:
        return db.execute(query, params).fetchall()


class CoordinatorVisibility(a.Harness):
    def snapshot(self, audience_all=False, limit=200):
        route = f'/api/v1/sessions/{COORD}/conversation?limit={limit}' + ('&audience=all' if audience_all else '')
        status, body = self.api('GET', route, timeout=30)
        return body if status == 200 else {'messages': [], 'activities': [], 'turns': [], 'errorCode': body.get('errorCode')}

    def conversation_id(self):
        row = sql(self.data / 'ao.db', "select id from conversations where current_session_id=? or session_id=? order by latest_sequence desc limit 1", COORD, COORD)
        return row[0][0]

    def prepare_db(self):
        target = self.data / 'ao.db'
        with a.db_open(LIVE_DB) as src, sqlite3.connect(target) as dest:
            src.backup(dest)
        target.chmod(0o600)
        coord_ws = self.root / 'coordinator-workspace'
        coord_project = self.root / 'coordination'
        for p in (coord_ws, coord_project):
            p.mkdir(parents=True)
        for p in (coord_ws, coord_project):
            subprocess.run(['/usr/bin/git', 'init', '-q', '-b', 'main', str(p)], check=True)
        conn = sqlite3.connect(target)
        before = conn.execute("select model, reasoning_effort from conversations where current_session_id=? or session_id=?", (COORD, COORD)).fetchall()
        # Only the coordinator is made runnable in scratch; paths move into the scratch root.
        conn.execute("update projects set path=? where id='overall-coordinator'", (str(coord_project),))
        conn.execute("update sessions set workspace_path=?, workspace_repo_path='' where id=?", (str(coord_ws), COORD))
        conn.execute("update sessions set model=?, effort='high' where id=?", (self.args.coordinator_model, COORD))
        conn.execute("update conversations set model=?, reasoning_effort='high' where current_session_id=? or session_id=?", (self.args.coordinator_model, COORD, COORD))
        conn.commit(); conn.close()
        self.result['dbCopy'] = {'source': 'live ao.db via sqlite backup API (mode=ro)', 'sha256': hashlib.sha256(target.read_bytes()).hexdigest(),
                                 'bytes': target.stat().st_size, 'coordinatorModelOverride': self.args.coordinator_model,
                                 'originalCoordinatorConversationSettings': [list(r) for r in before]}

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
        snap = self.snapshot(audience_all)
        return {m['id']: m for m in snap.get('messages', [])}, snap

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
        r.update(scenario='coordinator-visibility', kind='targeted_coordinator_visibility_backend_not_acceptance', autonomyAccepted=False)
        r['harnessSha'] = subprocess.check_output(['/usr/bin/git', '-C', str(Path(__file__).resolve().parents[2]), 'rev-parse', 'HEAD'], text=True).strip()
        r['harnessSourceSha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        data = self.args.daemon.read_bytes()
        r['binary'] = {'path': str(self.args.daemon), 'sha256': hashlib.sha256(data).hexdigest()}
        meta = subprocess.check_output(['go', 'version', '-m', str(self.args.daemon)], text=True, cwd=self.root)
        for key in ('vcs.revision', 'vcs.modified'):
            m = re.search(r'\b' + re.escape(key) + r'=(\S+)', meta)
            r['binary'][key] = m.group(1) if m else None
        self.check('binary_hash_and_clean_revision', r['binary']['sha256'] == self.args.expect_sha256 and r['binary']['vcs.revision'] == self.args.expect_revision and r['binary']['vcs.modified'] == 'false',
                   expectedSha256=self.args.expect_sha256, expectedRevision=self.args.expect_revision)
        self.prepare_db()
        before_sessions = sql(self.data / 'ao.db', 'select id, num, created_at, kind, harness, project_id, is_terminated from sessions order by id')
        coord_before = [row for row in before_sessions if row[0] == COORD][0]
        self.start()
        self.check('isolated_daemon_ready', True)
        cid = self.conversation_id()
        # Fixed snapshot of what Ed sees before any new traffic.
        default0, _ = self.visible_ids()
        r['before'] = {'visibleMessages': len(default0), 'storedMessages': sql(self.data / 'ao.db', 'select count(*) from conversation_messages where conversation_id=?', cid)[0][0]}

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
            self.host_before = host
        r['d_existingCoordinatorStandingInstruction'] = d
        self.check('d_standing_instruction_delivered_hidden', d['delivered'] and d.get('turnState') == 'completed' and d.get('hiddenInDefaultView') and d.get('taggedInternalInAllView') and d.get('assistantRepliesHiddenInDefault'))

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
        r['c_routineReportHidden'] = c_res
        self.check('c_routine_report_hidden_no_notification', c_res.get('turnState') == 'completed' and c_res.get('newVisibleMessages') == 0 and c_res.get('newStoredMessagesInAllView', 0) >= 1 and c_res.get('newMessagesTaggedInternal') and c_res.get('newCoordinatorNotifications') == 0)

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
        self.check('exactly_the_scripted_mutations', all(m['route'].startswith(f'/api/v1/sessions/{COORD}/') for m in self.mutations))
        self.result['mutationAudit'] = self.mutations
        self.result['providerTurns'] = [{'turnId': t[0], 'state': t[1], 'startedAt': t[2], 'completedAt': t[3]} for t in
                                        sql(self.data / 'ao.db', "select id, state, started_at, completed_at from conversation_turns where conversation_id=? order by started_at desc limit 12", cid)]

    def finalize_cv(self):
        self.finalize()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--daemon', type=Path, required=True)
    parser.add_argument('--expect-sha256', required=True)
    parser.add_argument('--expect-revision', required=True)
    parser.add_argument('--acp-runtime-dir', type=Path, required=True)
    parser.add_argument('--agent-bin-dir', action='append', default=[])
    parser.add_argument('--coordinator-model', default='claude-sonnet-5-5')
    parser.add_argument('--standing-wait', type=float, default=150)
    parser.add_argument('--turn-wait', type=float, default=240)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--scratch-parent', type=Path, default=Path('/private/tmp'))
    args = parser.parse_args()
    args.daemon = args.daemon.resolve()
    args.mode = 'real'; args.agent = 'claude-code'; args.pass_env = []; args.auth_readonly = []; args.answers = {}
    args.scenario = 'coordinator-visibility'; args.poll = 1.0; args.duration = 900
    harness = None
    try:
        harness = CoordinatorVisibility(args)
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
