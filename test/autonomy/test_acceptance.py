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


if __name__ == '__main__':
    unittest.main()
