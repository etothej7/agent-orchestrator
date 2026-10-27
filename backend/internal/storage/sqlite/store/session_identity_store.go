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

// SetSessionProofHash issues or replaces a session's identity proof, stored as its SHA-256 hex
// only. A replacement makes the previous proof stop verifying at once.
func (s *Store) SetSessionProofHash(ctx context.Context, id domain.SessionID, hash string, now time.Time) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.qw.UpsertSessionIdentity(ctx, gen.UpsertSessionIdentityParams{SessionID: string(id), ProofSha256: hash, IssuedAt: now.UTC()}); err != nil {
		return fmt.Errorf("store identity proof for %s: %w", id, err)
	}
	return nil
}

// SessionIdentityByProofHash finds the session a proof hash was issued to. found is false when
// no session holds that proof.
func (s *Store) SessionIdentityByProofHash(ctx context.Context, hash string) (id domain.SessionID, storedHash string, found bool, err error) {
	row, err := s.qr.GetSessionIdentityByProofHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("look up identity proof: %w", err)
	}
	return domain.SessionID(row.SessionID), row.ProofSha256, true, nil
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
