import unittest
import acceptance as a


class EvidenceTests(unittest.TestCase):
    def event(self, kind, ref='a' * 40, author='worker', outcome='recorded', at='1'):
        return dict(kind=kind, reference=ref, senderSessionId=author, result=outcome, createdAt=at)

    def test_done_requires_current_independent_review_and_passing_check(self):
        story = {'status': 'done'}
        events = [self.event('implementation', at='1'), self.event('testing', outcome='passed', at='2'),
                  self.event('review_passed', author='reviewer', at='3')]
        self.assertTrue(a.latest_evidence(story, events)['valid'])
        self.assertFalse(a.latest_evidence(story, events[:2])['valid'])
        self.assertFalse(a.latest_evidence(story, events + [self.event('review_failed', at='4')])['valid'])
        self.assertFalse(a.latest_evidence(story, events + [self.event('implementation', ref='b'*40, at='4')])['valid'])
        events[-1]['senderSessionId'] = 'worker'
        self.assertFalse(a.latest_evidence(story, events)['valid'])

    def test_public_payload_is_not_approved_by_prose_or_empty_reason(self):
        message = {'role': 'assistant', 'text': 'Done, everything passed'}
        self.assertEqual(a.classify_message(message), 'non_actionable_or_unclassified')
        message.update(audience='human', purpose='scope_question', text='Which format?\n\nWhy your input is needed: scope choice')
        self.assertEqual(a.classify_message(message), 'scope_question')
        self.assertIn('reasoning_activity', a.privacy_violations({'activityKind': 'reasoning'}))
        self.assertIn('delivery_handle', a.privacy_violations({'text': 'id=private-key'}, ['private-key']))
        self.assertIn('private_sentinel', a.privacy_violations({'text': a.PRIVATE}))
        self.assertEqual(a.privacy_violations({'providerTurnId': ''}), [])

    def test_stable_key_replay_cannot_create_a_second_turn(self):
        rows = [{'conversation_id': 'c', 'client_message_id': 'key', 'turn_id': 't'}]
        self.assertEqual(a.duplicate_dispatches(rows + rows), [])
        self.assertEqual(len(a.duplicate_dispatches(rows + [dict(rows[0], turn_id='other')])), 1)

    def test_idle_never_becomes_done(self):
        self.assertEqual(a.workflow_classification({}, {}, False, 1, 30), 'idle_agent_pending_work')
        self.assertEqual(a.workflow_classification({}, {}, False, 31, 30), 'stalled_ready_work')
        self.assertEqual(a.workflow_classification({'question': 'scope?'}, {}, False, 31, 30), 'blocked_no_ready_work')
        self.assertEqual(a.workflow_classification({'status': 'done'}, {}, False, 31, 30), 'completed_no_ready_work')

class HarnessPolicyTests(unittest.TestCase):
    def test_latency_uses_same_story_stable_delivery(self):
        h = a.Harness.__new__(a.Harness)
        h.report_times = {'r': {'id': 'r', 'story_id': 's', 'created_at': '2026-01-01T00:00:00Z'}}
        h.continuation_history = {'own-key': {'storyId': 's'}, 'other-key': {'storyId': 'other'}}
        h.observed_dispatches = {
            'wrong': {'id': 'wrong', 'client_message_id': 'other-key', 'created_at': '2026-01-01T00:00:01Z'},
            'right': {'id': 'right', 'client_message_id': 'own-key', 'turn_id': 't', 'created_at': '2026-01-01T00:00:03Z'}}
        result = h.latencies([{'story': {'id': 's'}}])
        self.assertEqual(result[0]['seconds'], 3)
        self.assertEqual(result[0]['dispatchMessageId'], 'right')

    def test_scope_answer_does_not_nudge_or_answer_ambiguous_owner(self):
        import types
        h = a.Harness.__new__(a.Harness)
        h.args = types.SimpleNamespace(answers={'Which format?': 'JSON'})
        h.answer_ids = set()
        sent = []
        h.require = lambda *args: sent.append(args)
        message = {'id': 'm', 'role': 'assistant', 'audience': 'human', 'purpose': 'scope_question',
                   'text': 'Which format?\n\nWhy your input is needed: scope decision'}
        story = {'id': 's', 'revision': 2, 'question': message['text'], 'questionEventId': 'm', 'questionRequesterSessionId': 'worker'}
        h.answer_scope(message, [])
        h.answer_scope(message, [{'story': dict(story, questionEventId='wrong')}])
        h.answer_scope(message, [{'story': story}, {'story': dict(story, id='other')}])
        self.assertEqual(sent, [])
        h.answer_scope(message, [{'story': story}])
        h.answer_scope(message, [{'story': story}])
        self.assertEqual(len(sent), 1)
        self.assertEqual(sent[0][1], '/api/v1/workspace/commands')
        self.assertEqual(sent[0][2]['kind'], 'answer')


class ProcessOwnershipTests(unittest.TestCase):
    def test_reused_pid_is_not_an_ancestry_root(self):
        historical = {10: 'old-start'}
        current = {10: (1, 'new-start', 'unrelated'), 11: (10, 'child-start', 'unrelated-child')}
        self.assertEqual(a.live_owned_processes(current, historical, 'unique'), {})

    def test_live_identity_roots_and_detached_marker_are_tracked(self):
        historical = {10: 'same-start', 20: 'old-start'}
        current = {10: (1, 'same-start', 'daemon'), 11: (10, 'child-start', 'worker'),
                   21: (20, 'unrelated-start', 'unrelated'),
                   30: (1, 'detached-start', 'host AO_ACCEPTANCE_RUN_ID=unique')}
        self.assertEqual(set(a.live_owned_processes(current, historical, 'unique')), {10, 11, 30})


class ReviewRegressionTests(unittest.TestCase):
    def test_check_command_cannot_be_an_echo_or_other_story(self):
        self.assertTrue(a.invokes_check('python3 check.py --story-id story-a', 'story-a'))
        self.assertFalse(a.invokes_check('echo python3 check.py --story-id story-a', 'story-a'))
        self.assertFalse(a.invokes_check('python3 check.py --story-id story-b', 'story-a'))
        self.assertFalse(a.invokes_check('cat check.py', 'story-a'))
        self.assertFalse(a.invokes_git_show('echo git show ' + 'a'*40, 'a'*40))
        self.assertTrue(a.invokes_git_show('git show ' + 'a'*40 + ' -- feature.py', 'a'*40))

    def test_opening_prehandoff_and_shared_turns_are_not_provenance(self):
        event = dict(id='h', storyId='story-a', kind='handoff', result='uncertain', recipientSessionId='reviewer',
                     reference='turn', deliveryKind='turn', createdAt='2026-01-01T00:00:10Z')
        self.assertEqual(a.delivered_handoffs([event], 'story-a', 'reviewer', []), {})
        event['result'] = 'delivered'
        receipts = a.delivered_handoffs([event], 'story-a', 'reviewer', [])
        self.assertFalse(a.after_handoff({'turn_id': 'turn', 'created_at': '2026-01-01T00:00:09Z'}, receipts))
        self.assertTrue(a.after_handoff({'turn_id': 'turn', 'created_at': '2026-01-01T00:00:11Z'}, receipts))
        self.assertEqual(a.delivered_handoffs([event, dict(event, id='other', storyId='story-b')], 'story-a', 'reviewer', []), {})
        self.assertFalse(a.invokes_git_show('git show ' + 'a'*40 + ' -- wrong.py', 'a'*40))

    def test_missing_dispatch_is_retained_and_terminal_report_is_excluded(self):
        h = a.Harness.__new__(a.Harness)
        h.report_times = {'r': {'id': 'r', 'story_id': 's', 'created_at': '2026-01-01T00:00:10Z'}}
        h.continuation_history = {}; h.observed_dispatches = {}
        detail = {'story': {'id': 's'}, 'events': []}
        result = h.latencies([detail])
        self.assertTrue(result[0]['required']); self.assertFalse(result[0]['matched'])
        detail['events'] = [{'id': 'done', 'kind': 'done', 'createdAt': '2026-01-01T00:00:09Z'}]
        result = h.latencies([detail])
        self.assertFalse(result[0]['required'])
        self.assertEqual(result[0]['excludedReason'], 'explicit_done_before_report')

    def test_superseded_question_cannot_hide_a_missing_dispatch(self):
        h = a.Harness.__new__(a.Harness)
        h.report_times = {'r': {'id': 'r', 'story_id': 's', 'created_at': '2026-01-01T00:00:10Z'}}
        h.continuation_history = {}; h.observed_dispatches = {}
        events = [dict(id='q1', kind='question', createdAt='2026-01-01T00:00:01Z'),
                  dict(id='q2', kind='question', createdAt='2026-01-01T00:00:02Z'),
                  dict(id='a2', kind='answer', relatedEventId='q2', result='delivered', createdAt='2026-01-01T00:00:03Z')]
        row = h.latencies([{'story': {'id': 's'}, 'events': events}])[0]
        self.assertTrue(row['required']); self.assertFalse(row['matched'])

    @unittest.skipUnless(a.sys.platform == 'darwin', 'macOS sandbox policy test')
    def test_actor_sandbox_cannot_signal_outside_fixture(self):
        import tempfile, types, subprocess, sys, json
        from pathlib import Path
        with tempfile.TemporaryDirectory(prefix='ao-signal-test-', dir='/private/tmp') as directory:
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory); h.port = 65530
            h.args = types.SimpleNamespace(mode='real')
            profile = h.sandbox_profile(False)
            outside = subprocess.Popen(['/bin/sleep', '15'])
            code = """import subprocess,os,signal,sys,json
inside=subprocess.Popen(['/bin/sleep','10'])
try:
 os.kill(inside.pid,signal.SIGTERM);inside.wait(timeout=3)
 try:
  os.kill(int(sys.argv[1]),signal.SIGTERM);denied=False
 except PermissionError: denied=True
 print(json.dumps({'outsideDenied':denied}))
finally:
 if inside.poll() is None: inside.kill();inside.wait()
"""
            try:
                result = subprocess.run(['/usr/bin/sandbox-exec','-f',str(profile),sys.executable,'-c',code,str(outside.pid)],
                                        capture_output=True,text=True,timeout=8)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertTrue(json.loads(result.stdout)['outsideDenied'])
                self.assertIsNone(outside.poll())
            finally:
                if outside.poll() is None: outside.terminate()
                outside.wait(timeout=3)


class FinalizationTests(unittest.TestCase):
    def test_compound_proof_commands_fail_closed(self):
        sha = 'a' * 40
        for command in ['false && git show '+sha+' -- feature.py ; true',
                        'git show '+sha+' -- feature.py || true',
                        'git show '+sha+' -- feature.py | cat',
                        '(git show '+sha+' -- feature.py)']:
            self.assertFalse(a.invokes_git_show(command, sha), command)
        self.assertFalse(a.invokes_check('false && python3 check.py --story-id s ; true', 's'))

    def test_cleanup_exception_cannot_emit_pass(self):
        import tempfile, types, json
        from pathlib import Path
        with tempfile.TemporaryDirectory() as directory:
            h = a.Harness.__new__(a.Harness)
            h.args = types.SimpleNamespace(output=Path(directory), mode='mechanics')
            h.result = {'assertions': {'runtime': {'passed': True}, 'processes_gone': {'passed': True}, 'accounts_purged': {'passed': True}}, 'errors': []}
            def fail(): raise PermissionError('must not appear in result')
            h.stop = fail
            h.purge_accounts = fail
            h.finalize()
            result = json.loads((Path(directory)/'result.json').read_text())
            self.assertFalse(result['passed'])
            self.assertFalse(result['assertions']['processes_gone']['passed'])
            self.assertFalse(result['assertions']['accounts_purged']['passed'])
            self.assertEqual(result['errors'], ['process_teardown:PermissionError', 'credential_teardown:PermissionError'])

    def test_go_sqlite_timestamp_has_timezone_and_fraction(self):
        self.assertEqual(a.stamp('2026-10-25 21:27:27.994829 +0000 UTC'), a.stamp('2026-10-25T21:27:27.994829Z'))


class HostTransportTests(unittest.TestCase):
    @unittest.skipUnless(a.sys.platform == 'darwin', 'macOS sandbox policy test')
    def test_new_host_transport_allowed_existing_listener_denied(self):
        import socket, subprocess, tempfile, types, sys, json
        from pathlib import Path
        with tempfile.TemporaryDirectory(prefix='ao-transport-test-', dir='/private/tmp') as directory, socket.socket() as outside:
            outside.bind(('127.0.0.1', 0)); outside.listen()
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory); h.port = 65530
            h.protected_ports = {outside.getsockname()[1]}
            h.args = types.SimpleNamespace(mode='real')
            profile = h.sandbox_profile(False)
            code = """import socket,sys,json
with socket.socket() as server:
 server.bind(('127.0.0.1',0));server.listen()
 with socket.create_connection(server.getsockname(),timeout=2) as client:
  accepted,_=server.accept();accepted.close()
 denied=False
 try:
  other=socket.create_connection(('127.0.0.1',int(sys.argv[1])),timeout=2);other.close()
 except OSError: denied=True
 print(json.dumps({'privateHostConnected':True,'outsideDenied':denied}))
"""
            result = subprocess.run(['/usr/bin/sandbox-exec','-f',str(profile),sys.executable,'-c',code,str(outside.getsockname()[1])],capture_output=True,text=True,timeout=8)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(json.loads(result.stdout), {'privateHostConnected':True,'outsideDenied':True})


class ProcessEncodingTests(unittest.TestCase):
    def test_non_utf8_unrelated_environment_cannot_abort_cleanup(self):
        from unittest.mock import patch
        h = a.Harness.__new__(a.Harness)
        raw = b'101 1 Sun Oct 25 12:00:00 2026 helper ENV=\xff\n102 101 Sun Oct 25 12:00:01 2026 child\n'
        with patch.object(a.subprocess, 'check_output', return_value=raw):
            processes = h.process_rows()
        self.assertEqual(processes[101][:2], (1, 'Sun Oct 25 12:00:00 2026'))
        self.assertEqual(set(a.live_owned_processes(processes, {101: processes[101][1]}, 'run')), {101, 102})


class FinalEvidenceRegressionTests(unittest.TestCase):
    def test_two_commits_from_one_fixture_do_not_cover_second_fixture(self):
        import tempfile, types
        from pathlib import Path
        from unittest.mock import patch
        class GateReached(Exception): pass
        with tempfile.TemporaryDirectory() as directory:
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory)
            h.profile = h.root / 'unused.sb'
            h.env = {}
            h.result = {}
            h.project_ids = ['labels', 'totals']
            h.repos = [dict(project=p, path=directory, base='b'*40, function='f', contract='assert f()') for p in h.project_ids]
            h.git = lambda *args: 'changed'
            details = [{'story': {'id': sid, 'projectIds': ['labels']}} for sid in ['s1', 's2']]
            evidence = {sid: {'sha': 'a'*40} for sid in ['s1', 's2']}
            gates = []
            def check(name, passed, **metadata):
                gates.append((name, passed, metadata))
                raise GateReached()
            h.check = check
            with patch.object(a.subprocess, 'run', return_value=types.SimpleNamespace(returncode=0)):
                with self.assertRaises(GateReached): h.verify_code({}, details, evidence)
            self.assertEqual(len(h.result['commits']), 2)
            self.assertEqual(gates[0][0], 'actual_code_commits_and_golden_checks')
            self.assertFalse(gates[0][1])
            self.assertEqual(gates[0][2]['coveredProjectIds'], ['labels'])

    def test_binary_hash_is_recorded_and_missing_or_dirty_commit_fails(self):
        import tempfile, types, hashlib
        from pathlib import Path
        from unittest.mock import patch
        class StopBeforeRuntime(Exception): pass
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / 'ao'
            binary.write_bytes(b'fixture daemon bytes')
            for revision, dirty, expected in [('a'*40, 'false', True), ('', 'false', False), ('a'*40, 'true', False)]:
                with self.subTest(revision=revision, dirty=dirty):
                    h = a.Harness.__new__(a.Harness)
                    h.root = Path(directory)
                    h.args = types.SimpleNamespace(daemon=binary)
                    h.result = {'assertions': {}}
                    def stop(): raise StopBeforeRuntime()
                    h.seed_check = stop
                    metadata = 'build vcs.revision='+revision+'\nbuild vcs.modified='+dirty+'\n'
                    with patch.object(a.shutil, 'which', return_value='/fixture/go'), patch.object(a.subprocess, 'check_output', side_effect=['c'*40, metadata]):
                        with self.assertRaises(StopBeforeRuntime): h.run()
                    self.assertEqual(h.result['binary']['sha256'], hashlib.sha256(binary.read_bytes()).hexdigest())
                    self.assertEqual(h.result['assertions']['binary_commit_provenance']['passed'], expected)


if __name__ == '__main__':
    unittest.main()
