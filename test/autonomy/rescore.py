"""Read-only re-score of retained evidence; never launches daemon/provider/checks."""
import argparse
import hashlib
import json
from pathlib import Path
import acceptance as a


def rescore(source):
    original = json.loads(source.read_text())
    state = a.read_state(Path(original['scratchRoot']) / 'home/.ao/data/ao.db')
    events = a.records(state, 'workspace_story_events')
    details = [{'story': s, 'events': [e for e in events if e.get('storyId') == s['id']]}
               for s in a.records(state, 'workspace_stories')]
    h = a.Harness.__new__(a.Harness)
    h.root = Path(original['scratchRoot'])
    h.env = {'PATH': '/usr/bin:/bin', 'HOME': str(h.root / 'home')}
    h.repos = original['fixtures']
    h.result = {'assertions': {}}
    h.verify_actor_provenance(state, details, original['storyEvidence'])
    workers = {s['id'] for s in state['sessions'] if s.get('kind') == 'worker'}
    samples = []
    for sample in original['observationSamples']:
        active = {t.get('handled_by_session_id') for t in state['conversation_turns']
                  if t.get('handled_by_session_id') in workers and a.stamp(t.get('started_at')) is not None
                  and a.stamp(t['started_at']) <= sample['at']
                  and (a.stamp(t.get('completed_at')) is None or a.stamp(t['completed_at']) > sample['at'])}
        samples.append({'at': sample['at'], 'workerIds': sorted(active)})
    h.result.update({'kind': 'offline_rescore_same_run', 'newRuntimeRun': False, 'autonomyAccepted': False,
                     'sourceResult': str(source), 'sourceResultSha256': hashlib.sha256(source.read_bytes()).hexdigest(),
                     'originalHarnessSha': original['harnessSha'], 'runtimeBinary': original['binary'],
                     'rescoreSourceSha256': hashlib.sha256(Path(a.__file__).read_bytes()).hexdigest(),
                     'rescoreHarnessSha': a.subprocess.check_output(['/usr/bin/git', '-C', str(Path(a.__file__).resolve().parents[2]), 'rev-parse', 'HEAD'], text=True).strip(),
                     'reportDeliverySeconds': a.report_delivery_timings(state),
                     'phaseRecipientLatency': a.phase_latencies(state, details, {c['deliveryId']: c for c in a.records(state, 'story_continuations') if c.get('deliveryId')}, 30, original.get('mainSessionId')),
                     'originalReportToNextDispatchSeconds': original['reportToNextDispatchSeconds'],
                     'workerSamplesReconstructedFromTurnLifetimes': samples,
                     'maxActiveWorkers': max((len(s['workerIds']) for s in samples), default=0),
                     'limits': ['Historical concurrency reconstructed from durable turn start/end times.',
                                'Report delivery and recipient turn start do not prove a subsequent work dispatch.',
                                'Original result remains unchanged and NOT ACCEPTED. No daemon, provider or golden check executed.']})
    h.check('parallelism_at_most_four', bool(samples) and h.result['maxActiveWorkers'] <= 4)
    return h.result


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('result', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.output.resolve() == args.result.resolve():
        parser.error('output must not replace original evidence')
    result = rescore(args.result)
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(result, indent=2) + '\n')
    print(json.dumps({'rescore': str(args.output), 'autonomyAccepted': False, 'assertions': result['assertions']}))
