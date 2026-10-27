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

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func humanRequestTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t.UTC(), Valid: true} }

func humanRequestFromRow(row gen.HumanRequest) (domain.HumanRequest, error) {
	var changes []domain.HumanRequestChange
	if err := json.Unmarshal([]byte(row.Changes), &changes); err != nil {
		return domain.HumanRequest{}, fmt.Errorf("human request %s has unreadable changes: %w", row.ID, err)
	}
	req := domain.HumanRequest{
		ID: row.ID, Kind: domain.HumanRequestKind(row.Kind), SessionID: domain.SessionID(row.SessionID.String), ProjectID: row.ProjectID.String,
		Requester: domain.HumanRequestRequester{Kind: domain.HumanRequestRequesterKind(row.RequesterKind), SessionID: domain.SessionID(row.RequesterSessionID)},
		Payload:   json.RawMessage(row.Payload), PayloadHash: row.PayloadHash, Summary: row.Summary, Changes: changes,
		State: domain.HumanRequestState(row.State), Result: row.Result, CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC(),
	}
	if row.DecidedAt.Valid {
		req.DecidedAt = row.DecidedAt.Time.UTC()
	}
	return req, nil
}

func humanRequestsFromRows(rows []gen.HumanRequest) ([]domain.HumanRequest, error) {
	out := make([]domain.HumanRequest, 0, len(rows))
	for _, row := range rows {
		req, err := humanRequestFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, nil
}

// InsertHumanRequest files a pending request. When an open request with the same kind,
// target and payload hash already exists the new one is not stored and the existing one is
// returned with inserted false, so a retried filing never creates a second card.
func (s *Store) InsertHumanRequest(ctx context.Context, req domain.HumanRequest) (domain.HumanRequest, bool, error) {
	if (req.SessionID == "") == (req.ProjectID == "") {
		return domain.HumanRequest{}, false, errors.New("a human request is about a session or a project, not both or neither")
	}
	changes, err := json.Marshal(req.Changes)
	if err != nil {
		return domain.HumanRequest{}, false, fmt.Errorf("encode human request changes: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.InsertHumanRequest(ctx, gen.InsertHumanRequestParams{
		ID: req.ID, Kind: string(req.Kind), SessionID: nullString(string(req.SessionID)), ProjectID: nullString(req.ProjectID),
		RequesterKind: string(req.Requester.Kind), RequesterSessionID: string(req.Requester.SessionID),
		Payload: string(req.Payload), PayloadHash: req.PayloadHash, Summary: req.Summary, Changes: string(changes),
		CreatedAt: req.CreatedAt.UTC(), ExpiresAt: req.ExpiresAt.UTC(),
	})
	if err != nil {
		return domain.HumanRequest{}, false, fmt.Errorf("insert human request: %w", err)
	}
	if rows == 1 {
		stored, err := s.qw.GetHumanRequest(ctx, req.ID)
		if err != nil {
			return domain.HumanRequest{}, false, fmt.Errorf("read back human request: %w", err)
		}
		out, err := humanRequestFromRow(stored)
		return out, true, err
	}
	existing, err := s.qw.GetOpenHumanRequest(ctx, gen.GetOpenHumanRequestParams{Kind: string(req.Kind), TargetKey: req.TargetKey(), PayloadHash: req.PayloadHash})
	if err != nil {
		return domain.HumanRequest{}, false, fmt.Errorf("find the open human request: %w", err)
	}
	out, err := humanRequestFromRow(existing)
	return out, false, err
}

// GetHumanRequest reads one request; found is false when there is none.
func (s *Store) GetHumanRequest(ctx context.Context, id string) (domain.HumanRequest, bool, error) {
	row, err := s.qr.GetHumanRequest(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HumanRequest{}, false, nil
	}
	if err != nil {
		return domain.HumanRequest{}, false, fmt.Errorf("read human request %s: %w", id, err)
	}
	req, err := humanRequestFromRow(row)
	return req, err == nil, err
}

// ListHumanRequests returns requests newest first: those in one state, or the most recent of
// any state when state is empty.
func (s *Store) ListHumanRequests(ctx context.Context, state domain.HumanRequestState, limit int) ([]domain.HumanRequest, error) {
	var rows []gen.HumanRequest
	var err error
	if state == "" {
		rows, err = s.qr.ListRecentHumanRequests(ctx, int64(limit))
	} else {
		rows, err = s.qr.ListHumanRequestsByState(ctx, gen.ListHumanRequestsByStateParams{State: string(state), Limit: int64(limit)})
	}
	if err != nil {
		return nil, fmt.Errorf("list human requests: %w", err)
	}
	return humanRequestsFromRows(rows)
}

// CountPendingHumanRequests is how many open requests a requester has.
func (s *Store) CountPendingHumanRequests(ctx context.Context, requester domain.HumanRequestRequester) (int, error) {
	n, err := s.qr.CountPendingHumanRequestsByRequester(ctx, gen.CountPendingHumanRequestsByRequesterParams{RequesterKind: string(requester.Kind), RequesterSessionID: string(requester.SessionID)})
	return int(n), err
}

// CountHumanRequestsFiledSince is how many requests a requester filed at or after since.
func (s *Store) CountHumanRequestsFiledSince(ctx context.Context, requester domain.HumanRequestRequester, since time.Time) (int, error) {
	n, err := s.qr.CountHumanRequestsFiledSince(ctx, gen.CountHumanRequestsFiledSinceParams{RequesterKind: string(requester.Kind), RequesterSessionID: string(requester.SessionID), CreatedAt: since.UTC()})
	return int(n), err
}

// ClaimHumanRequest moves a pending, unexpired request to approved, once. False means it was
// not claimable (already decided, expired, or unknown).
func (s *Store) ClaimHumanRequest(ctx context.Context, id string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ClaimHumanRequest(ctx, gen.ClaimHumanRequestParams{DecidedAt: humanRequestTime(now), ID: id})
	return rows == 1, err
}

// DenyHumanRequest moves a pending request to denied. False means it was not pending.
func (s *Store) DenyHumanRequest(ctx context.Context, id string, now time.Time) (bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.DenyHumanRequest(ctx, gen.DenyHumanRequestParams{DecidedAt: humanRequestTime(now), ID: id})
	return rows == 1, err
}

// FinishHumanRequest records the outcome of applying a claimed request: applied or failed.
func (s *Store) FinishHumanRequest(ctx context.Context, id string, state domain.HumanRequestState, result string, now time.Time) error {
	if state != domain.HumanRequestApplied && state != domain.HumanRequestFailed {
		return fmt.Errorf("a claimed human request finishes applied or failed, not %q", state)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.FinishHumanRequest(ctx, gen.FinishHumanRequestParams{State: string(state), Result: result, DecidedAt: humanRequestTime(now), ID: id})
	if err != nil {
		return fmt.Errorf("finish human request %s: %w", id, err)
	}
	if rows != 1 {
		return fmt.Errorf("finish human request %s: not in the approved state", id)
	}
	return nil
}

// ExpireHumanRequests expires pending requests past their deadline and fails approvals that
// were claimed before staleBefore and never finished. It returns the rows it changed.
func (s *Store) ExpireHumanRequests(ctx context.Context, now, staleBefore time.Time) ([]domain.HumanRequest, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ExpireHumanRequests(ctx, gen.ExpireHumanRequestsParams{Now: humanRequestTime(now), StaleBefore: humanRequestTime(staleBefore)})
	if err != nil {
		return nil, fmt.Errorf("expire human requests: %w", err)
	}
	return humanRequestsFromRows(rows)
}

// FindOpenHumanRequest returns the open request with this kind, target (domain.HumanRequest.TargetKey) and
// payload hash, if there is one.
func (s *Store) FindOpenHumanRequest(ctx context.Context, kind domain.HumanRequestKind, targetKey, payloadHash string) (domain.HumanRequest, bool, error) {
	row, err := s.qr.GetOpenHumanRequest(ctx, gen.GetOpenHumanRequestParams{Kind: string(kind), TargetKey: targetKey, PayloadHash: payloadHash})
	if errors.Is(err, sql.ErrNoRows) {
		return domain.HumanRequest{}, false, nil
	}
	if err != nil {
		return domain.HumanRequest{}, false, fmt.Errorf("find the open human request: %w", err)
	}
	req, err := humanRequestFromRow(row)
	return req, err == nil, err
}

// CountAllPendingHumanRequests is how many open requests there are, whoever filed them.
func (s *Store) CountAllPendingHumanRequests(ctx context.Context) (int, error) {
	n, err := s.qr.CountAllPendingHumanRequests(ctx)
	return int(n), err
}
