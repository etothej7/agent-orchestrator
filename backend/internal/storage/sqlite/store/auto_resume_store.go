package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// daemonRunsKept bounds the boot log. The resumer reads only the last few
// minutes of it and the capture step only the previous run.
const daemonRunsKept = 20

// AutoResumePolicy reads a session's auto-resume policy. A session with no stored
// policy is allowed. An unreadable value is an error: callers that gate an action
// on it must use AutoResumeAllowed, which fails closed.
func (s *Store) AutoResumePolicy(ctx context.Context, id domain.SessionID) (domain.AutoResumePolicy, error) {
	row, err := s.qr.GetSessionAutoResume(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AutoResumeAllowed, nil
	}
	if err != nil {
		return domain.AutoResumeNever, fmt.Errorf("read auto-resume policy for %s: %w", id, err)
	}
	policy := domain.AutoResumePolicy(row.Policy)
	if !policy.Valid() {
		return domain.AutoResumeNever, fmt.Errorf("auto-resume policy for %s has unknown value %q", id, row.Policy)
	}
	return policy, nil
}

// AutoResumeAllowed reports whether AO may resume the session on its own. It
// FAILS CLOSED: a read error or an unknown stored value is "not allowed".
func (s *Store) AutoResumeAllowed(ctx context.Context, id domain.SessionID) bool {
	policy, err := s.AutoResumePolicy(ctx, id)
	return err == nil && policy.Allows()
}

// SetAutoResumePolicy stores a session's policy and keeps any recorded stop reason.
func (s *Store) SetAutoResumePolicy(ctx context.Context, id domain.SessionID, policy domain.AutoResumePolicy, now time.Time) error {
	if !policy.Valid() {
		return fmt.Errorf("invalid auto-resume policy %q", policy)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.UpsertAutoResumePolicy(ctx, gen.UpsertAutoResumePolicyParams{SessionID: string(id), Policy: string(policy), UpdatedAt: now})
}

// StopReason reads why the agent was last stopped on purpose ("" if it was not).
func (s *Store) StopReason(ctx context.Context, id domain.SessionID) (domain.StopReason, error) {
	row, err := s.qr.GetSessionAutoResume(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StopReasonNone, nil
	}
	if err != nil {
		return domain.StopReasonNone, fmt.Errorf("read stop reason for %s: %w", id, err)
	}
	return domain.StopReason(row.StopReason), nil
}

// SetStopReason records (or, with StopReasonNone, clears) why an agent was stopped
// on purpose, keeping the session's policy.
func (s *Store) SetStopReason(ctx context.Context, id domain.SessionID, reason domain.StopReason, now time.Time) error {
	if !reason.Valid() {
		return fmt.Errorf("invalid stop reason %q", reason)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if reason == domain.StopReasonNone {
		// Clearing only touches a row that has a reason. Every controller start clears,
		// and that must not create a row per session.
		return s.qw.ClearStopReason(ctx, gen.ClearStopReasonParams{UpdatedAt: now, SessionID: string(id)})
	}
	return s.qw.UpsertStopReason(ctx, gen.UpsertStopReasonParams{SessionID: string(id), StopReason: string(reason), UpdatedAt: now})
}

// StartDaemonRun records a new run and prunes the log to the most recent runs.
func (s *Store) StartDaemonRun(ctx context.Context, bootID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.inTx(ctx, "start daemon run", func(q *gen.Queries) error {
		if err := q.InsertDaemonRun(ctx, gen.InsertDaemonRunParams{BootID: bootID, StartedAt: now, HeartbeatAt: now}); err != nil {
			return err
		}
		return q.PruneDaemonRuns(ctx, daemonRunsKept)
	})
}

// HeartbeatDaemonRun refreshes the run's liveness stamp.
func (s *Store) HeartbeatDaemonRun(ctx context.Context, bootID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.HeartbeatDaemonRun(ctx, gen.HeartbeatDaemonRunParams{HeartbeatAt: now, BootID: bootID})
}

// MarkDaemonShutdownStarted stamps the start of a graceful shutdown. The first
// stamp wins; a repeat is a no-op.
func (s *Store) MarkDaemonShutdownStarted(ctx context.Context, bootID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.MarkDaemonShutdownStarted(ctx, gen.MarkDaemonShutdownStartedParams{
		ShutdownStartedAt: sql.NullTime{Time: now, Valid: true}, MAX: now, BootID: bootID,
	})
}

// MarkDaemonEndedClean stamps the end of a graceful shutdown.
func (s *Store) MarkDaemonEndedClean(ctx context.Context, bootID string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.MarkDaemonEndedClean(ctx, gen.MarkDaemonEndedCleanParams{EndedCleanAt: sql.NullTime{Time: now, Valid: true}, BootID: bootID})
}

// PreviousDaemonRun returns the most recent run other than bootID, if any.
func (s *Store) PreviousDaemonRun(ctx context.Context, bootID string) (domain.DaemonRun, bool, error) {
	row, err := s.qr.GetPreviousDaemonRun(ctx, bootID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.DaemonRun{}, false, nil
	}
	if err != nil {
		return domain.DaemonRun{}, false, err
	}
	run := domain.DaemonRun{BootID: row.BootID, StartedAt: row.StartedAt, HeartbeatAt: row.HeartbeatAt}
	if row.ShutdownStartedAt.Valid {
		t := row.ShutdownStartedAt.Time
		run.ShutdownStartedAt = &t
	}
	if row.EndedCleanAt.Valid {
		t := row.EndedCleanAt.Time
		run.EndedCleanAt = &t
	}
	return run, true, nil
}

// DaemonRunsSince counts runs (including the current one) that started at or after since.
func (s *Store) DaemonRunsSince(ctx context.Context, since time.Time) (int, error) {
	n, err := s.qr.CountDaemonRunsSince(ctx, since)
	return int(n), err
}

// GetResumeIntent reads a session's captured intent.
func (s *Store) GetResumeIntent(ctx context.Context, id domain.SessionID) (domain.ResumeIntent, bool, error) {
	row, err := s.qr.GetResumeIntent(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ResumeIntent{}, false, nil
	}
	if err != nil {
		return domain.ResumeIntent{}, false, err
	}
	intent, err := decodeResumeIntent(row.SessionID, row.Record, row.Revision)
	return intent, err == nil, err
}

// ListResumeIntents returns every captured intent, ordered by session id.
func (s *Store) ListResumeIntents(ctx context.Context) ([]domain.ResumeIntent, error) {
	rows, err := s.qr.ListResumeIntents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ResumeIntent, 0, len(rows))
	for _, row := range rows {
		intent, err := decodeResumeIntent(row.SessionID, row.Record, row.Revision)
		if err != nil {
			return nil, err
		}
		out = append(out, intent)
	}
	return out, nil
}

func decodeResumeIntent(sessionID, record string, revision int64) (domain.ResumeIntent, error) {
	var intent domain.ResumeIntent
	if err := json.Unmarshal([]byte(record), &intent); err != nil {
		return domain.ResumeIntent{}, fmt.Errorf("decode resume intent for %s: %w", sessionID, err)
	}
	intent.SessionID = domain.SessionID(sessionID)
	intent.Revision = revision
	return intent, nil
}

// CaptureResumeIntent records that a session lost its agent in a restart. An
// intent that is still live (pending or resuming) is KEPT untouched, so a second
// boot before the first finished neither restarts its clock nor resets its
// attempts. A finished one is replaced by the fresh intent, carrying over its
// recent resume timestamps for the flap guard. It reports the intent now stored
// and whether it is the one passed in.
func (s *Store) CaptureResumeIntent(ctx context.Context, fresh domain.ResumeIntent) (domain.ResumeIntent, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var stored domain.ResumeIntent
	var created bool
	err := s.inTx(ctx, "capture resume intent", func(q *gen.Queries) error {
		row, err := q.GetResumeIntent(ctx, string(fresh.SessionID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			existing, decodeErr := decodeResumeIntent(row.SessionID, row.Record, row.Revision)
			if decodeErr != nil {
				return decodeErr
			}
			if existing.State.Live() {
				stored, created = existing, false
				return nil
			}
			fresh.RecentResumes = existing.RecentResumes
			payload, encodeErr := json.Marshal(fresh)
			if encodeErr != nil {
				return encodeErr
			}
			if _, updateErr := q.UpdateResumeIntent(ctx, gen.UpdateResumeIntentParams{Record: string(payload), SessionID: string(fresh.SessionID), Revision: row.Revision}); updateErr != nil {
				return updateErr
			}
			fresh.Revision = row.Revision + 1
			stored, created = fresh, true
			return nil
		}
		payload, encodeErr := json.Marshal(fresh)
		if encodeErr != nil {
			return encodeErr
		}
		if _, insertErr := q.InsertResumeIntent(ctx, gen.InsertResumeIntentParams{SessionID: string(fresh.SessionID), Record: string(payload)}); insertErr != nil {
			return insertErr
		}
		fresh.Revision = 1
		stored, created = fresh, true
		return nil
	})
	return stored, created, err
}

// SaveResumeIntent is compare-and-swap on the intent's revision. It reports false
// when another writer changed the intent first.
func (s *Store) SaveResumeIntent(ctx context.Context, intent domain.ResumeIntent) (bool, error) {
	payload, err := json.Marshal(intent)
	if err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	n, err := s.qw.UpdateResumeIntent(ctx, gen.UpdateResumeIntentParams{Record: string(payload), SessionID: string(intent.SessionID), Revision: intent.Revision})
	return n == 1, err
}

// DeleteResumeIntent removes a session's intent (a manual resume, a kill, a policy change).
func (s *Store) DeleteResumeIntent(ctx context.Context, id domain.SessionID) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.qw.DeleteResumeIntent(ctx, string(id))
}
