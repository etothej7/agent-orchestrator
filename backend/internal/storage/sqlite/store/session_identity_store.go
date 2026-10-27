package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// SetSessionProofHash issues a session's identity proof, stored as its SHA-256 hex only, in two
// phases (ED8 P2). A session's first proof, or one whose current proof was never presented, becomes
// the active proof at once (nothing can depend on the old one). A proof that is in use is replaced
// only when its successor is first presented: the new hash waits as PENDING beside it, both verify,
// and a launch that fails therefore leaves the old proof working. Another pending proof replaces
// the previous pending one.
func (s *Store) SetSessionProofHash(ctx context.Context, id domain.SessionID, hash string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	replaced, err := s.qw.ReplaceUnconfirmedSessionIdentity(ctx, gen.ReplaceUnconfirmedSessionIdentityParams{ProofSha256: hash, IssuedAt: now.UTC(), SessionID: string(id)})
	if err != nil {
		return fmt.Errorf("store identity proof for %s: %w", id, err)
	}
	if replaced > 0 {
		return nil
	}
	pending, err := s.qw.SetPendingSessionIdentity(ctx, gen.SetPendingSessionIdentityParams{PendingSha256: sql.NullString{String: hash, Valid: true}, IssuedAt: now.UTC(), SessionID: string(id)})
	if err != nil {
		return fmt.Errorf("store pending identity proof for %s: %w", id, err)
	}
	if pending > 0 {
		return nil
	}
	if err := s.qw.InsertSessionIdentity(ctx, gen.InsertSessionIdentityParams{SessionID: string(id), ProofSha256: hash, IssuedAt: now.UTC()}); err != nil {
		return fmt.Errorf("store identity proof for %s: %w", id, err)
	}
	return nil
}

// SessionIdentityByProofHash finds the session a proof hash was issued to; the hash may be the
// active proof or a pending one. found is false when no session holds that proof.
func (s *Store) SessionIdentityByProofHash(ctx context.Context, hash string) (id domain.SessionID, storedHash string, found bool, err error) {
	row, err := s.qr.GetSessionIdentityByProofHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("look up identity proof: %w", err)
	}
	return domain.SessionID(row.SessionID), hash, true, nil
}

// ConfirmSessionProof records that a proof of the session was presented. Presenting the pending
// proof promotes it (the old active proof stops verifying); presenting the active proof for the
// first time confirms it. Anything else is ignored.
func (s *Store) ConfirmSessionProof(ctx context.Context, id domain.SessionID, hash string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	promoted, err := s.qw.PromoteSessionIdentity(ctx, gen.PromoteSessionIdentityParams{ConfirmedAt: sql.NullTime{Time: now.UTC(), Valid: true}, SessionID: string(id), PendingSha256: sql.NullString{String: hash, Valid: true}})
	if err != nil {
		return fmt.Errorf("promote identity proof for %s: %w", id, err)
	}
	if promoted > 0 {
		return nil
	}
	if err := s.qw.ConfirmSessionIdentity(ctx, gen.ConfirmSessionIdentityParams{ConfirmedAt: sql.NullTime{Time: now.UTC(), Valid: true}, SessionID: string(id)}); err != nil {
		return fmt.Errorf("confirm identity proof for %s: %w", id, err)
	}
	return nil
}

// SessionProofConfirmed reports whether the session has a proof that was actually presented, i.e.
// a running process holds and uses it. A proof that was issued but never used does not count: the
// process-ancestry bridge and the coordinator's two phases only end once the new process is up.
func (s *Store) SessionProofConfirmed(ctx context.Context, id domain.SessionID) (bool, error) {
	row, err := s.qr.GetSessionIdentity(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read identity proof for %s: %w", id, err)
	}
	return row.ConfirmedAt.Valid, nil
}

// SessionProofGeneration reports how many proofs a session has had (0 when it has none).
func (s *Store) SessionProofGeneration(ctx context.Context, id domain.SessionID) (int, error) {
	row, err := s.qr.GetSessionIdentity(ctx, string(id))
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read identity proof for %s: %w", id, err)
	}
	return int(row.Generation), nil
}

// DeleteSessionProof revokes a session's proof.
func (s *Store) DeleteSessionProof(ctx context.Context, id domain.SessionID) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.DeleteSessionIdentity(ctx, string(id)); err != nil {
		return fmt.Errorf("revoke identity proof for %s: %w", id, err)
	}
	return nil
}

// SetReviewProofHash issues or replaces a reviewer's identity proof, stored as its SHA-256 hex
// only. A replacement makes the previous proof stop verifying at once.
func (s *Store) SetReviewProofHash(ctx context.Context, reviewID, hash string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.UpsertReviewIdentity(ctx, gen.UpsertReviewIdentityParams{ReviewID: reviewID, ProofSha256: hash, IssuedAt: now.UTC()}); err != nil {
		return fmt.Errorf("store reviewer identity proof for %s: %w", reviewID, err)
	}
	return nil
}

// ReviewIdentityByProofHash finds the reviewer a proof hash was issued to.
func (s *Store) ReviewIdentityByProofHash(ctx context.Context, hash string) (reviewID, storedHash string, found bool, err error) {
	row, err := s.qr.GetReviewIdentityByProofHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("look up reviewer identity proof: %w", err)
	}
	return row.ReviewID, row.ProofSha256, true, nil
}

// ReviewProofGeneration reports how many proofs a reviewer has had (0 when it has none).
func (s *Store) ReviewProofGeneration(ctx context.Context, reviewID string) (int, error) {
	row, err := s.qr.GetReviewIdentity(ctx, reviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read reviewer identity proof for %s: %w", reviewID, err)
	}
	return int(row.Generation), nil
}

// DeleteReviewProof revokes a reviewer's proof.
func (s *Store) DeleteReviewProof(ctx context.Context, reviewID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.DeleteReviewIdentity(ctx, reviewID); err != nil {
		return fmt.Errorf("revoke reviewer identity proof for %s: %w", reviewID, err)
	}
	return nil
}

// SessionsWithProof reports which of the given sessions hold an identity proof right now, in one
// query (the session list asks for every row at once).
func (s *Store) SessionsWithProof(ctx context.Context, ids []domain.SessionID) (map[domain.SessionID]bool, error) {
	out := map[domain.SessionID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	raw := make([]string, len(ids))
	for i, id := range ids {
		raw[i] = string(id)
	}
	rows, err := s.qr.ListSessionIdentitiesByIDs(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("list session identities: %w", err)
	}
	for _, id := range rows {
		out[domain.SessionID(id)] = true
	}
	return out, nil
}
