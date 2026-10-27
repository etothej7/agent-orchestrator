import json
import os
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import autoresume as R  # noqa: E402


def sample(t, live=(), activity=None, hosts=None):
    sessions = {sid: {'activity': (activity or {}).get(sid, 'idle'), 'terminated': False, 'hibernated': False} for sid in live}
    return {'t': t, 'sessions': sessions, 'hosts': {sid: [100 + i] for i, sid in enumerate(live)} if hosts is None else hosts}


def view(**kw):
    base = {'activity': 'idle', 'terminated': False, 'hibernated': False, 'workspace': '/w', 'convId': 'c',
            'providerConversation': 'ao-harness:x:p1', 'prompt': 'h', 'policy': 'allowed', 'stopReason': '', 'fingerprint': None}
    return {**base, **kw}


def snap(sessions, turns=None, messages=None):
    return {'sessions': sessions, 'turns': turns or {s: [] for s in sessions}, 'messages': messages or {s: [] for s in sessions}}


def msg(text='x', origin='human', key='', role='user', turn='t1'):
    return {'id': 'm' + text[:6] + key, 'origin': origin, 'role': role, 'text': text, 'key': key, 'turnId': turn, 'createdAt': ''}


class StageOrder(unittest.TestCase):
    stages = [['coord'], ['mgr'], ['wkr', 'cdx']]

    def test_barrier_respected(self):
        tl = [sample(1, ['coord']), sample(2, ['coord', 'mgr']), sample(3, ['coord', 'mgr', 'wkr', 'cdx'])]
        self.assertTrue(R.stage_order_gate(tl, self.stages)['passed'])

    def test_worker_before_coordinator_fails(self):
        tl = [sample(1, ['wkr']), sample(5, ['wkr', 'coord', 'mgr', 'cdx'])]
        result = R.stage_order_gate(tl, self.stages)
        self.assertFalse(result['passed'])
        self.assertTrue(result['violations'])

    def test_missing_session_fails(self):
        tl = [sample(1, ['coord']), sample(2, ['coord', 'mgr', 'wkr'])]
        self.assertEqual(R.stage_order_gate(tl, self.stages)['missing'], ['cdx'])

    def test_exited_row_is_not_live(self):
        tl = [sample(1, ['coord'], activity={'coord': 'exited'})]
        self.assertIsNone(R.first_live(tl, 'coord'))

    def test_host_without_a_row_state_is_not_live(self):
        self.assertIsNone(R.first_live([{'t': 1, 'sessions': {}, 'hosts': {'coord': [9]}}], 'coord'))


class ActiveAndHibernate(unittest.TestCase):
    def test_active_resume_passes_and_detects_changes(self):
        before = snap({'a': view()})
        tl = [sample(1, ['a'])]
        self.assertTrue(R.active_resume_gate(tl, before, snap({'a': view()}), ['a'])['passed'])
        changed = snap({'a': view(providerConversation='ao-harness:x:p2')})
        self.assertIn('provider_conversation_changed', R.active_resume_gate(tl, before, changed, ['a'])['problems']['a'])
        self.assertIn('never_live', R.active_resume_gate([], before, before, ['a'])['problems']['a'])
        gone = snap({'a': view(activity='exited')})
        self.assertIn('not_live_at_end', R.active_resume_gate(tl, before, gone, ['a'])['problems']['a'])

    def test_hibernate_requires_marker_not_exited_and_no_provider(self):
        parked = snap({'m': view(hibernated=True)})
        self.assertTrue(R.hibernate_gate([sample(1, [])], parked, ['m'])['passed'])
        self.assertIn('not_hibernated', R.hibernate_gate([], snap({'m': view()}), ['m'])['problems']['m'])
        self.assertIn('exited', R.hibernate_gate([], snap({'m': view(hibernated=True, activity='exited')}), ['m'])['problems']['m'])
        self.assertIn('provider_launched', R.hibernate_gate([sample(1, ['m'])], parked, ['m'])['problems']['m'])

    def test_wake_gate(self):
        good = {'m': {'status': 202, 'turnStarted': True, 'live': True, 'sameConversation': True}}
        self.assertTrue(R.wake_gate(good)['passed'])
        self.assertFalse(R.wake_gate({'m': {**good['m'], 'status': 409}})['passed'])
        self.assertFalse(R.wake_gate({'m': {**good['m'], 'sameConversation': False}})['passed'])
        self.assertFalse(R.wake_gate({})['passed'])


class StaysDownAndWorktrees(unittest.TestCase):
    def test_stays_down(self):
        before = snap({'n': view()}, turns={'n': []})
        after = snap({'n': view(activity='exited', policy='never')}, turns={'n': []})
        self.assertTrue(R.stays_down_gate([sample(1, [])], before, after, ['n'], 'never')['passed'])
        self.assertIn('launched', R.stays_down_gate([sample(1, ['n'])], before, after, ['n'])['problems']['n'])
        up = snap({'n': view(activity='idle')})
        self.assertIn('not_down_at_end', R.stays_down_gate([], before, up, ['n'])['problems']['n'])
        self.assertIn('policy_not_never', R.stays_down_gate([], before, snap({'n': view(activity='exited')}), ['n'], 'never')['problems']['n'])
        grown = snap({'n': view(activity='exited')}, turns={'n': [{'id': 't'}]})
        self.assertIn('new_turns', R.stays_down_gate([], before, grown, ['n'])['problems']['n'])

    def test_worktree_gate(self):
        fp = {'exists': True, 'digest': 'd', 'entries': 3, 'rootInode': 1, 'rootBirth': 2.0}
        before = snap({'a': view(fingerprint=fp)})
        self.assertTrue(R.worktree_gate(before, snap({'a': view(fingerprint=dict(fp))}))['passed'])
        self.assertIn('digest', R.worktree_gate(before, snap({'a': view(fingerprint={**fp, 'digest': 'e'})}))['problems']['a'])
        self.assertIn('rootInode', R.worktree_gate(before, snap({'a': view(fingerprint={**fp, 'rootInode': 9})}))['problems']['a'])
        self.assertIn('exists', R.worktree_gate(before, snap({'a': view(fingerprint={'exists': False})}))['problems']['a'])
        self.assertFalse(R.worktree_gate(snap({'a': view()}), snap({'a': view()}))['passed'])  # nothing compared is not a pass

    def test_tree_fingerprint_detects_content_new_file_and_recreation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / 'wt'
            root.mkdir()
            (root / 'a.txt').write_text('one')
            first = R.tree_fingerprint(root)
            self.assertEqual(first, R.tree_fingerprint(root))
            (root / 'a.txt').write_text('two')
            self.assertNotEqual(first['digest'], R.tree_fingerprint(root)['digest'])
            (root / 'a.txt').write_text('one')
            (root / 'b.txt').write_text('new')
            self.assertNotEqual(first['digest'], R.tree_fingerprint(root)['digest'])
            (root / 'b.txt').unlink()
            self.assertEqual(first['digest'], R.tree_fingerprint(root)['digest'])
            (root / 'sub').mkdir()
            (root / 'link').symlink_to('a.txt')
            linked = R.tree_fingerprint(root)
            (root / 'link').unlink()
            (root / 'link').symlink_to('sub')
            self.assertNotEqual(linked['digest'], R.tree_fingerprint(root)['digest'])
            self.assertFalse(R.tree_fingerprint(Path(tmp) / 'missing')['exists'])


class ContinueTurns(unittest.TestCase):
    key = R.CONTINUE_PREFIX + 's1:boot1'
    text = 'AO restarted. Continue where you left off.'

    def after(self, **messages):
        return snap({s: view() for s in messages}, messages={s: m for s, m in messages.items()})

    def test_exactly_one_keyed_automation_turn(self):
        after = self.after(w=[msg(self.text, 'automation', self.key)], idle=[])
        self.assertTrue(R.continue_turn_gate(None, after, ['w'], ['idle'])['passed'])

    def test_missing_duplicate_wrong_origin_unkeyed_and_unexpected_all_fail(self):
        for name, messages, expect in [
                ('missing', {'w': [], 'i': []}, ['w']),
                ('duplicate', {'w': [msg(self.text, 'automation', self.key), msg(self.text, 'automation', self.key + 'x')], 'i': []}, ['w']),
                ('human', {'w': [msg(self.text, 'human', self.key)], 'i': []}, ['w']),
                ('unkeyed', {'w': [msg(self.text, 'automation', '')], 'i': []}, ['w']),
                ('noturn', {'w': [msg(self.text, 'automation', self.key, turn=None)], 'i': []}, ['w']),
                ('idle_got_one', {'w': [msg(self.text, 'automation', self.key)], 'i': [msg(self.text, 'automation', self.key + 'i')]}, ['w'])]:
            with self.subTest(name):
                self.assertFalse(R.continue_turn_gate(None, self.after(**messages), expect, ['i'])['passed'])


class NoFreshRelaunch(unittest.TestCase):
    def setUp(self):
        self.before = snap({'a': view(workspace='/w/a')})
        self.after = snap({'a': view(workspace='/w/a')}, messages={'a': [msg('AR_ORIG_WKR_1 do it')]})
        self.live = [sample(1, ['a'])]
        self.tokens = {'a': 'AR_ORIG_WKR_1'}

    def gate(self, shim=(), after=None, timeline=None):
        return R.no_fresh_relaunch_gate(self.before, after or self.after, list(shim), 0, ['a'], self.tokens, self.live if timeline is None else timeline)

    def test_clean_resume_passes(self):
        loads = [{'t': 2, 'method': 'session/load', 'cwd': '/w/a', 'sid': 'p1', 'text': ''}]
        self.assertTrue(self.gate(loads)['passed'])

    def test_session_new_and_prompt_replay_fail(self):
        self.assertIn('fresh_session_new', self.gate([{'t': 2, 'method': 'session/new', 'cwd': '/w/a', 'sid': '', 'text': ''}])['problems']['a'])
        replay = {'t': 2, 'method': 'session/prompt', 'cwd': '', 'sid': 'p1', 'text': 'AR_ORIG_WKR_1 do it'}
        self.assertIn('original_prompt_resent', self.gate([replay])['problems']['a'])

    def test_other_sessions_and_pre_restart_calls_are_ignored(self):
        other = {'t': 2, 'method': 'session/new', 'cwd': '/w/b', 'sid': '', 'text': ''}
        old = {'t': -5, 'method': 'session/new', 'cwd': '/w/a', 'sid': '', 'text': ''}
        self.assertTrue(self.gate([other, old])['passed'])

    def test_changed_conversation_prompt_or_resent_message_fail(self):
        changed = snap({'a': view(workspace='/w/a', providerConversation='ao-harness:x:p2')})
        self.assertIn('conversation_changed', self.gate(after=changed)['problems']['a'])
        twice = snap({'a': view(workspace='/w/a')}, messages={'a': [msg('AR_ORIG_WKR_1 a'), msg('AR_ORIG_WKR_1 b')]})
        self.assertIn('original_prompt_messages=2', self.gate(after=twice)['problems']['a'])

    def test_vacuous_when_never_resumed(self):
        self.assertIn('not_resumed_so_nothing_to_judge', self.gate(timeline=[])['problems']['a'])


class DuplicatesNoticesCodex(unittest.TestCase):
    def test_duplicates(self):
        after = snap({'a': view()}, messages={'a': [msg('AO restarted', 'automation', R.CONTINUE_PREFIX + 'k')]})
        self.assertTrue(R.duplicate_gate([sample(1, ['a'])], after, ['a'])['passed'])
        two_hosts = [{'t': 1, 'sessions': {'a': {'activity': 'idle'}}, 'hosts': {'a': [1, 2]}}]
        self.assertIn('simultaneous_hosts=2', R.duplicate_gate(two_hosts, after, ['a'])['problems']['a'])
        twice = snap({'a': view()}, messages={'a': [msg('AO restarted', 'automation', R.CONTINUE_PREFIX + 'k'), msg('AO restarted', 'automation', R.CONTINUE_PREFIX + 'j')]})
        self.assertIn('continue_turns=2', R.duplicate_gate([sample(1, ['a'])], twice, ['a'])['problems']['a'])
        self.assertFalse(R.duplicate_gate([], after, ['a'])['passed'])

    def test_failure_notice(self):
        after = snap({'f': view(activity='exited')})
        notice = lambda sid: {'text': f"Session {sid} couldn't resume: its saved conversation can't be resumed safely", 'createdAt': ''}  # noqa: E731
        self.assertTrue(R.failure_notice_gate([notice('f')], 'f', ['w', 'coord'], after, [sample(1, [])])['passed'])
        self.assertFalse(R.failure_notice_gate([], 'f', ['w'], after, [])['passed'])
        self.assertFalse(R.failure_notice_gate([notice('f'), notice('f')], 'f', ['w'], after, [])['passed'])
        self.assertFalse(R.failure_notice_gate([notice('f'), notice('w')], 'f', ['w'], after, [])['passed'])
        self.assertFalse(R.failure_notice_gate([notice('f')], 'f', ['w'], after, [sample(1, ['f'])])['passed'])
        self.assertTrue(R.NOTICE_TEXT.search("Session x couldn’t resume"))

    def test_codex_settle(self):
        after = snap({'c': view()})
        self.assertEqual(R.codex_settle_gate([sample(50, ['c'])], after, [], 'c', 0, 120)['outcome'], 'resumed')
        self.assertEqual(R.codex_settle_gate([sample(500, ['c'])], after, [], 'c', 0, 120)['outcome'], 'resumed_late')
        notice = [{'text': 'Session c couldn\'t resume', 'createdAt': ''}]
        self.assertTrue(R.codex_settle_gate([sample(1, [])], after, notice, 'c', 0, 120)['passed'])
        self.assertEqual(R.codex_settle_gate([sample(1, [])], after, [], 'c', 0, 120)['outcome'], 'neither')


class Plumbing(unittest.TestCase):
    def test_provider_sid_and_host_regex(self):
        self.assertEqual(R.provider_sid('ao-harness:opencode-v2:ar-123'), 'ar-123')
        self.assertEqual(R.provider_sid(''), '')
        command = '/x/ao chat-host ar-work-1 /data /w acp abc -- opencode acp AO_ACCEPTANCE_RUN_ID=t'
        import re
        self.assertEqual(re.search(r'\bchat-host\s+(\S+)\s', command).group(1), 'ar-work-1')

    def test_cycle_definitions_cover_the_requested_scenarios(self):
        self.assertEqual({c['order'] for c in R.CYCLES.values()}, {'hosts-first', 'daemon-first', 'kill9'})
        self.assertTrue(R.CYCLES['flap']['flap'] and R.CYCLES['resume-failure']['fail'] == 'wkr_fail')
        for role in ('coord', 'mgr_idle', 'mgr_busy', 'wkr', 'codex_wkr', 'never_orch', 'stopped', 'terminated'):
            self.assertIn(role, R.CYCLES['kill9']['roles'])

    def test_parse_rejects_unsafe_combinations(self):
        daemon = sys.executable
        for argv in (['--daemon', daemon, '--mode', 'real', '--simulate-resumer'], ['--daemon', daemon, '--cycles', 'nope'],
                     ['--daemon', daemon, '--pass-env', 'ANTHROPIC_API_KEY']):
            with self.subTest(argv), self.assertRaises(SystemExit):
                R.parse(argv)

    def test_mechanics_and_simulated_runs_never_certify(self):
        gates = {'x': {'passed': True}}
        results = [{'cycle': 'kill9', 'gates': gates, 'safety': {}, 'errors': []}]
        with tempfile.TemporaryDirectory() as tmp:
            for extra, mode in (([], 'mechanics'), (['--simulate-resumer'], 'mechanics')):
                args = R.parse(['--daemon', sys.executable, '--mode', mode, '--output', tmp, *extra])
                code = R.emit(args, results, 0)
                self.assertEqual(code, 1)
                self.assertFalse(json.loads((Path(tmp) / 'result.json').read_text())['passed'])
            args = R.parse(['--daemon', sys.executable, '--mode', 'real', '--output', tmp])
            self.assertEqual(R.emit(args, results, 0), 0)
            results[0]['errors'] = ['boom']
            self.assertEqual(R.emit(args, results, 0), 1)


if __name__ == '__main__':
    unittest.main()
