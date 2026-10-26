import copy
import json
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


    @unittest.skipUnless(a.sys.platform == 'darwin', 'macOS resolver socket test')
    def test_real_mode_can_reach_only_named_system_resolver(self):
        import tempfile, types, subprocess, sys
        from pathlib import Path
        with tempfile.TemporaryDirectory(prefix='ao-dns-test-', dir='/private/tmp') as directory:
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory); h.port = 65530; h.protected_ports = set()
            h.args = types.SimpleNamespace(mode='real', daemon=Path(sys.executable))
            resolver = '(allow network-outbound (literal "/private/var/run/mDNSResponder"))'
            self.assertNotIn(resolver, h.sandbox_profile(True).read_text())
            profile = h.sandbox_profile(False)
            self.assertIn(resolver, profile.read_text())
            code = 'import socket; s=socket.socket(socket.AF_UNIX); s.connect("/private/var/run/mDNSResponder"); s.close()'
            result = subprocess.run(['/usr/bin/sandbox-exec', '-f', str(profile), sys.executable, '-c', code], cwd=directory, capture_output=True, timeout=8)
            self.assertEqual(result.returncode, 0, result.stderr)
            h.args.mode = 'mechanics'
            self.assertNotIn(resolver, h.sandbox_profile(False).read_text())


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

    def test_real_restart_waits_for_two_stories_and_running_known_actor(self):
        details = [{'story': {'id': sid, 'status': 'in_progress'}} for sid in ['s1', 's2']]
        state = {'sessions': [{'id': 'main'}, {'id': 'worker'}],
                 'conversation_turns': [{'handled_by_session_id': 'worker', 'state': 'running'}]}
        self.assertEqual(a.midflow_actor_ids(state, details, 'main'), ['worker'])
        self.assertEqual(a.midflow_actor_ids(state, details[:1], 'main'), [])
        self.assertEqual(a.midflow_actor_ids(state, [], 'main'), [])
        for actor, status in [('worker', 'queued'), ('worker', 'completed'), ('main', 'running'), ('unknown', 'running'), (None, 'running')]:
            state['conversation_turns'] = [{'handled_by_session_id': actor, 'state': status}]
            self.assertEqual(a.midflow_actor_ids(state, details, 'main'), [])
        state['conversation_turns'] = [{'handled_by_session_id': 'worker', 'state': 'running'}]
        for detail in details: detail['story']['status'] = 'done'
        self.assertEqual(a.midflow_actor_ids(state, details, 'main'), [])

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


class PersistedFormatTests(unittest.TestCase):
    def test_shell_wrapped_proofs_remain_standalone(self):
        import shlex
        sha = 'a'*40
        for shell in ['/bin/zsh', '/bin/bash', '/bin/sh']:
            self.assertTrue(a.invokes_check(shell+' -lc '+shlex.quote('python3 check.py --story-id s'), 's'))
            self.assertTrue(a.invokes_git_show(shell+' -c '+shlex.quote('git show '+sha+' -- feature.py'), sha))
            for payload in ['false && git show '+sha+' -- feature.py ; true', 'echo git show '+sha+' -- feature.py', 'git show '+sha+' -- feature.py | cat']:
                self.assertFalse(a.invokes_git_show(shell+' -lc '+shlex.quote(payload), sha))
        self.assertFalse(a.invokes_check('/unknown/shell -c '+shlex.quote('python3 check.py --story-id s'), 's'))

    def test_failed_check_is_finished_but_running_or_inconsistent_is_not(self):
        self.assertTrue(a.command_finished({'status': 'failed'}, {'exitCode': 1}))
        self.assertTrue(a.command_finished({'status': 'completed'}, {'exitCode': 0}))
        for status, code in [('running', 1), ('failed', 0), ('completed', 1), ('failed', None), ('completed', False)]:
            self.assertFalse(a.command_finished({'status': status}, {'exitCode': code}))

    def test_managers_do_not_count_as_worker_parallelism(self):
        state = {'sessions': [{'id':'m','kind':'orchestrator'}, {'id':'w','kind':'worker'}],
                 'conversation_turns': [{'handled_by_session_id':x,'state':'running'} for x in ['m','w','unknown']]}
        self.assertEqual(a.running_worker_ids(state), ['w'])

    def test_report_receipt_is_separate_from_delayed_actor_start(self):
        state = {'sessions':[{'id':'m'}], 'reports':[{'id':'r','story_id':'s','recipient_session_id':'m','delivery_batch_id':'batch','created_at':'2026-01-01T00:00:00Z'}],
                 'conversation_messages':[{'id':'msg','client_message_id':'batch','conversation_id':'c','turn_id':'t','created_at':'2026-01-01T00:00:01Z'}],
                 'conversation_turns':[{'id':'t','conversation_id':'c','handled_by_session_id':'m','started_at':'2026-01-01T00:02:00Z'}]}
        row = a.report_delivery_timings(state)[0]
        self.assertEqual((row['deliverySeconds'],row['turnStartSeconds']), (1,120))
        state['conversation_turns'][0]['handled_by_session_id'] = 'wrong'
        self.assertFalse(a.report_delivery_timings(state)[0]['matched'])
        self.assertIsNone(a.report_delivery_timings(state)[0]['turnStartSeconds'])


class DeterministicRemediationTests(unittest.TestCase):

    @unittest.skipUnless(a.sys.platform == 'darwin', 'macOS isolated fixed-source behavior check')
    def test_verifier_rejects_already_true_gate_and_broken_initial_behavior(self):
        import tempfile, types, sys
        from pathlib import Path
        with tempfile.TemporaryDirectory(prefix='ao-fixed-review-', dir='/private/tmp') as directory:
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory); h.port = 65530; h.args = types.SimpleNamespace(mode='mechanics')
            h.profile = h.sandbox_profile(False)
            h.env = {'PATH':'/usr/bin:/bin','HOME':directory,'TMPDIR':directory,'PYTHONDONTWRITEBYTECODE':'1'}
            fixture = {'path':directory,'function':'normalize','contract':"assert normalize(' A B ') == 'a-b'\nimport feature\nassert feature.REVIEW_FIX_APPLIED is True"}
            good = "def normalize(value):\n    return value.strip().lower().replace(' ', '-')\nREVIEW_FIX_APPLIED = True\n"
            sources = {'a'*40: good.replace('True','False'), 'b'*40:good}
            h.git_blob = lambda repo, ref: sources[ref.split(':')[0]].encode()
            chain = {'initialSha':'a'*40,'finalSha':'b'*40}
            self.assertEqual(len(h.verify_review_fixture(fixture, chain)), 2)
            # Reviewer's exact false-pass: gate already True, behavior fixed only later.
            sources['a'*40] = "def normalize(value):\n    return value\nREVIEW_FIX_APPLIED = True\n"
            self.assertIsNone(h.verify_review_fixture(fixture, chain))
            # A real False->True transition alone cannot mask broken initial behavior.
            sources['a'*40] = sources['a'*40].replace('True','False')
            self.assertIsNone(h.verify_review_fixture(fixture, chain))
            sources['a'*40] = good
            self.assertIsNone(h.verify_review_fixture(fixture, chain))
            sources['a'*40] = good.replace('True','False')
            sources['b'*40] = good.replace('True','False')
            self.assertIsNone(h.verify_review_fixture(fixture, chain))


    def test_real_fixture_stays_failed_until_required_review_change(self):
        import tempfile, subprocess, sys, os
        from pathlib import Path
        with tempfile.TemporaryDirectory(prefix='ao-review-fixture-', dir='/private/tmp') as directory:
            h = a.Harness.__new__(a.Harness)
            h.root = Path(directory); h.repos = []; h.result = {}
            h.env = {'PATH':'/usr/bin:/bin', 'HOME':directory, 'GIT_AUTHOR_NAME':'fixture', 'GIT_AUTHOR_EMAIL':'fixture@localhost',
                     'GIT_COMMITTER_NAME':'fixture', 'GIT_COMMITTER_EMAIL':'fixture@localhost', 'PYTHONDONTWRITEBYTECODE':'1'}
            h.fixtures()
            fixture = h.repos[0]; repo = Path(fixture['path'])
            checker = (repo/'check.py').read_bytes()
            def check():
                return subprocess.run([sys.executable, 'check.py', '--story-id', 'labels'], cwd=repo, env=h.env, capture_output=True).returncode
            self.assertEqual(check(), 1)
            source = "def normalize(value):\n    return value.strip().lower().replace(' ', '-')\nREVIEW_FIX_APPLIED = False\n"
            (repo/'feature.py').write_text(source)
            self.assertEqual(check(), 1)
            (repo/'feature.py').write_text(source.replace('False', 'True'))
            self.assertEqual(check(), 0)
            self.assertEqual((repo/'check.py').read_bytes(), checker)

    def test_chain_requires_failed_inspection_manager_route_new_commit_and_rereview(self):
        import copy
        old, final = 'a'*40, 'b'*40
        at = lambda n: '2026-01-01T00:00:%02dZ' % n
        events = [dict(id='finding',kind='review_failed',reference=old,senderSessionId='reviewer',createdAt=at(2)),
                  dict(id='route',kind='handoff',result='delivered',reference='receipt',recipientSessionId='worker',senderSessionId='manager',createdAt=at(3)),
                  dict(id='impl',kind='implementation',reference=final,senderSessionId='worker',createdAt=at(4)),
                  dict(id='review',kind='review_passed',reference=final,senderSessionId='reviewer',createdAt=at(6))]
        checks = [dict(actorId='worker',sha=old,exitCode=1,at=at(1)),dict(actorId='worker',sha=final,exitCode=0,at=at(5))]
        inspections = [dict(reviewEventId='finding',sha=old,actorId='reviewer'),dict(reviewEventId='review',sha=final,actorId='reviewer')]
        def chain(e=events,c=checks,i=inspections,sha=final,managers={'manager'},receipts={'route'}):
            return a.review_remediation_chain(e,c,i,'worker','reviewer',sha,managers,receipts)
        self.assertEqual(chain()['finalSha'], final)
        self.assertIsNone(chain(e=[events[0],*events[2:]]))
        self.assertIsNone(chain(i=inspections[1:]))
        self.assertIsNone(chain(i=inspections[:1]))
        self.assertIsNone(chain(sha=old))
        self.assertIsNone(chain(managers={'other'}))
        self.assertIsNone(chain(receipts=set()))
        early = copy.deepcopy(checks); early[1]['at'] = at(2)
        self.assertIsNone(chain(c=early))
        passed = copy.deepcopy(checks); passed[0]['exitCode'] = 0
        self.assertIsNone(chain(c=passed))


class PhaseLatencyPolicyTests(unittest.TestCase):
    def state(self, busy=False):
        turns = [dict(id='target',conversation_id='c',handled_by_session_id='m',started_at='2026-01-01T00:00:20Z')]
        if busy:
            turns[0]['started_at'] = '2026-01-01T00:02:20Z'
            turns.append(dict(id='current',conversation_id='c',handled_by_session_id='m',started_at='2025-12-31T23:59:00Z',completed_at='2026-01-01T00:02:00Z'))
        return {'sessions':[{'id':'m'}], 'conversation_turns':turns, 'conversation_activities':[],
                'conversation_messages':[dict(id='msg',conversation_id='c',turn_id='target',client_message_id='batch',created_at='2026-01-01T00:00:00Z')],
                'reports':[dict(id='r',story_id='s',state='done',recipient_session_id='m',delivery_batch_id='batch',created_at='2026-01-01T00:00:00Z')]}

    def test_idle_busy_bound_and_cold_start_unknown(self):
        for busy in [False,True]:
            state = self.state(busy)
            row = a.recipient_turn_latency(state,'m','2026-01-01T00:00:00Z','target',30)
            self.assertEqual(row['recipientState'],'busy' if busy else 'idle')
            self.assertEqual(row['seconds'],20)
            self.assertTrue(row['passed'])
            self.assertIsNone(row['providerColdStartSeconds'])
            state['conversation_turns'][0]['started_at'] = '2026-01-01T00:%02d:30Z' % (2 if busy else 0)
            self.assertTrue(a.recipient_turn_latency(state,'m','2026-01-01T00:00:00Z','target',30)['passed'])
            state['conversation_turns'][0]['started_at'] = '2026-01-01T00:%02d:30.001Z' % (2 if busy else 0)
            self.assertFalse(a.recipient_turn_latency(state,'m','2026-01-01T00:00:00Z','target',30)['passed'])

    def test_missing_start_or_busy_end_and_wrong_actor_fail(self):
        state = self.state(True)
        state['conversation_turns'][1]['completed_at'] = None
        self.assertFalse(a.recipient_turn_latency(state,'m','2026-01-01T00:00:00Z','target',30)['passed'])
        state = self.state(); state['conversation_turns'][0]['started_at'] = None
        self.assertFalse(a.recipient_turn_latency(state,'m','2026-01-01T00:00:00Z','target',30)['passed'])
        self.assertFalse(a.recipient_turn_latency(self.state(),'wrong','2026-01-01T00:00:00Z','target',30)['passed'])

    def test_phase_batch_and_transition_need_correlated_actual_start(self):
        state = self.state()
        details = [{'story':{'id':'s','assignments':[{'role':'manager','sessionId':'m'}]},'events':[]}]
        self.assertTrue(a.phase_latencies(state,details,{},30)[0]['passed'])
        state['conversation_messages'] = []
        self.assertFalse(a.phase_latencies(state,details,{},30)[0]['passed'])
        state['reports'] = []
        details[0]['events'] = [dict(id='review',storyId='s',kind='in_review',createdAt='2026-01-01T00:00:00Z')]
        state['conversation_messages'] = self.state()['conversation_messages']
        history = {'batch':dict(storyId='s',recipientSessionId='m',phaseStatus='in_review')}
        self.assertTrue(a.phase_latencies(state,details,history,30)[0]['passed'])
        history['batch']['storyId'] = 'wrong'
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])

    def test_busy_same_story_turn_queued_before_signal_can_start_after_owner(self):
        state = self.state(True)
        state['reports'][0]['delivery_batch_id'] = 'missing'
        state['conversation_messages'][0]['created_at'] = '2025-12-31T23:59:30Z'
        details = [{'story':{'id':'s','assignments':[{'role':'manager','sessionId':'m'}]},'events':[]}]
        history = {'batch':dict(storyId='s',recipientSessionId='m',phaseStatus='backlog')}
        row = a.phase_latencies(state,details,history,30)[0]
        self.assertTrue(row['passed'])
        self.assertEqual(row['seconds'],20)
        self.assertEqual(row['correlation'],'same_story_continuation')
        history['batch']['recipientSessionId']='other'
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])

    def test_superseded_question_does_not_exclude_missing_phase_start(self):
        state = self.state(); state['conversation_messages'] = []
        state['reports'][0]['created_at'] = '2026-01-01T00:00:05Z'
        details = [{'story':{'id':'s','assignments':[]},'events':[
            dict(id='q1',kind='question',createdAt='2026-01-01T00:00:01Z'),
            dict(id='q2',kind='question',createdAt='2026-01-01T00:00:02Z'),
            dict(id='answer',kind='answer',relatedEventId='q2',result='delivered',createdAt='2026-01-01T00:00:03Z')]}]
        row = a.phase_latencies(state,details,{},30)[0]
        self.assertTrue(row['required'])
        self.assertFalse(row['passed'])
        self.assertNotIn('excludedReason',row)

    def test_pending_unclaimed_report_uses_exact_cdc_cursor_and_manager(self):
        state = self.state()
        state['reports'][0].update(recipient_session_id=None, delivery_batch_id=None)
        details = [{'story':{'id':'s','assignments':[{'role':'manager','sessionId':'m'}]},'events':[
            dict(id='report-event',storyId='s',kind='report',reference='r',result='done',createdAt='2026-01-01T00:00:00Z')]}]
        history = {'batch':dict(storyId='s',recipientSessionId='m',phaseReportId='report-event')}
        row = a.phase_latencies(state,details,history,30)[0]
        self.assertTrue(row['passed'])
        self.assertIsNone(row['outboxRecipientSessionId'])
        self.assertEqual(row['recipientSessionId'],'m')
        history['batch']['phaseReportId']='unrelated'
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])
        history['batch']['phaseReportId']='report-event'
        history['batch']['recipientSessionId']='unassigned'
        state['sessions'].append({'id':'unassigned'})
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])

    def test_done_correlates_next_ready_story_manager_not_completed_owner(self):
        state = self.state(); state['reports']=[]
        details = [
            {'story':{'id':'completed','epicId':'epic','assignments':[{'role':'manager','sessionId':'old'}]},
             'events':[dict(id='done',storyId='completed',kind='done',createdAt='2026-01-01T00:00:00Z')]},
            {'story':{'id':'next','epicId':'epic','status':'backlog','assignments':[{'role':'manager','sessionId':'m'}]},'events':[]}]
        history = {'batch':dict(storyId='next',recipientSessionId='m',phaseStatus='backlog')}
        row = a.phase_latencies(state,details,history,30,main='coordinator')[0]
        self.assertTrue(row['passed'])
        self.assertEqual(row['dispatchedStoryId'],'next')
        self.assertEqual(row['recipientSessionId'],'m')
        self.assertEqual(row['correlation'],'next_ready_story_continuation')
        history['batch']['storyId']='completed'
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])
        history['batch']['storyId']='unrelated'
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])
        history['batch']['storyId']='next'
        history['batch']['recipientSessionId']='old'
        state['sessions'].append({'id':'old'})
        self.assertFalse(a.phase_latencies(state,details,history,30)[0]['passed'])

    def test_in_turn_revision_linked_action_and_fail_closed_negatives(self):
        state = self.state(True)
        state['conversation_messages'] = []
        sha = 'a' * 40
        events = [dict(id='impl', storyId='s', kind='implementation', reference=sha,
                       senderSessionId='w', createdAt='2025-12-31T23:59:59Z'),
                  dict(id='report', storyId='s', kind='report', reference='r', result='done',
                       senderSessionId='w', createdAt='2026-01-01T00:00:00Z'),
                  dict(id='action', storyId='s', kind='in_review', reference=sha, result='recorded',
                       senderSessionId='m', createdAt='2026-01-01T00:01:00Z')]
        details = [{'story': {'id':'s', 'assignments':[{'role':'manager','sessionId':'m'}, {'role':'worker','sessionId':'w'}]}, 'events':events}]
        row = a.phase_latencies(state, details, {}, 30)[0]
        self.assertTrue(row['passed'])
        self.assertEqual(row['correlation'], 'in_turn_durable_action')
        self.assertEqual(row['actionEventId'], 'action')
        self.assertEqual(row['seconds'], 60)  # no bound on the manager's own turn
        for changes in [dict(storyId='wrong'), dict(kind='assignment'), dict(senderSessionId='other'),
                        dict(reference='b'*40), dict(createdAt='2025-12-31T23:59:59Z'),
                        dict(createdAt='2026-01-01T00:03:00Z')]:
            changed = copy.deepcopy(details)
            changed[0]['events'][-1].update(changes)
            self.assertFalse(a.phase_latencies(state, changed, {}, 30)[0]['passed'], changes)
        late = copy.deepcopy(state)
        late['conversation_messages'] = self.state()['conversation_messages']
        late['conversation_turns'][0]['started_at'] = '2026-01-01T00:05:00Z'
        no_action = copy.deepcopy(details); no_action[0]['events'].pop()
        self.assertFalse(a.phase_latencies(late, no_action, {}, 30)[0]['passed'])

    def test_in_turn_handoff_requires_revision_message_and_recipient(self):
        state = self.state(True); state['conversation_messages'] = []
        state['conversation_turns'].append(dict(id='reviewturn', handled_by_session_id='reviewer'))
        state['conversation_messages'].append(dict(id='handoff-message',turn_id='reviewturn',role='user',
            sender_session_id='m',text='Review ' + 'a'*40,created_at='2026-01-01T00:01:00Z'))
        details = [{'story':{'id':'s','assignments':[{'role':'manager','sessionId':'m'},{'role':'reviewer','sessionId':'reviewer'}]},'events':[
            dict(id='impl',storyId='s',kind='implementation',reference='a'*40,senderSessionId='w',createdAt='2025-12-31T23:59:59Z'),
            dict(id='report',storyId='s',kind='report',reference='r',result='done',senderSessionId='w',createdAt='2026-01-01T00:00:00Z'),
            dict(id='handoff',storyId='s',kind='handoff',reference='reviewturn',deliveryKind='turn',result='delivered',senderSessionId='m',recipientSessionId='reviewer',createdAt='2026-01-01T00:01:00Z')]}]
        self.assertTrue(a.phase_latencies(state,details,{},30)[0]['passed'])
        state['conversation_messages'][0]['text']='Unrelated task'
        self.assertFalse(a.phase_latencies(state,details,{},30)[0]['passed'])
        state['conversation_messages'][0]['text']='Review '+'a'*40
        state['conversation_turns'][-1]['handled_by_session_id']='other'
        self.assertFalse(a.phase_latencies(state,details,{},30)[0]['passed'])

    SHA = 'a' * 40

    def scenario(self, outcome='review_passed', handoff=None, message_at='2026-01-01T00:01:00Z', duplicate=False):
        """In Review trigger inside a busy manager turn; the action is a Ready write or a handoff."""
        state = self.state(True); state['reports'] = []; state['conversation_messages'] = []
        state['conversation_turns'] += [dict(id='wturn', handled_by_session_id='w'), dict(id='w2turn', handled_by_session_id='w2'),
                                        dict(id='rturn', handled_by_session_id='reviewer')]
        events = [dict(id='impl', storyId='s', kind='implementation', reference=self.SHA, senderSessionId='w', createdAt='2025-12-31T23:59:30Z'),
                  dict(id='trig', storyId='s', kind='in_review', reference=self.SHA, result='recorded', senderSessionId='m', createdAt='2026-01-01T00:00:00Z')]
        if outcome:
            events.append(dict(id='outcome', storyId='s', kind=outcome, reference=self.SHA, senderSessionId='reviewer', createdAt='2026-01-01T00:00:30Z'))
        action = dict(id='action', storyId='s', reference=self.SHA, result='recorded', senderSessionId='m', createdAt='2026-01-01T00:01:00Z', kind='ready_for_approval')
        if handoff:
            recipient, turn, delivery = handoff
            action.update(kind='handoff', reference=turn, deliveryKind=delivery, result='delivered', recipientSessionId=recipient)
            state['conversation_messages'].append(dict(id='hm', turn_id=turn, role='user', sender_session_id='m',
                                                       text='Fix ' + self.SHA, created_at=message_at))
            if duplicate:
                state['workspace_story_events'] = [dict(record=json.dumps(e)) for e in (action, dict(action, id='twin'))]
        events.append(action)
        assignments = [{'role': 'manager', 'sessionId': 'm'}, {'role': 'worker', 'sessionId': 'w'},
                       {'role': 'worker', 'sessionId': 'w2'}, {'role': 'reviewer', 'sessionId': 'reviewer'}]
        return state, [{'story': {'id': 's', 'assignments': assignments}, 'events': events}]

    def trigger(self, *scenario):
        return next(r for r in a.phase_latencies(*scenario[:1], scenario[1], {}, 30) if r['sourceId'] == 'trig')

    def test_in_review_ready_needs_intervening_reviewer_pass(self):
        row = self.trigger(*self.scenario())
        self.assertTrue(row['passed'])
        self.assertEqual((row['correlation'], row['phaseEvidenceEventId'], row['actionKind']),
                         ('in_turn_durable_action', 'outcome', 'ready_for_approval'))
        self.assertFalse(self.trigger(*self.scenario(outcome=None))['passed'], 'no review outcome')
        self.assertFalse(self.trigger(*self.scenario('review_failed'))['passed'], 'failed review cannot become Ready')
        for change in [dict(senderSessionId='m'), dict(reference='b' * 40), dict(createdAt='2026-01-01T00:02:00Z')]:
            state, details = self.scenario()
            next(e for e in details[0]['events'] if e['id'] == 'outcome').update(change)
            self.assertFalse(self.trigger(state, details)['passed'], change)

    def test_review_failed_handoff_targets_implementation_author_by_turn(self):
        row = self.trigger(*self.scenario('review_failed', ('w', 'wturn', 'turn')))
        self.assertTrue(row['passed'])
        self.assertEqual((row['actionKind'], row['actionMessageId'], row['phaseEvidenceEventId']), ('handoff', 'hm', 'outcome'))
        for label, handoff in [('assigned worker who is not the author', ('w2', 'w2turn', 'turn')),
                               ('wrong role', ('reviewer', 'rturn', 'turn')),
                               ('steering receipt', ('w', 'wturn', 'steer'))]:
            self.assertFalse(self.trigger(*self.scenario('review_failed', handoff))['passed'], label)

    def test_handoff_duplicate_event_and_message_window_fail_closed(self):
        handoff = ('w', 'wturn', 'turn')
        self.assertFalse(self.trigger(*self.scenario('review_failed', handoff, duplicate=True))['passed'], 'two handoff events for one turn')
        self.assertTrue(self.trigger(*self.scenario('review_failed', handoff, message_at='2026-01-01T00:01:02Z'))['passed'], '2 s boundary')
        self.assertFalse(self.trigger(*self.scenario('review_failed', handoff, message_at='2026-01-01T00:01:03Z'))['passed'], 'message outside 2 s')

    def idle_case(self, manager_end='2026-01-01T00:00:20Z', wake_at='2026-01-01T00:01:08Z', cursor='rev', batch_turn=None, wake_actor='m'):
        state = {'sessions': [{'id': 'm'}, {'id': 'w'}],
                 'conversation_turns': [dict(id='t0', conversation_id='c', handled_by_session_id='m', started_at='2026-01-01T00:00:00Z', completed_at=manager_end),
                                        dict(id='wake', conversation_id='c', handled_by_session_id=wake_actor, started_at=wake_at)],
                 'conversation_activities': [],
                 'conversation_messages': [dict(id='wm', conversation_id='c', turn_id='wake', client_message_id='story-continue-x', created_at=wake_at)],
                 'reports': [dict(id='r', story_id='s', session_id='w', state='done', delivery_batch_id='batch', created_at='2026-01-01T00:01:00Z')]}
        if batch_turn:
            state['conversation_messages'].append(dict(id='bm', conversation_id='c', turn_id=batch_turn, client_message_id='batch', created_at='2026-01-01T00:06:00Z'))
        details = [{'story': {'id': 's', 'assignments': [{'role': 'manager', 'sessionId': 'm'}, {'role': 'worker', 'sessionId': 'w'}]}, 'events': [
            dict(id='rev', storyId='s', kind='report', reference='r', result='done', senderSessionId='w', createdAt='2026-01-01T00:01:00Z'),
            dict(id='act', storyId='s', kind='in_review', senderSessionId='m', createdAt='2026-01-01T00:01:20Z')]}]
        links = {'story-continue-x': {'storyId': 's', 'phaseReportId': cursor}}
        return state, details, links

    def test_idle_manager_cdc_wake_requires_idle_gate_link_and_bound(self):
        out = a.idle_wake_evidence(*self.idle_case(), now=1e12)
        self.assertTrue(out['passed'] and out['decided'])
        self.assertEqual((out['managerIdleSeconds'], out['wake']['seconds'], out['wake']['recipientState']), (40, 8, 'idle'))
        self.assertEqual((out['nextManagerAction']['eventId'], out['nextManagerAction']['seconds']), ('act', 12))
        busy = a.idle_wake_evidence(*self.idle_case(manager_end='2026-01-01T00:00:55Z'), now=1e12)
        self.assertEqual((busy['passed'], busy['decided'], busy['failure']), (False, True, 'manager_not_idle_before_done'))
        gate = a.idle_wake_evidence(*self.idle_case(manager_end='2026-01-01T00:00:50Z'), now=1e12)
        self.assertTrue(gate['passed'], 'exactly 10 s idle is enough')
        for label, kwargs in [('over bound', dict(wake_at='2026-01-01T00:01:31Z')), ('wrong cursor', dict(cursor='other')),
                              ('wake handled by someone else', dict(wake_actor='w')), ('before the trigger', dict(wake_at='2026-01-01T00:00:59Z')),
                              ('outbox batch is the wake turn', dict(batch_turn='wake'))]:
            self.assertFalse(a.idle_wake_evidence(*self.idle_case(**kwargs), now=1e12)['passed'], label)

    def test_idle_wake_wrong_story_cursor_and_outbox_only_fail_and_wait_for_deadline(self):
        state, details, links = self.idle_case()
        links['story-continue-x']['storyId'] = 'other'
        self.assertFalse(a.idle_wake_evidence(state, details, links, now=1e12)['passed'])
        # The late ordinary batch must not stand in for a CDC wake.
        state, details, links = self.idle_case(batch_turn='later')
        state['conversation_turns'].append(dict(id='later', conversation_id='c', handled_by_session_id='m', started_at='2026-01-01T00:06:00Z'))
        state['conversation_messages'] = [m for m in state['conversation_messages'] if m['id'] != 'wm']
        early = a.idle_wake_evidence(state, details, links, now=a.stamp('2026-01-01T00:01:10Z'))
        self.assertEqual((early['decided'], early['passed']), (False, False))
        late = a.idle_wake_evidence(state, details, links, now=a.stamp('2026-01-01T00:02:00Z'))
        self.assertEqual((late['decided'], late['passed'], late['failure']), (True, False, 'no_cdc_wake_turn'))

    def test_in_turn_ready_requires_later_same_revision_done(self):
        state = self.state(True); state['reports'] = []; state['conversation_messages'] = []
        details = [{'story': {'id':'s','assignments':[{'role':'manager','sessionId':'m'}]}, 'events':[
            dict(id='ready',storyId='s',kind='ready_for_approval',reference='a'*40,senderSessionId='m',createdAt='2026-01-01T00:00:00Z'),
            dict(id='done',storyId='s',kind='done',reference='a'*40,result='recorded',senderSessionId='m',createdAt='2026-01-01T00:01:00Z')]}]
        self.assertTrue(a.phase_latencies(state,details,{},30)[0]['passed'])
        details[0]['events'][1]['reference']='b'*40
        self.assertFalse(a.phase_latencies(state,details,{},30)[0]['passed'])

    def test_done_excluded_only_without_other_ready_story(self):
        state = self.state();state['reports']=[]
        details = [{'story':{'id':'s','assignments':[{'role':'manager','sessionId':'m'}]},'events':[dict(id='done',storyId='s',kind='done',createdAt='2026-01-01T00:00:00Z')]}]
        self.assertFalse(a.phase_latencies(state,details,{},30)[0]['required'])
        details.append({'story':{'id':'ready','assignments':[]},'events':[]})
        self.assertTrue(a.phase_latencies(state,details,{},30)[0]['required'])


if __name__ == '__main__':
    unittest.main()
