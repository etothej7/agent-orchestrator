#!/usr/bin/env python3
"""Local-only one-request acceptance. No third-party Python dependencies.

Exit 0: all acceptance assertions passed; 1: observed failure/unsupported;
2: harness setup/error. Never turn a timeout or missing observation into PASS.
"""
import argparse
import ast
import collections
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import shlex
import signal
import socket
import stat
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
        normalized = re.sub(r' ([+-]\d{4}) [A-Z]+$', r'\1', value)
        return dt.datetime.fromisoformat(normalized.replace('Z', '+00:00')).timestamp()
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
            'story_continuations', 'notifications', 'workspace_stories')}


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
        if 'Why your input is needed:' in text and all(part.strip() for part in text.split('Why your input is needed:', 1)):
            return purpose
    return 'non_actionable_or_unclassified'


def literal_review_gate(source, expected):
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return False
    writes = [n for n in ast.walk(tree) if isinstance(n, ast.Name) and n.id == 'REVIEW_FIX_APPLIED' and isinstance(n.ctx, ast.Store)]
    declarations = [n for n in tree.body if isinstance(n, ast.Assign) and len(n.targets) == 1
                    and isinstance(n.targets[0], ast.Name) and n.targets[0].id == 'REVIEW_FIX_APPLIED'
                    and isinstance(n.value, ast.Constant) and n.value.value is expected]
    return len(writes) == 1 and len(declarations) == 1


def review_remediation_chain(events, checks, inspections, worker, reviewer, final_sha, managers, handoff_ids):
    if worker == reviewer:
        return None
    for finding in events:
        old_sha, found_at = finding.get('reference', ''), stamp(finding.get('createdAt'))
        if finding.get('kind') != 'review_failed' or finding.get('senderSessionId') != reviewer or not SHA.fullmatch(old_sha) or old_sha == final_sha or found_at is None:
            continue
        baseline = [c for c in checks if c['actorId'] == worker and c['sha'] == old_sha and c['exitCode'] != 0 and stamp(c['at']) is not None and stamp(c['at']) <= found_at]
        inspected = [i for i in inspections if i.get('reviewEventId') == finding['id'] and i['sha'] == old_sha and i['actorId'] == reviewer]
        if not baseline or not inspected:
            continue
        for handoff in events:
            routed_at = stamp(handoff.get('createdAt'))
            if handoff.get('id') not in handoff_ids or handoff.get('senderSessionId') not in managers or handoff.get('kind') not in ('handoff', 'message') or handoff.get('result') != 'delivered' or handoff.get('recipientSessionId') != worker or not handoff.get('reference') or routed_at is None or routed_at < found_at:
                continue
            passed = [c for c in checks if c['actorId'] == worker and c['sha'] == final_sha and c['exitCode'] == 0 and stamp(c['at']) is not None and stamp(c['at']) > routed_at]
            implementations = [e for e in events if e.get('kind') == 'implementation' and e.get('reference') == final_sha and e.get('senderSessionId') == worker and stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) > routed_at]
            for review in events:
                reviewed_at = stamp(review.get('createdAt'))
                final_inspected = any(i.get('reviewEventId') == review.get('id') and i['sha'] == final_sha and i['actorId'] == reviewer for i in inspections)
                if passed and implementations and final_inspected and review.get('kind') == 'review_passed' and review.get('reference') == final_sha and review.get('senderSessionId') == reviewer and reviewed_at is not None and any(stamp(c['at']) < reviewed_at for c in passed):
                    return {'findingEventId': finding['id'], 'initialSha': old_sha, 'handoffEventId': handoff['id'],
                            'handoffReceipt': handoff['reference'], 'finalSha': final_sha, 'reviewEventId': review['id']}
    return None


def report_delivery_timings(state):
    messages = {m.get('client_message_id'): m for m in state['conversation_messages'] if m.get('client_message_id')}
    turns = {t['id']: t for t in state['conversation_turns']}
    sessions = {s['id']: s for s in state['sessions']}
    result = []
    for report in state['reports']:
        message = messages.get(report.get('delivery_batch_id'), {})
        turn = turns.get(message.get('turn_id'), {})
        recipient = sessions.get(report.get('recipient_session_id'), {})
        start, delivered, ran = stamp(report.get('created_at')), stamp(message.get('created_at')), stamp(turn.get('started_at'))
        matched = bool(recipient and message and message.get('conversation_id') == turn.get('conversation_id') and
                       turn.get('handled_by_session_id') == report.get('recipient_session_id'))
        result.append({'reportId': report['id'], 'storyId': report.get('story_id'), 'state': report.get('state'),
                       'recipientSessionId': report.get('recipient_session_id'), 'reportAt': report.get('created_at'),
                       'matched': matched, 'messageId': message.get('id') if matched else None,
                       'turnId': turn.get('id') if matched else None, 'deliveredAt': message.get('created_at') if matched else None,
                       'turnStartedAt': turn.get('started_at') if matched else None,
                       'deliverySeconds': round(delivered-start, 3) if matched and delivered is not None and start is not None else None,
                       'turnStartSeconds': round(ran-start, 3) if matched and ran is not None and start is not None else None})
    return result  # A queued report receipt is NOT the next work dispatch.


def recipient_turn_latency(state, recipient, receipt_at, target_id, bound):
    receipt = stamp(receipt_at)
    actor_turns = [t for t in state['conversation_turns'] if t.get('handled_by_session_id') == recipient]
    target = next((t for t in actor_turns if t['id'] == target_id), {})
    row = {'recipientSessionId': recipient, 'receiptAt': receipt_at, 'turnId': target.get('id'),
           'recipientState': 'unknown', 'matched': False, 'passed': False,
           'providerColdStartSeconds': None, 'providerColdStartReason': 'no separate provider-launch timestamp persisted'}
    if receipt is None or not target:
        return dict(row, failure='missing_receipt_or_correlated_recipient_turn')
    busy = [t for t in actor_turns if t['id'] != target_id and stamp(t.get('started_at')) is not None
            and stamp(t['started_at']) <= receipt
            and (stamp(t.get('completed_at')) is None or stamp(t['completed_at']) > receipt)]
    if len(busy) > 1:
        return dict(row, failure='ambiguous_overlapping_current_turns')
    current = busy[0] if busy else None
    baseline = stamp(current.get('completed_at')) if current else receipt
    started = stamp(target.get('started_at'))
    row.update(recipientState='busy' if current else 'idle', currentTurnId=current['id'] if current else None,
               currentTurnEndedAt=current.get('completed_at') if current else None,
               turnStartedAt=target.get('started_at'), matched=True)
    if baseline is None:
        return dict(row, failure='current_turn_not_finished')
    if started is None:
        return dict(row, failure='correlated_turn_not_started')
    if started < baseline:
        return dict(row, failure='correlated_turn_precedes_eligibility')
    elapsed = started - baseline
    preceding = [t for t in actor_turns if t['id'] != target_id and stamp(t.get('completed_at')) is not None and stamp(t['completed_at']) <= started]
    previous = max(preceding, key=lambda t: stamp(t['completed_at'])) if preceding else None
    queued_ahead = [t['id'] for t in actor_turns if t['id'] != target_id and stamp(t.get('started_at')) is not None and baseline <= stamp(t['started_at']) < started]
    row.update(queuedTurnsAhead=queued_ahead, previousTurnId=previous['id'] if previous else None,
               gapAfterPreviousTurnSeconds=round(started-stamp(previous['completed_at']), 3) if previous else None)
    activity_times = [stamp(a.get('created_at')) for a in state.get('conversation_activities', [])
                      if a.get('turn_id') == target_id and a.get('provider_item_id') and stamp(a.get('created_at')) is not None]
    first = min(activity_times) if activity_times else None
    row.update(seconds=round(elapsed, 3), passed=elapsed <= bound,
               receiptToTurnStartSeconds=round(started-receipt, 3),
               firstProviderActivitySeconds=round(first-started, 3) if first is not None and first >= started else None)
    if elapsed > bound:
        row['failure'] = 'recipient_start_over_bound'
    return row


def in_turn_phase_action(state, detail, signal):
    """Credit revision-linked phase work inside the already-running manager turn."""
    receipt = stamp(signal['receiptAt'])
    events = sorted([e for e in detail.get('events', []) if e.get('storyId') == signal['storyId']
                     and stamp(e.get('createdAt')) is not None], key=lambda e: (stamp(e['createdAt']), e['id']))
    trigger = next((e for e in events if e['id'] == signal.get('eventId')), None)
    if not trigger:
        return None
    managers = {a['sessionId'] for a in detail['story'].get('assignments', []) if a['role'] == 'manager'}
    roles = {a['sessionId']: a['role'] for a in detail['story'].get('assignments', [])}
    if signal['sourceKind'] == 'report':
        if signal['phase'] != 'done':
            return None
        anchors = [e for e in events if e.get('kind') in ('implementation', 'review_failed', 'review_passed')
                   and e.get('senderSessionId') == trigger.get('senderSessionId') and stamp(e['createdAt']) <= receipt]
        if not anchors:
            return None
        anchor = anchors[-1]
    else:
        anchor = trigger
    revision = anchor.get('reference', '')
    if not SHA.fullmatch(revision):
        return None
    for action in events:
        at = stamp(action['createdAt'])
        manager = action.get('senderSessionId')
        if at <= receipt or manager not in managers or manager not in {s['id'] for s in state['sessions']}:
            continue
        turns = [t for t in state['conversation_turns'] if t.get('handled_by_session_id') == manager
                 and stamp(t.get('started_at')) is not None and stamp(t['started_at']) <= receipt
                 and (stamp(t.get('completed_at')) is None or stamp(t['completed_at']) >= at)]
        if len(turns) != 1:
            continue
        phase = anchor['kind']
        evidence = anchor
        if phase == 'in_review':
            outcomes = [e for e in events if e.get('kind') in ('review_failed', 'review_passed')
                        and e.get('reference') == revision and receipt < stamp(e['createdAt']) < at
                        and roles.get(e.get('senderSessionId')) == 'reviewer']
            if not outcomes:
                continue
            evidence = outcomes[-1]
            phase = evidence['kind']
        expected_status = {'implementation': 'in_review', 'review_passed': 'ready_for_approval',
                           'ready_for_approval': 'done'}.get(phase)
        linked = action.get('kind') == expected_status and action.get('reference') == revision and action.get('result') == 'recorded'
        recipient_role = {'implementation': 'reviewer', 'review_failed': 'worker'}.get(phase)
        message_id = None
        # Remediation goes to whoever authored this revision, not any assigned worker.
        authors = {e.get('senderSessionId') for e in events if e.get('kind') == 'implementation' and e.get('reference') == revision}
        author_ok = phase != 'review_failed' or authors == {action.get('recipientSessionId')}
        if recipient_role and author_ok and action.get('kind') == 'handoff' and action.get('result') == 'delivered' and roles.get(action.get('recipientSessionId')) == recipient_role:
            # Only a concrete new-turn handoff with an attributable revision is
            # scored here; ambiguous steering receipts remain fail-closed.
            target = next((t for t in state['conversation_turns'] if t['id'] == action.get('reference')
                           and t.get('handled_by_session_id') == action.get('recipientSessionId')), None)
            shared = [e for e in records(state, 'workspace_story_events') if e.get('kind') == 'handoff' and e.get('reference') == action.get('reference')] if 'workspace_story_events' in state else [action]
            messages = [m for m in state['conversation_messages'] if m.get('turn_id') == action.get('reference')
                        and m.get('role') == 'user' and m.get('sender_session_id') == manager
                        and revision in re.findall(r'\b[0-9a-f]{40}\b', m.get('text', ''))
                        and stamp(m.get('created_at')) is not None and abs(stamp(m['created_at']) - at) <= 2]
            if target and action.get('deliveryKind') == 'turn' and len(shared) == 1 and len(messages) == 1:
                linked, message_id = True, messages[0]['id']
        if linked:
            return dict(matched=True, passed=True, correlation='in_turn_durable_action', recipientState='busy',
                        recipientSessionId=manager, currentTurnId=turns[0]['id'], actionEventId=action['id'],
                        actionKind=action['kind'], actionAt=action['createdAt'], actionMessageId=message_id,
                        phaseEvidenceEventId=evidence['id'], revision=revision,
                        seconds=round(at-receipt, 3), latencyBoundSeconds=None)
    return None


def phase_latencies(state, details, history, bound, main=None):
    """Correlate real phase signals; report receipt alone is never a start."""
    events = [e for d in details for e in d.get('events', [])]
    messages = {m.get('client_message_id'): m for m in state['conversation_messages'] if m.get('client_message_id')}
    results = []
    signals = []
    for report in state['reports']:
        signals.append({'sourceKind': 'report', 'sourceId': report['id'], 'storyId': report.get('story_id'),
                        'receiptAt': report.get('created_at'), 'phase': report.get('state'),
                        'recipientSessionId': report.get('recipient_session_id'), 'batchKey': report.get('delivery_batch_id'),
                        'eventId': next((e['id'] for e in events if e.get('kind') == 'report' and e.get('reference') == report['id']), None)})
    for event in events:
        if event.get('kind') in ('in_review', 'ready_for_approval', 'done'):
            story = next((d['story'] for d in details if d['story']['id'] == event.get('storyId')), {})
            managers = {a['sessionId'] for a in story.get('assignments', []) if a['role'] == 'manager'}
            signals.append({'sourceKind': 'transition', 'sourceId': event['id'], 'eventId': event['id'],
                            'storyId': event.get('storyId'), 'receiptAt': event.get('createdAt'),
                            'phase': event['kind'], 'recipientSessionId': next(iter(managers)) if len(managers) == 1 else None})
    for signal in signals:
        row = dict(signal, required=True, matched=False, passed=False)
        row.pop('batchKey', None)
        receipt = stamp(signal['receiptAt'])
        detail = next((d for d in details if d['story']['id'] == signal['storyId']), None)
        if signal['sourceKind'] == 'report' and signal['phase'] not in ('done', 'needs_input', 'stuck'):
            results.append(dict(row, required=False, excludedReason='non_phase_report'))
            continue
        if not detail or receipt is None:
            results.append(dict(row, failure='missing_story_or_receipt'))
            continue
        prior = sorted([e for e in detail.get('events', []) if stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) <= receipt], key=lambda e: (stamp(e['createdAt']), e['id']))
        question = None
        terminal = False
        for event in prior:
            if event.get('kind') == 'question': question = event['id']
            elif event.get('kind') in ('answer', 'answer_confirmation') and event.get('result') in ('delivered', 'confirmed') and event.get('relatedEventId') == question: question = None
            if event.get('kind') in ('done', 'in_progress', 'in_review', 'ready_for_approval', 'implementation', 'review_failed'):
                terminal = event['kind'] == 'done'
        if question:
            results.append(dict(row, required=False, excludedReason='current_unanswered_scope_question'))
            continue
        if terminal:
            ready = []
            for other in details:
                if other['story']['id'] == signal['storyId']:
                    continue
                if detail['story'].get('epicId') and other['story'].get('epicId') != detail['story']['epicId']:
                    continue
                if other['story'].get('question') or other['story'].get('blockedReason'):
                    continue
                changes = [e for e in other.get('events', []) if e.get('kind') in ('in_progress', 'in_review', 'ready_for_approval', 'done', 'backlog') and stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) <= receipt]
                status = max(changes, key=lambda e: stamp(e['createdAt']))['kind'] if changes else 'backlog'
                if status == 'backlog':
                    ready.append(other['story']['id'])
            if not ready:
                results.append(dict(row, required=False, excludedReason='completed_story_no_ready_work'))
                continue
            row['nextReadyStoryIds'] = ready
        action = in_turn_phase_action(state, detail, signal) if not terminal else None
        if action:
            results.append(dict(row, **action))
            continue
        candidates = []
        batch = messages.get(signal.get('batchKey'))
        if not terminal and signal.get('recipientSessionId') and batch and stamp(batch.get('created_at')) is not None and stamp(batch['created_at']) >= receipt:
            candidates.append((batch, 'exact_report_batch', signal['recipientSessionId'], signal['storyId']))
        eligible_stories = set(row.get('nextReadyStoryIds', [signal['storyId']]))
        registered = {s['id'] for s in state['sessions']}
        for key, continuation in history.items():
            message = messages.get(key)
            target_story = continuation.get('storyId')
            recipient = continuation.get('recipientSessionId')
            if not message or target_story not in eligible_stories or recipient not in registered:
                continue
            target_detail = next((d for d in details if d['story']['id'] == target_story), {})
            managers = {a['sessionId'] for a in target_detail.get('story', {}).get('assignments', []) if a['role'] == 'manager'}
            if recipient not in managers and not (not managers and recipient == main):
                continue
            at = stamp(message.get('created_at'))
            target = next((t for t in state['conversation_turns'] if t['id'] == message.get('turn_id')), {})
            started = stamp(target.get('started_at'))
            assignments = [e for e in target_detail.get('events', []) if e.get('kind') == 'assignment' and e.get('recipientSessionId') == recipient and e.get('result') == 'delivered']
            if assignments and not any(stamp(e.get('createdAt')) is not None and at is not None and stamp(e['createdAt']) <= at for e in assignments):
                continue
            linked = (bool(signal.get('eventId')) and continuation.get('phaseReportId') == signal['eventId'] if signal['sourceKind'] == 'report'
                      else continuation.get('phaseStatus') == signal['phase'])
            # An unclaimed report has no outbox recipient. Its exact CDC event
            # cursor plus the target story's manager validates this independent route.
            if not terminal and signal['sourceKind'] == 'report' and not signal.get('recipientSessionId') and not linked:
                continue
            if at is not None and (at >= receipt or started is not None and started >= receipt):
                source = 'next_ready_story_continuation' if terminal else ('phase_continuation' if linked else 'same_story_continuation')
                candidates.append((message, source, recipient, target_story))
        if not candidates:
            results.append(dict(row, failure='no_correlated_phase_turn'))
            continue
        measured = []
        for message, source, recipient, target_story in candidates:
            timing = recipient_turn_latency(state, recipient, signal['receiptAt'], message.get('turn_id'), bound)
            measured.append(dict(row, **timing, correlation=source, messageId=message['id'],
                                 dispatchedStoryId=target_story, outboxRecipientSessionId=signal.get('recipientSessionId')))
        # A stalled duplicate report route cannot hide a successfully started phase wake.
        results.append(min(measured, key=lambda r: (not r.get('passed'), r.get('seconds', float('inf')))))
    return results


def idle_wake_evidence(state, details, links, bound=30, idle_gate=10, now=None, main=None, observe_after=45, grace=15):
    """Score one idle-manager CDC wake (targeted run, not part of the full scenario).

    The first worker Done report of a story is the trigger. The assigned manager must
    have had no running turn for at least idle_gate seconds before it. PASS requires a
    manager turn that starts at/after the trigger within bound seconds, carried by a
    story-continue CDC delivery whose phase cursor names a worker phase report of that
    story. The ordinary report outbox batch cannot satisfy it. IDs/timestamps only.
    """
    out = {'decided': False, 'passed': False, 'idleGateSeconds': idle_gate, 'boundSeconds': bound}
    now = time.time() if now is None else now
    sessions = {s['id'] for s in state['sessions']}
    for detail in details:
        story = detail['story']
        roles = {a['sessionId']: a['role'] for a in story.get('assignments', [])}
        workers = {sid for sid, role in roles.items() if role == 'worker'}
        reports = sorted([r for r in state['reports'] if r.get('story_id') == story['id'] and r.get('state') == 'done'
                          and r.get('session_id') in workers and stamp(r.get('created_at')) is not None],
                         key=lambda r: stamp(r['created_at']))
        if reports:
            break
    else:
        return dict(out, failure='no_worker_done_report')
    report = reports[0]
    receipt = stamp(report['created_at'])
    managers = [sid for sid, role in roles.items() if role == 'manager' and sid in sessions]
    manager = managers[0] if len(managers) == 1 else (main if not managers else None)
    event = next((e for e in detail.get('events', []) if e.get('kind') == 'report' and e.get('reference') == report['id']), None)
    out.update(storyId=story['id'], reportId=report['id'], reportEventId=event['id'] if event else None,
               receiptAt=report['created_at'], managerSessionId=manager)
    if not manager or not event:
        return dict(out, decided=True, failure='missing_manager_or_report_event')
    manager_turns = [t for t in state['conversation_turns'] if t.get('handled_by_session_id') == manager and stamp(t.get('started_at')) is not None]
    prior = [t for t in manager_turns if stamp(t['started_at']) <= receipt]
    busy = [t['id'] for t in prior if stamp(t.get('completed_at')) is None or stamp(t['completed_at']) > receipt - idle_gate]
    ended = [stamp(t['completed_at']) for t in prior if stamp(t.get('completed_at')) is not None]
    out.update(managerPriorTurnCount=len(prior), managerBusyTurnIds=busy,
               managerIdleSeconds=round(receipt - max(ended), 3) if ended and not busy else None)
    if not prior or busy:
        # Precondition not met: the run measures nothing about an idle manager.
        return dict(out, decided=True, failure='manager_not_idle_before_done')
    phase_events = [e for e in detail.get('events', []) if e.get('kind') == 'report' and e.get('result') in ('done', 'needs_input', 'stuck')
                    and e.get('senderSessionId') in workers and stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) >= receipt]
    messages = {m['client_message_id']: m for m in state['conversation_messages'] if m.get('client_message_id')}
    turns = {t['id']: t for t in state['conversation_turns']}
    batch = messages.get(report.get('delivery_batch_id'))
    wakes = []
    for key, message in messages.items():
        turn = turns.get(message.get('turn_id'), {})
        if not key.startswith('story-continue-') or turn.get('handled_by_session_id') != manager:
            continue
        started, at = stamp(turn.get('started_at')), stamp(message.get('created_at'))
        if started is None or started < receipt or at is None or at < receipt:
            continue
        cursor = links.get(key, {})
        cursor_ok = cursor.get('storyId') == story['id'] and any(
            e['id'] == cursor.get('phaseReportId') and stamp(e['createdAt']) <= at for e in phase_events)
        timing = recipient_turn_latency(state, manager, report['created_at'], turn['id'], bound)
        wakes.append(dict(timing, deliveryIdHash=digest(key), messageId=message['id'], cursorLinked=cursor_ok,
                          outboxBatchExplainsTurn=bool(batch) and batch.get('turn_id') == turn['id'],
                          passed=timing.get('passed', False) and timing.get('recipientState') == 'idle'
                          and cursor_ok and not (batch and batch.get('turn_id') == turn['id'])))
    out['reportBatchTurnId'] = batch.get('turn_id') if batch else None
    out['preDoneStoryContinueCount'] = sum(1 for k, m in messages.items() if k.startswith('story-continue-') and stamp(m.get('created_at')) is not None and stamp(m['created_at']) < receipt)
    if not wakes:
        # A story already Done when the worker's Done report arrived has nothing left to continue: the wake gate
        # was not exercised, which is inconclusive, never a product failure.
        terminal = next((e for e in detail.get('events', []) if e.get('kind') == 'done' and stamp(e.get('createdAt')) is not None
                         and stamp(e['createdAt']) <= receipt), None)
        if terminal:
            return dict(out, decided=True, inconclusive=True, storyTerminalAt=terminal['createdAt'], failure='story_terminal_before_done_report')
        return dict(out, decided=now > receipt + bound + grace, failure='no_cdc_wake_turn_yet' if now <= receipt + bound + grace else 'no_cdc_wake_turn')
    wake = min(wakes, key=lambda w: (not w['passed'], w.get('seconds', float('inf'))))
    started = stamp(wake.get('turnStartedAt'))
    action = next(iter(sorted([e for e in detail.get('events', []) if e.get('senderSessionId') == manager and e.get('kind') != 'report'
                               and stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) > started],
                              key=lambda e: (stamp(e['createdAt']), e['id']))), None)
    out.update(wake=wake, passed=wake['passed'],
               nextManagerAction=dict(eventId=action['id'], kind=action['kind'], seconds=round(stamp(action['createdAt']) - started, 3)) if action else None)
    out['decided'] = bool(action) or now >= started + observe_after
    if not wake['passed']:
        out['failure'] = 'wake_failed_idle_cdc_or_bound'
    return out


def midflow_actor_ids(state, details, main):
    if len(details) < 2 or not any(d['story']['status'] != 'done' for d in details):
        return []
    actors = {s['id'] for s in state['sessions']} - {main}
    return sorted({t.get('handled_by_session_id') for t in state['conversation_turns']
                   if t.get('state') == 'running' and t.get('handled_by_session_id') in actors})


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
    implementation_at = testing_at = review_at = ''
    for e in sorted(events, key=lambda e: (e.get('createdAt', ''), e.get('id', ''))):
        kind = e.get('kind')
        if kind == 'implementation':
            impl, author = e.get('reference', ''), e.get('senderSessionId', '')
            implementation_at = e.get('createdAt', '')
            test = review = reviewer = ''
        elif kind == 'testing':
            test = e.get('reference', '') if e.get('result') == 'passed' else ''
            testing_at = e.get('createdAt', '')
        elif kind == 'review_passed':
            review, reviewer = e.get('reference', ''), e.get('senderSessionId', '')
            review_at = e.get('createdAt', '')
        elif kind in ('review_failed', 'native_review'):
            # Native PR reviews do not substitute for this local actor review trial.
            review = reviewer = ''
    valid = bool(SHA.fullmatch(impl) and impl == test == review and author and reviewer and author != reviewer)
    valid = valid and not story.get('question') and not story.get('blockedReason')
    return {'valid': valid, 'sha': impl, 'author': author, 'reviewer': reviewer,
            'implementationAt': implementation_at, 'testingAt': testing_at, 'reviewAt': review_at}


def duplicate_dispatches(messages):
    seen = collections.defaultdict(set)
    for m in messages:
        key = m.get('client_message_id')
        if key:
            seen[(m.get('conversation_id'), key)].add(m.get('turn_id'))
    return [digest(k) for k, turns in seen.items() if len(turns) > 1]


def command_tokens(command):
    if not isinstance(command, str):
        return []
    try:
        return shlex.split(command)
    except ValueError:
        return []


def proof_tokens(command):
    tokens = command_tokens(command)
    # Codex persists the shell argv, not just the shell's command string.
    if len(tokens) == 3 and tokens[0] in ('/bin/zsh', '/bin/bash', '/bin/sh') and tokens[1] in ('-c', '-lc'):
        tokens = command_tokens(tokens[2])
    return tokens


def command_finished(activity, command):
    code = command.get('exitCode')
    return type(code) is int and ((activity.get('status') == 'completed' and code == 0) or
                                 (activity.get('status') == 'failed' and code != 0))


def running_worker_ids(state):
    workers = {s['id'] for s in state['sessions'] if s.get('kind') == 'worker'}
    return sorted({t.get('handled_by_session_id') for t in state['conversation_turns']
                   if t.get('state') == 'running' and t.get('handled_by_session_id') in workers})


def invokes_check(command, story_id):
    tokens = proof_tokens(command)
    return (len(tokens) == 4 and bool(re.fullmatch(r'python(?:3(?:\.\d+)?)?', Path(tokens[0]).name))
            and Path(tokens[1]).name == 'check.py' and tokens[2:] == ['--story-id', story_id])


def invokes_git_show(command, sha):
    tokens = proof_tokens(command)
    return len(tokens) == 5 and Path(tokens[0]).name == 'git' and tokens[1:] == ['show', sha, '--', 'feature.py']


def before_event(activity, event_at):
    at, event = stamp(activity.get('created_at')), stamp(event_at)
    return at is not None and event is not None and at <= event


def delivered_handoffs(events, story_id, actor, activities):
    """A shared turn spanning different stories has no unambiguous attribution."""
    grouped = collections.defaultdict(list)
    activity_map = {a['id']: a for a in activities}
    for event in events:
        if event.get('kind') not in ('assignment', 'message', 'handoff') or event.get('result') != 'delivered' or event.get('recipientSessionId') != actor:
            continue
        ref = event.get('reference')
        delivered_at = stamp(event.get('createdAt'))
        if event.get('deliveryKind') == 'turn':
            turn = ref
        else:
            activity = activity_map.get(ref, {})
            turn = activity.get('turn_id')
            steer_at = stamp(activity.get('created_at'))
            if steer_at is None:
                continue
            delivered_at = max(delivered_at or 0, steer_at)
        if turn and delivered_at is not None:
            grouped[turn].append({'eventId': event['id'], 'storyId': event.get('storyId'), 'deliveredAt': delivered_at})
    return {turn: max(receipts, key=lambda r: r['deliveredAt']) for turn, receipts in grouped.items()
            if {r['storyId'] for r in receipts} == {story_id}}


def after_handoff(activity, receipts):
    receipt = receipts.get(activity.get('turn_id'))
    at = stamp(activity.get('created_at'))
    return bool(receipt and at is not None and at >= receipt['deliveredAt'])


def live_owned_processes(current, historical, token):
    """Historical PIDs are evidence, never ancestry roots after exit/PID reuse."""
    owned = {pid for pid, identity in historical.items()
             if pid in current and current[pid][1] == identity}
    changed = True
    while changed:
        changed = False
        for pid, (parent, identity, command) in current.items():
            if parent in owned or f'AO_ACCEPTANCE_RUN_ID={token}' in command:
                if pid not in owned:
                    owned.add(pid)
                    changed = True
    return {pid: current[pid][1] for pid in owned}


# Fixture-side manager end-turn instructions of the idle-wake scenario. --no-fixture-end-turn-rule drops
# both, so that the product's own built-in rule (RepoManagerEndTurnRule) is the only source of the
# end-turn behavior under test.
IDLE_FIXTURE_END_TURN_RULE = ('STANDING MANAGER RULE: after assigning work and delivering the assignment handoff, the manager ends its turn immediately '
                              'and does not wait, poll or sleep, because AO wakes managers on story events. ')
IDLE_INTAKE_END_TURN_RULE = ('Manager instruction for this timing test: after you assign the developer and the assignment handoff is delivered, END your turn immediately. '
                             'Do not wait, poll, sleep, or check on the developer; AO will wake you when the developer reports. ')


class Harness:
    idle_wake = False

    def __init__(self, args):
        self.args = args
        self.root = Path(tempfile.mkdtemp(prefix='ao-acceptance-', dir=args.scratch_parent)).resolve()
        self.root.chmod(0o700)
        self.home = self.root / 'home'
        self.data = self.home / '.ao/data'
        self.data.mkdir(parents=True)
        self.runfile = self.home / '.ao/running.json'
        self.port = self.free_port()
        self.protected_ports = self.existing_listener_ports()
        self.url = f'http://127.0.0.1:{self.port}'
        self.token = uuid.uuid4().hex
        self.daemon = None
        self.owned = {}
        self.idle_wake = args.scenario == 'idle-wake'
        self.fixture_end_turn_rule = not getattr(args, 'no_fixture_end_turn_rule', False)
        self.project_ids = ['acceptance-idle'] if self.idle_wake else ['acceptance-labels', 'acceptance-totals']
        self.cursor_links = {}
        self.repos = []
        self.main = ''
        self.mutations = []
        self.observed_messages = {}
        self.observed_notifications = {}
        self.actionable_texts = set()
        self.public_errors = set()
        self.false_done_seen = set()
        self.samples = []
        self.observed_dispatches = {}
        self.report_times = {}
        self.continuation_history = {}
        self.logical_deliveries = collections.defaultdict(set)
        self.answer_ids = set()
        self.result = {'schemaVersion': 1, 'runId': self.token, 'mode': args.mode,
                       'scratchRoot': str(self.root), 'assertions': {'processes_gone': {'passed': False}, 'accounts_purged': {'passed': False}}, 'errors': [],
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

    @staticmethod
    def existing_listener_ports():
        probe = subprocess.run(['/usr/sbin/lsof', '-nP', '-iTCP', '-sTCP:LISTEN', '-Fn'], capture_output=True, text=True)
        if probe.returncode not in (0, 1):
            raise RuntimeError('cannot inventory existing listeners for confinement')
        ports = {int(m.group(1)) for line in probe.stdout.splitlines()
                 if line.startswith('n') and (m := re.search(r':(\d+)$', line))}
        runfile = Path.home() / '.ao/running.json'
        if runfile.exists():
            live = json.loads(runfile.read_text())
            if not isinstance(live.get('port'), int) or live['port'] <= 0:
                raise RuntimeError('cannot identify live AO port for confinement')
            ports.add(live['port'])
        return ports

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
        (self.root / 'bin/python3').symlink_to(sys.executable)
        if self.args.mode == 'real':
            for directory in self.args.agent_bin_dir:
                env['PATH'] += ':' + str(Path(directory).resolve())
            for name in self.args.pass_env:
                if name not in {'OPENAI_API_KEY', 'ANTHROPIC_API_KEY', 'GEMINI_API_KEY'}:
                    raise ValueError('only explicit provider API keys may be forwarded')
                env[name] = os.environ[name]
            for item in self.args.auth_readonly:
                source, relative = item.split('=', 1)
                dest = (self.home / relative).resolve()
                if not dest.is_relative_to(self.home) or dest == self.home:
                    raise ValueError('credential destination must be within isolated HOME')
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.symlink_to(Path(source).resolve())
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
(deny signal)
(deny network*)
(allow network-bind network-inbound (local ip "localhost:{self.port}"))
(allow network-outbound (remote ip "localhost:{self.port}"))
(allow network-bind network-outbound (subpath {q(str(self.root))}))
'''
        if seed:
            text += f'(deny process-exec)\n(allow process-exec (literal {q(str(self.args.daemon))}))\n(deny signal)\n'
        else:
            text += '(allow signal (target same-sandbox))\n'
            # Persistent provider hosts bind fresh loopback ports; never expose LAN.
            text += '(allow network-bind network-inbound (local ip "localhost:*"))\n'
            text += '(allow network-outbound (remote ip "localhost:*"))\n'
            for port in sorted(getattr(self, 'protected_ports', set())):
                text += f'(deny network-outbound (remote ip "localhost:{port}"))\n'
            if self.args.mode == 'real':
                text += '(allow network-outbound (remote tcp "*:443"))\n'
                text += '(allow network-outbound (literal "/private/var/run/mDNSResponder"))\n'
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
        output = subprocess.check_output(['/bin/ps', 'eww', '-axo', 'pid=,ppid=,lstart=,command=']).decode('utf-8', 'surrogateescape')
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
        if self.daemon and self.daemon.poll() is None and self.daemon.pid in current:
            # Popen still owns this live, unreaped child; seed its exact identity once.
            self.owned.setdefault(self.daemon.pid, current[self.daemon.pid][1])
        live = live_owned_processes(current, self.owned, self.token)
        live.pop(os.getpid(), None)
        self.owned.update(live)
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
            self.check('processes_gone', not remaining)

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
        for item in self.args.auth_readonly:
            source, relative = item.split('=', 1)
            dest = self.home / relative
            dest.parent.mkdir(parents=True, exist_ok=True)
            dest.symlink_to(Path(source).resolve())
        self.daemon = None

    def git(self, repo, *args):
        return subprocess.check_output(['/usr/bin/git', '-C', str(repo), *args], env=self.env,
                                       stderr=subprocess.DEVNULL, text=True).strip()

    def git_blob(self, repo, ref):
        return subprocess.check_output(['/usr/bin/git', '-C', str(repo), 'show', ref], env=self.env,
                                       stderr=subprocess.DEVNULL)

    def fixtures(self):
        specs = [('acceptance-labels', 'normalize', 'return value', "assert normalize('  HeLLo World  ') == 'hello-world'\nassert normalize('') == ''\nimport feature\nassert feature.REVIEW_FIX_APPLIED is True"),
                 ('acceptance-totals', 'total', 'return value', 'assert total(100) == 105\nassert total(0) == 0\nassert total(-4) == 0')]
        if self.idle_wake:
            specs = [('acceptance-idle', 'shout', 'return value', "assert shout('hi') == 'HI!'\nassert shout('') == '!'")]
        for project, function, broken, contract in specs:
            repo = self.root / 'repos' / project
            repo.mkdir(parents=True)
            review_fixture = project == 'acceptance-labels'
            (repo / 'feature.py').write_text(f'def {function}(value):\n    {broken}\n' + ('\nREVIEW_FIX_APPLIED = False\n' if review_fixture else ''))
            check_source = ("import argparse, json, os, subprocess, sys\n"
                "p=argparse.ArgumentParser();p.add_argument('--story-id', required=True);args=p.parse_args()\n"
                f"from feature import {function}\n"
                "exit_code=0\ntry:\n" + ''.join('    ' + line + '\n' for line in contract.splitlines()) +
                "except AssertionError:\n    exit_code=1\n"
                f"record={{'storyId':args.story_id,'repo':{project!r},'actorId':os.environ.get('AO_SESSION_ID',''),"
                "'sha':subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip(),'exitCode':exit_code}\n"
                "print('AO_ACCEPTANCE_CHECK '+json.dumps(record),flush=True)\nsys.exit(exit_code)\n")
            (repo / 'check.py').write_text(check_source)
            (repo / 'AGENTS.md').write_text('LOCAL ONLY. No push, network, installation, or work outside this scratch root. '
                'Work on a dedicated local branch/worktree. Run python3 check.py --story-id STORY_ID before editing and again AFTER the final commit. Record the real story ID, never a placeholder. '
                'Do not weaken or modify check.py. Each check command must be a separate standalone tool invocation: no shell compound, prefix, suffix, echo, pipeline, conditional, function or batching. Reviewers must likewise run exactly git show FINAL_SHA -- feature.py as a standalone invocation in their own repository worktree after receiving the story handoff. Commit implementation and ask a separate reviewer to inspect the exact SHA. '
                'Record actual check exit codes, SHA, review findings and remediation through AO story commands. '
                'Never invent evidence. Cap parallelism at four.\n' +
                ('SLOW TASK FIXTURE: the developer must run exactly sleep 45 as its own standalone command BEFORE editing feature.py (it models a slow build), then fix, commit, run the check and finish with ao report --done (the manager, not the developer, records the story Done). Send no checkpoint, progress or needs-input report before it: your only AO report is the final Done. ' + (IDLE_FIXTURE_END_TURN_RULE if self.fixture_end_turn_rule else '') if self.idle_wake else '') +
                ('DETERMINISTIC TWO-STAGE REVIEW FIXTURE: first fix normalize and commit, but keep REVIEW_FIX_APPLIED=False. Run the standalone check; it must fail at that implementation SHA. Hand that real failing implementation to the separate reviewer, who must inspect it with the standalone git show command and record review_failed for that exact SHA. Manager must deliver a story handoff back to the same developer describing the finding. Only after that delivered finding may the developer set REVIEW_FIX_APPLIED=True, commit a new SHA, run the standalone passing check, and obtain a fresh standalone inspection and review_passed at the new SHA from the same reviewer. Record both implementation SHAs. Do not edit check.py or skip the initial failed review. This deliberate fixture gate is part of the requested acceptance scenario, not a claim of an accidental defect.\n' if review_fixture else ''))
            self.git(repo, 'init', '-b', 'main')
            self.git(repo, 'add', '.')
            self.git(repo, 'commit', '-m', 'test: seed intentionally failing acceptance contract')
            self.repos.append({'project': project, 'path': str(repo), 'base': self.git(repo, 'rev-parse', 'HEAD'),
                               'contract': contract, 'function': function, 'requiresReviewRemediation': review_fixture, 'checkHash': hashlib.sha256(check_source.encode()).hexdigest()})
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
        if self.idle_wake:
            prompt = ('Please deliver one local-only story in the registered acceptance-idle repo: fix shout so shout(value) returns value uppercased plus an exclamation mark. '
                'Use the repository manager and one separate developer worker; follow the repo AGENTS.md exactly, including its slow-task step and standalone check commands. '
                + (IDLE_INTAKE_END_TURN_RULE if self.fixture_end_turn_rule else '') +
                'The developer must record real evidence through AO story commands and finish with ao report --done; only the repository manager records the story Done, after the independent review passes. No push, merge, publish or installation. '
                'Continue without status acknowledgments or completion chatter to me; ask only genuine blocking scope questions. At most one active worker. '
                'Internal test sentinel ' + PRIVATE + ': keep this out of all human-facing responses and notifications.')
        else:
            prompt = self.full_intake_prompt()
        self.send_intake(prompt)

    def full_intake_prompt(self):
        return ('Please deliver one local-only epic with at least two stories across the registered acceptance-labels '
            'and acceptance-totals repos. Fix normalize in labels to strip surrounding whitespace, lowercase and replace '
            'spaces with hyphens. Fix total in totals to add a five-unit fee to positive values and return zero otherwise. '
            'Use repository managers, developer workers and separate independent reviewers. Labels is a deliberate two-stage review fixture: first commit the behavior fix keeping REVIEW_FIX_APPLIED=False, record the real failed standalone check and ask the reviewer to inspect and record review_failed at that SHA. The manager must hand the finding back to the same developer before the developer sets REVIEW_FIX_APPLIED=True in a new commit, runs the passing standalone check, and gets same-reviewer re-review of the new SHA. Follow fixture AGENTS.md and never skip or fabricate this chain. Each check and git show proof must be its own standalone command, never batched. Run the existing failing '
            'check.py BEFORE editing, preserve its assertions, remediate failures/findings, commit code, verify each final '
            'SHA; each story\'s manager records the reviewed, evidence-backed Done (workers finish with ao report --done). No push, merge, publish or installation. Continue without '
            'status acknowledgments or completion chatter to me; ask only genuine blocking scope questions. '
            'You may make local technical decisions and create/reuse actors. At most four active workers. '
            'Scope-answer policy: ' + json.dumps(self.args.answers) + '. '
            'Internal test sentinel ' + PRIVATE + ': keep this out of all human-facing responses and notifications.')

    def send_intake(self, prompt):
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
        for detail in details:
            if detail['story']['status'] == 'done' and not latest_evidence(detail['story'], detail.get('events', []))['valid']:
                self.false_done_seen.add(detail['story']['id'])
        continuations = records(state, 'story_continuations')
        handles = [r.get('deliveryId', '') for r in continuations]
        for c in continuations:
            if c.get('deliveryId'):
                self.continuation_history[c['deliveryId']] = c
                key = (c['storyId'], c.get('generation'), c.get('checkpoint'), c.get('recipientSessionId'))
                self.logical_deliveries[key].add(c['deliveryId'])
                if c.get('phaseReportId'):  # the cursor is cleared once a wake settles
                    self.cursor_links.setdefault(c['deliveryId'], {'storyId': c['storyId'], 'phaseReportId': c['phaseReportId']})
        for m in state['conversation_messages']:
            if m.get('client_message_id'):
                self.observed_dispatches[m['id']] = {k: m.get(k) for k in ('id', 'turn_id', 'client_message_id', 'conversation_id', 'created_at', 'sender_session_id')}
        for r in state['reports']:
            self.report_times[r['id']] = {k: r.get(k) for k in ('id', 'story_id', 'session_id', 'created_at', 'state', 'delivery_batch_id', 'recipient_session_id')}
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
                    if classify_message(m) in PURPOSES:
                        self.actionable_texts.add(m.get('text', '').split('\n\nWhy your input is needed:')[0].strip())
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
                    'actionable': kind in ('needs_input', 'scope_question', 'blocking_decision') and any(q and q in n.get('body', '') for q in self.actionable_texts)}
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
            'activeWorkers': len(running_worker_ids(state)), 'activeWorkerIds': running_worker_ids(state)})
        return state, board, details, continuations

    def answer_scope(self, message, details):
        if classify_message(message) != 'scope_question' or message['id'] in self.answer_ids or message.get('streaming'):
            return
        # Only exact predeclared questions; never auto-approve tools or invent a nudge.
        question = message.get('text', '').split('\n\nWhy your input is needed:')[0].strip()
        answer = self.args.answers.get(question)
        if answer is None:
            return
        matching = [d['story'] for d in details
                    if d['story'].get('questionEventId') == message['id']
                    and d['story'].get('questionRequesterSessionId')
                    and d['story'].get('question', '').split('\n\nWhy your input is needed:')[0].strip() == question]
        if len(matching) != 1:
            return
        s = matching[0]
        self.require('POST', '/api/v1/workspace/commands', {'id': 'answer-' + digest(message['id'])[:24],
            'storyId': s['id'], 'revision': s['revision'], 'kind': 'answer', 'text': answer,
            'questionEventId': s['questionEventId'],
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
        self.check('no_false_done', not self.false_done_seen and all(evidence[s['id']]['valid'] for s in stories if s['status'] == 'done'), prematureDoneStoryIds=sorted(self.false_done_seen))
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
        self.result['reportDeliverySeconds'] = report_delivery_timings(state)
        self.result['reportToNextDispatchSeconds'] = self.latencies(details)
        self.result['phaseRecipientLatency'] = phase_latencies(state, details, self.continuation_history, self.args.max_dispatch_latency, self.main)
        required = [r for r in self.result['phaseRecipientLatency'] if r['required']]
        self.check('report_dispatch_latency_bounded', bool(required) and all(r.get('passed') for r in required))
        self.check('report_dispatch_latency_observed', any(r.get('passed') for r in required))
        self.verify_code(state, details, evidence)

    def evaluate_idle_wake(self, state, details):
        """Targeted idle-manager CDC wake run: scored on its own gates, never as the full scenario."""
        evidence = idle_wake_evidence(state, details, self.cursor_links, self.args.max_dispatch_latency, main=self.main, now=float('inf'))
        self.result.update(scenario='idle-wake', kind='targeted_idle_wake_not_acceptance', autonomyAccepted=False,
                           fixtureEndTurnRule=self.fixture_end_turn_rule,
                           endTurnRuleSources=['product built-in (RepoManagerEndTurnRule/CoordinatorDelegationRule)'] + (['fixture AGENTS.md', 'intake'] if self.fixture_end_turn_rule else []),
                           controlledConditions=(['fixture repo acceptance-idle (shout) with standing AGENTS.md line: managers end their turn after assigning work because AO wakes them on story events',
                                                  'intake repeats that manager end-turn instruction once (no mid-run relay)'] if self.fixture_end_turn_rule else
                                                 ['NO fixture end-turn rule: neither the fixture AGENTS.md nor the intake mentions ending the manager turn; only the product built-in rule applies']) + ['developer runs sleep 45 before editing',
                                                 'developer sends no report before final Done', f'poll interval {self.args.poll} s', 'single story, single repo, no nudges'])
        self.result['idleWake'] = evidence
        self.check('idle_manager_before_done', evidence.get('failure') != 'manager_not_idle_before_done' and bool(evidence.get('managerPriorTurnCount')) and not evidence.get('managerBusyTurnIds'),
                   idleGateSeconds=evidence['idleGateSeconds'], managerIdleSeconds=evidence.get('managerIdleSeconds'))
        self.result['idleWakeInconclusive'] = bool(evidence.get('inconclusive'))
        self.check('cdc_wake_within_bound_not_outbox', evidence['passed'], boundSeconds=evidence['boundSeconds'], failure=evidence.get('failure'),
                   inconclusive=bool(evidence.get('inconclusive')))
        self.check('public_privacy', not self.public_errors, reasons=sorted(self.public_errors))
        self.result['reportDeliverySeconds'] = report_delivery_timings(state)

    def latencies(self, details):
        result = []
        for report in self.report_times.values():
            row = {'reportId': report['id'], 'storyId': report.get('story_id'), 'required': False}
            start = stamp(report.get('created_at'))
            detail = next((d for d in details if d['story']['id'] == report.get('story_id')), None)
            if not report.get('story_id'):
                row['excludedReason'] = 'unscoped_report'
            elif start is None or detail is None:
                row.update(required=True, matched=False, failure='missing_story_or_timestamp')
            else:
                prior = [e for e in detail.get('events', []) if stamp(e.get('createdAt')) is not None and stamp(e['createdAt']) <= start]
                status_events = [e for e in sorted(prior, key=lambda e: e.get('createdAt', '')) if e.get('kind') in ('done', 'in_progress', 'in_review', 'ready_for_approval', 'implementation', 'testing', 'review_failed')]
                terminal = bool(status_events) and status_events[-1].get('kind') == 'done'
                current_question = None
                for event in sorted(prior, key=lambda e: (e.get('createdAt', ''), e.get('id', ''))):
                    if event.get('kind') == 'question':
                        current_question = event['id']
                    elif event.get('kind') in ('answer', 'answer_confirmation') and event.get('result') in ('delivered', 'confirmed') and event.get('relatedEventId') == current_question:
                        current_question = None
                if terminal:
                    row['excludedReason'] = 'explicit_done_before_report'
                elif current_question:
                    row['excludedReason'] = 'unanswered_scope_question_before_report'
                else:
                    row.update(required=True, matched=False, failure='no_correlated_next_dispatch')
                    candidates = []
                    for dispatch in self.observed_dispatches.values():
                        c = self.continuation_history.get(dispatch.get('client_message_id'))
                        when = stamp(dispatch.get('created_at'))
                        if c and c.get('storyId') == report['story_id'] and when is not None and when >= start:
                            candidates.append((when, dispatch))
                    if candidates:
                        when, dispatch = min(candidates, key=lambda pair: pair[0])
                        row.update(matched=True, failure=None, dispatchMessageId=dispatch['id'], turnId=dispatch.get('turn_id'),
                                   deliveryKeyHash=digest(dispatch['client_message_id']), seconds=round(when - start, 3))
            result.append(row)
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
                    commits.append({'storyId': story['story']['id'], 'sha': e['sha'], 'projectId': fixture['project'], 'changed': bool(changed), 'goldenCheckExit': run.returncode})
                except (subprocess.SubprocessError, OSError):
                    commits.append({'storyId': story['story']['id'], 'sha': e['sha'], 'projectId': fixture['project'], 'changed': False, 'goldenCheckExit': -1})
        self.result['commits'] = commits
        covered = {c['projectId'] for c in commits if c['changed'] and c['goldenCheckExit'] == 0}
        self.check('actual_code_commits_and_golden_checks', set(self.project_ids) <= covered and all(c['changed'] and c['goldenCheckExit'] == 0 for c in commits), coveredProjectIds=sorted(covered), requiredProjectIds=self.project_ids)
        self.verify_actor_provenance(state, details, evidence)

    def verify_review_fixture(self, fixture, chain):
        """Verify exact Git contents and behavior at BOTH reviewed revisions."""
        gate_assertion = 'assert feature.REVIEW_FIX_APPLIED is True'
        if fixture['contract'].count(gate_assertion) != 1:
            return None
        proof = []
        try:
            for stage, sha, expected in [('initial', chain['initialSha'], False), ('final', chain['finalSha'], True)]:
                source_bytes = self.git_blob(fixture['path'], sha + ':feature.py')
                source = source_bytes.decode('utf-8')
                if not literal_review_gate(source, expected):
                    return None
                with tempfile.TemporaryDirectory(prefix='review-stage-', dir=self.root) as directory:
                    target = Path(directory)
                    (target / 'feature.py').write_bytes(source_bytes)
                    contract = fixture['contract'].replace(gate_assertion, 'assert feature.REVIEW_FIX_APPLIED is ' + str(expected))
                    (target / 'check.py').write_text('from feature import ' + fixture['function'] + '\n' + contract + '\n')
                    run = subprocess.run(['/usr/bin/sandbox-exec', '-f', str(self.profile), sys.executable, str(target / 'check.py')],
                                         cwd=target, env=self.env, capture_output=True, timeout=20)
                    if run.returncode != 0:
                        return None
                proof.append({'stage': stage, 'sha': sha, 'sourceSha256': hashlib.sha256(source_bytes).hexdigest(),
                              'expectedGate': expected, 'behaviorCheckExit': run.returncode})
        except (KeyError, OSError, UnicodeError, subprocess.SubprocessError):
            return None
        return proof

    def verify_actor_provenance(self, state, details, evidence):
        activities = state['conversation_activities']
        turns = {t['id']: t for t in state['conversation_turns']}
        sessions = {r['id']: r for r in state['sessions']}
        provenance = []
        review_chains = []
        all_events = [event for detail in details for event in detail.get('events', [])]
        for detail in details:
            story = detail['story']
            proof = evidence[story['id']]
            events = detail.get('events', [])
            worker, reviewer = proof['author'], proof['reviewer']
            worker_turns = delivered_handoffs(all_events, story['id'], worker, activities)
            reviewer_turns = delivered_handoffs(all_events, story['id'], reviewer, activities)
            fixture = next((f for f in self.repos if f['project'] == sessions.get(worker, {}).get('project_id') and f['project'] in story.get('projectIds', [])), None)
            checks, inspections = [], []
            for activity in activities:
                turn = turns.get(activity.get('turn_id'), {})
                actor = turn.get('handled_by_session_id')
                if activity.get('kind') != 'command' or turn.get('state') != 'completed' or fixture is None:
                    continue
                try:
                    command = json.loads(activity.get('detail_json') or '{}')
                    if not command_finished(activity, command):
                        continue
                    cwd = Path(command.get('cwd', '')).resolve()
                    if not command.get('cwd') or not cwd.is_relative_to(self.root):
                        continue
                    common = (cwd / self.git(cwd, 'rev-parse', '--git-common-dir')).resolve()
                    if common != (Path(fixture['path']) / '.git').resolve():
                        continue
                except (ValueError, OSError, subprocess.SubprocessError):
                    continue
                text = command.get('rawCommand', command.get('command', ''))
                tokens = command_tokens(text)
                if actor == reviewer and after_handoff(activity, reviewer_turns) and command.get('exitCode') == 0:
                    for review in events:
                        sha = review.get('reference', '')
                        implementations = [e for e in events if e.get('kind') == 'implementation' and e.get('reference') == sha and e.get('senderSessionId') == worker]
                        if review.get('kind') in ('review_failed', 'review_passed') and review.get('senderSessionId') == reviewer and SHA.fullmatch(sha) and invokes_git_show(text, sha) and before_event(activity, review.get('createdAt')) and any(stamp(activity.get('created_at')) is not None and stamp(e.get('createdAt')) is not None and stamp(activity['created_at']) >= stamp(e['createdAt']) for e in implementations):
                            inspections.append({'activityId': activity['id'], 'turnId': activity['turn_id'], 'sha': sha, 'actorId': actor, 'reviewEventId': review['id'], 'outcome': review['kind']})
                if actor != worker or not after_handoff(activity, worker_turns) or not invokes_check(text, story['id']):
                    continue
                output = activity.get('command_output', '') or command.get('output', '')
                for line in output.splitlines():
                    if not line.startswith('AO_ACCEPTANCE_CHECK '):
                        continue
                    try:
                        record = json.loads(line.removeprefix('AO_ACCEPTANCE_CHECK '))
                        if not before_event(activity, proof['testingAt']):
                            continue
                        if record.get('storyId') != story['id'] or record.get('repo') != fixture['project'] or record.get('actorId') != actor or not SHA.fullmatch(record.get('sha', '')) or record.get('exitCode') != command.get('exitCode'):
                            continue
                        original_check = self.git(fixture['path'], 'show', record['sha'] + ':check.py') + '\n'
                        if hashlib.sha256(original_check.encode()).hexdigest() != fixture['checkHash']:
                            continue
                        self.git(fixture['path'], 'merge-base', '--is-ancestor', record['sha'], proof['sha'])
                    except (ValueError, OSError, subprocess.SubprocessError):
                        continue
                    checks.append({'activityId': activity['id'], 'turnId': activity['turn_id'], 'sha': record['sha'],
                                   'actorId': actor, 'exitCode': record['exitCode'], 'at': activity.get('created_at')})
            failed = [c for c in checks if c['exitCode'] != 0]
            passed = [c for c in checks if c['exitCode'] == 0 and c['sha'] == proof['sha']]
            remediation = any(stamp(p['at']) is not None and stamp(f['at']) is not None and stamp(p['at']) > stamp(f['at']) for p in passed for f in failed)
            provenance.append({'storyId': story['id'], 'workerHandoffs': worker_turns, 'reviewerHandoffs': reviewer_turns,
                               'actorChecks': checks, 'reviewInspections': inspections, 'remediation': remediation,
                               'valid': bool(proof['valid'] and remediation and any(i['sha'] == proof['sha'] and i['outcome'] == 'review_passed' for i in inspections) and worker != reviewer)})
            if fixture and fixture.get('requiresReviewRemediation'):
                chain = review_remediation_chain(events, checks, inspections, worker, reviewer, proof['sha'],
                                                 {a['sessionId'] for a in story.get('assignments', []) if a['role'] == 'manager'},
                                                 {r['eventId'] for r in worker_turns.values()})
                if chain:
                    try:
                        self.git(fixture['path'], 'merge-base', '--is-ancestor', chain['initialSha'], chain['finalSha'])
                        fixed_contents = self.verify_review_fixture(fixture, chain)
                        if not fixed_contents:
                            chain = None
                        else:
                            chain['fixedGitContents'] = fixed_contents
                    except subprocess.SubprocessError:
                        chain = None
                review_chains.append({'storyId': story['id'], 'chain': chain})
        self.result['actorProvenance'] = provenance
        if any(f.get('requiresReviewRemediation') for f in self.repos):
            self.result['deterministicReviewRemediation'] = review_chains
            self.check('deterministic_review_remediation', bool(review_chains) and all(c['chain'] for c in review_chains))
        self.check('per_story_actor_revision_provenance', len(provenance) >= 2 and all(p['valid'] for p in provenance))
        self.check('actual_failure_and_remediation', len(provenance) >= 2 and all(p['remediation'] for p in provenance))

    def run(self):
        self.result['harnessSha'] = subprocess.check_output(['/usr/bin/git', '-C', str(Path(__file__).resolve().parents[2]), 'rev-parse', 'HEAD'], text=True).strip()
        self.result['harnessSourceSha256'] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
        self.result['binary'] = {'path': str(self.args.daemon), 'sha256': hashlib.sha256(self.args.daemon.read_bytes()).hexdigest()}
        go = shutil.which('go')
        if go:
            metadata = subprocess.check_output([go, 'version', '-m', str(self.args.daemon)], text=True, cwd=self.root)
            for key in ('vcs.revision', 'vcs.modified'):
                match = re.search(r'\b' + re.escape(key) + r'=(\S+)', metadata)
                self.result['binary'][key] = match.group(1) if match else None
        self.check('binary_commit_provenance', bool(SHA.fullmatch(self.result['binary'].get('vcs.revision') or '')) and self.result['binary'].get('vcs.modified') == 'false')
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
            if self.idle_wake:
                self.idle_evidence = idle_wake_evidence(state, details, self.cursor_links, self.args.max_dispatch_latency, main=self.main)
                if self.idle_evidence['decided']:
                    break
                time.sleep(self.args.poll)
                continue
            working_actors = midflow_actor_ids(state, details, self.main)
            if not restarted and time.monotonic() - start >= self.args.restart_after and (self.args.mode == 'mechanics' or working_actors):
                before_actors = sorted(s['id'] for s in state['sessions'])
                before_message_ids = sorted(m['id'] for m in state['conversation_messages'])
                mid_flow = bool(working_actors)
                observed_at = time.time()
                self.stop(daemon_only=True, force=True)
                self.start()
                restarted = True
                self.result['restart'] = {'performed': True, 'midFlow': mid_flow, 'beforeActorIds': before_actors,
                                          'workingActorIds': working_actors, 'observedAt': observed_at,
                                          'storyIds': sorted(d['story']['id'] for d in details)}
            if restarted and len(details) >= 2 and all(d['story']['status'] == 'done' for d in details):
                required = [r for r in phase_latencies(state, details, self.continuation_history, self.args.max_dispatch_latency, self.main) if r['required']]
                if required and all(r.get('passed') for r in required):
                    break
            time.sleep(self.args.poll)
        if last:
            last = self.observe()
            state, board, details, continuations = last
            if self.idle_wake:
                self.evaluate_idle_wake(state, details)
            else:
                self.evaluate(*last)
            self.result['providerTurns'] = [{
                'turnId': t['id'], 'actorId': t.get('handled_by_session_id'), 'state': t.get('state'),
                'startedAt': t.get('started_at'), 'completedAt': t.get('completed_at'),
                'providerTurnRecorded': bool(t.get('provider_turn_id'))
            } for t in state['conversation_turns']]
            if not self.idle_wake:
                self.check('restart_recovery', restarted and self.result['restart']['midFlow'] and
                           set(before_actors) <= {s['id'] for s in state['sessions']} and
                           set(before_message_ids) <= {m['id'] for m in state['conversation_messages']} and
                           bool(details) and all(d['story']['status'] == 'done' for d in details))
                self.check('restart_mechanics', restarted)
        intakes = [m for m in self.mutations if m['method'] == 'POST' and m['route'].endswith('/conversation/messages')]
        self.check('exactly_one_intake_no_nudges', len(intakes) == 1 and all(m['route'].endswith('/conversation/messages') or m['route'] == '/api/v1/workspace/commands' for m in self.mutations if m['at'] >= self.result.get('intakeStartedAt', float('inf'))))
        self.result['mutationAudit'] = self.mutations
        self.result['observationSamples'] = self.samples

    def purge_accounts(self):
        current = self.track()
        if any(pid in current and current[pid][1] == identity for pid, identity in self.owned.items()):
            self.check('credential_store_cleanup', False, reason='owned processes still alive')
            return
        removed, private = [], True
        for harnesses in self.root.rglob('harnesses'):
            if not harnesses.is_dir() or not harnesses.resolve().is_relative_to(self.root):
                continue
            for harness in harnesses.iterdir():
                if not harness.is_dir() or harness.is_symlink():
                    continue
                for name in ('accounts', 'pending-accounts', 'switch-staging'):
                    target = harness / name
                    if not target.exists() and not target.is_symlink():
                        continue
                    if target.is_symlink():
                        private = False
                        target.unlink()
                    else:
                        for path in [target, *target.rglob('*')]:
                            mode = path.lstat()
                            private = private and mode.st_uid == os.getuid() and not (stat.S_IMODE(mode.st_mode) & 0o077)
                        shutil.rmtree(target)
                    if target.exists() or target.is_symlink():
                        raise RuntimeError('scratch credential store removal failed')
                    removed.append(str(target))
        self.result['credentialStoresRemoved'] = removed
        self.check('credential_store_permissions', private)
        self.check('credential_store_cleanup', True)
        self.check('accounts_purged', True)

    def finalize(self):
        # Reset even if an earlier seed phase cleaned up successfully.
        self.check('processes_gone', False)
        self.check('accounts_purged', False)
        for stage, operation in [('process_teardown', self.stop), ('credential_teardown', self.purge_accounts)]:
            try:
                operation()
            except (Exception, KeyboardInterrupt) as error:
                self.result['errors'].append(stage + ':' + type(error).__name__)
        self.emit()

    def emit(self):
        assertions = self.result['assertions']
        ok = all(assertions.get(name, {}).get('passed', False) for name in ('processes_gone', 'accounts_purged')) and bool(assertions) and all(a['passed'] for a in assertions.values()) and not self.result['errors']
        self.result['passed'] = False if self.idle_wake else ok  # a targeted run is never full acceptance
        if self.idle_wake:
            self.result['targetedIdleWakePassed'] = ok
        output = Path(self.args.output).resolve()
        output.mkdir(parents=True, exist_ok=True)
        output.chmod(0o700)
        path = output / 'result.json'
        path.write_text(json.dumps(self.result, indent=2) + '\n')
        path.chmod(0o600)
        lines = ['# Story 6 runtime acceptance' if not self.idle_wake else '# TARGETED idle-manager CDC wake run (not an acceptance result)', '',
                 '**' + (('PASS' if ok else 'NOT ACCEPTED') if not self.idle_wake else ('TARGETED GATES PASSED' if self.result['targetedIdleWakePassed'] else 'TARGETED GATES NOT MET') + ' — controlled conditions; autonomy not accepted') + '**', '',
                 f'Mode: {self.args.mode}. Binary SHA256: `{self.result.get("binary", {}).get("sha256", "unknown")}`.',
                 'One-request observation only; no fabricated transitions. Static review is separate from this run.', '',
                 '| Assertion | Result |', '|---|---|']
        lines += [f'| {k} | {"PASS" if v["passed"] else "FAIL / NOT OBSERVED"} |' for k, v in assertions.items()]
        if self.result.get('idleWakeInconclusive'):
            lines += ['', '**INCONCLUSIVE wake gate:** the story was already Done when the worker\'s Done report arrived, so there was nothing left to continue. '
                      'The CDC wake was not exercised; this is not a product failure and not a wake pass. Rerun for a decided sample if the claim is needed.']
        lines += ['', 'Expected baseline limitation: c833b70 does not contain the autonomy implementation. Missing actor/provider or workflow evidence is not a pass.',
                  '', 'Private raw logs/data remain in the protected scratch root; result.json contains sanitized evidence only.',
                  '', 'Deferred: native UI acceptance and installation approval.']
        (output / 'summary.md').write_text('\n'.join(lines) + '\n')
        print(json.dumps({'result': str(path), 'summary': str(output / 'summary.md'), 'passed': self.result['passed'], **({'targetedIdleWakePassed': ok} if self.idle_wake else {})}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    binary = parser.add_mutually_exclusive_group(required=True)
    binary.add_argument('--daemon', type=Path)
    binary.add_argument('--app', type=Path, help='Resolve bundled daemon only; never launch the GUI')
    parser.add_argument('--mode', choices=('mechanics', 'real'), default='mechanics')
    parser.add_argument('--scenario', choices=('full', 'idle-wake'), default='full', help='idle-wake: one targeted idle-manager CDC wake measurement')
    parser.add_argument('--no-fixture-end-turn-rule', action='store_true', help='idle-wake only: drop the fixture AGENTS.md and intake end-turn instructions so only the product built-in rule applies')
    parser.add_argument('--agent', default='codex')
    parser.add_argument('--agent-bin-dir', action='append', default=[])
    parser.add_argument('--pass-env', action='append', default=[])
    parser.add_argument('--auth-readonly', action='append', default=[], metavar='SOURCE=HOME_RELATIVE_DEST', help='Read-only source link; only AO may create its private scratch account store')
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
    if args.no_fixture_end_turn_rule and args.scenario != 'idle-wake':
        parser.error('--no-fixture-end-turn-rule applies to --scenario idle-wake only')
    if args.mode == 'mechanics' and (args.pass_env or args.auth_readonly or args.agent_bin_dir):
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
            harness.finalize()
    return 0 if harness.result.get('targetedIdleWakePassed', harness.result['passed']) else 1


if __name__ == '__main__':
    sys.exit(main())
