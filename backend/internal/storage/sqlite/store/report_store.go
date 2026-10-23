package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/storage/sqlite/gen"
)

// CreateReport atomically persists one validated pending report and all outputs.
func (s *Store) CreateReport(ctx context.Context, rec domain.ReportRecord) (domain.ReportRecord, error) {
	if err := rec.Validate(); err != nil {
		return domain.ReportRecord{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if rec.StoryID != "" {
		if strings.TrimSpace(rec.ClientReportID) == "" {
			return domain.ReportRecord{}, apierr.Invalid("REPORT_KEY_REQUIRED", "Story reports need a stable client report id", nil)
		}
		old, err := s.qw.GetStoryReportByClientID(ctx, gen.GetStoryReportByClientIDParams{SessionID: string(rec.SessionID), ClientReportID: sql.NullString{String: rec.ClientReportID, Valid: true}})
		if err == nil {
			prior, err := s.reportWithOutputs(ctx, old)
			if err != nil {
				return prior, err
			}
			if prior.StoryID != rec.StoryID || prior.State != rec.State || prior.Note != rec.Note || prior.Message != rec.Message || !slices.Equal(prior.Outputs, rec.Outputs) {
				return prior, apierr.Conflict("REPORT_KEY_REUSED", "Report key belongs to a different request", nil)
			}
			return prior, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return domain.ReportRecord{}, err
		}
	}
	var row gen.Report
	err := s.inTx(ctx, "create report", func(q *gen.Queries) error {
		var err error
		if rec.StoryID != "" {
			row, err := q.GetWorkspaceStory(ctx, rec.StoryID)
			if err != nil {
				return err
			}
			var story domain.Story
			if err := json.Unmarshal([]byte(row.Record), &story); err != nil {
				return err
			}
			linked := false
			for _, a := range story.Assignments {
				linked = linked || a.SessionID == rec.SessionID
			}
			if !linked {
				return apierr.Invalid("REPORT_STORY_SESSION", "Reporter must be explicitly assigned to this story", nil)
			}
			if rec.State == domain.ReportNeedsInput || rec.State == domain.ReportStuck {
				if rec.State == domain.ReportNeedsInput {
					story.Question = rec.Note
				} else {
					story.BlockedReason = rec.Note
				}
				story.Revision = row.Revision + 1
				if _, err := q.UpdateWorkspaceStory(ctx, gen.UpdateWorkspaceStoryParams{ID: story.ID, Record: storyJSON(story), Revision: row.Revision}); err != nil {
					return err
				}
			}

		}
		row, err = q.CreateReport(ctx, gen.CreateReportParams{
			ID: rec.ID, SessionID: string(rec.SessionID), ProjectID: string(rec.ProjectID),
			StoryID: sql.NullString{String: rec.StoryID, Valid: rec.StoryID != ""}, ClientReportID: sql.NullString{String: rec.ClientReportID, Valid: rec.StoryID != ""},
			RecipientProjectID: sql.NullString{String: string(rec.RecipientProjectID), Valid: rec.RecipientProjectID != ""},
			State:              string(rec.State), Note: rec.Note, Message: rec.Message,
			CreatedAt: rec.CreatedAt, AvailableAt: rec.AvailableAt,
			SettlementDeadline: nullTime(rec.SettlementDeadline), RepeatCount: rec.RepeatCount,
		})
		if err != nil {
			return err
		}
		for i, output := range rec.Outputs {
			if err := q.CreateReportOutput(ctx, gen.CreateReportOutputParams{
				ReportID: rec.ID, Position: int64(i), Kind: string(output.Kind),
				Reference: output.Reference, Label: output.Label,
			}); err != nil {
				return err
			}
		}
		if rec.StoryID != "" {
			e := domain.StoryEvent{ID: "report-" + rec.ID, StoryID: rec.StoryID, Kind: "report", SenderSessionID: rec.SessionID, Reference: rec.ID, Text: rec.Note + rec.Message, Result: string(rec.State), CreatedAt: rec.CreatedAt}
			return q.CreateWorkspaceEvent(ctx, gen.CreateWorkspaceEventParams{ID: e.ID, StoryID: e.StoryID, Record: storyJSON(e), Request: storyJSON(e)})
		}
		return nil
	})
	if err != nil {
		return domain.ReportRecord{}, fmt.Errorf("create report %s: %w", rec.ID, err)
	}
	created := reportFromGen(row)
	created.Outputs = append([]domain.ReportOutput(nil), rec.Outputs...)
	return created, nil
}

// GetReport returns one report by durable ID with its ordered outputs.
func (s *Store) GetReport(ctx context.Context, id string) (domain.ReportRecord, bool, error) {
	row, err := s.qr.GetReport(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReportRecord{}, false, nil
	}
	if err != nil {
		return domain.ReportRecord{}, false, fmt.Errorf("get report %s: %w", id, err)
	}
	rec, err := s.reportWithOutputs(ctx, row)
	if err != nil {
		return domain.ReportRecord{}, false, err
	}
	return rec, true, nil
}

// ListReportsBySession returns a worker's reports in creation order.
func (s *Store) ListReportsBySession(ctx context.Context, id domain.SessionID) ([]domain.ReportRecord, error) {
	rows, err := s.qr.ListReportsBySession(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("list reports: %w", err)
	}
	return s.reportsWithOutputs(ctx, rows)
}

// ListReportedPRURLs returns only a worker's structured created-PR outputs.
func (s *Store) ListReportedPRURLs(ctx context.Context, id domain.SessionID) ([]string, error) {
	urls, err := s.qr.ListReportedPRURLs(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("list reported PR URLs for %s: %w", id, err)
	}
	return urls, nil
}

// ListReportsByProject is the read-only persisted report projection. Consumers
// such as Project Summary do not participate in the delivery claim lifecycle.
func (s *Store) ListReportsByProject(ctx context.Context, id domain.ProjectID) ([]domain.ReportRecord, error) {
	rows, err := s.qr.ListReportsByProject(ctx, string(id))
	if err != nil {
		return nil, fmt.Errorf("list project reports: %w", err)
	}
	return s.reportsWithOutputs(ctx, rows)
}

// ListPendingReportSchedule returns the earliest pending work regardless of
// availability so a coordinator can restore the original deadline after restart.
func (s *Store) ListPendingReportSchedule(ctx context.Context, limit int64) ([]domain.ReportRecord, error) {
	rows, err := s.qr.ListPendingReportSchedule(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("list report schedule: %w", err)
	}
	return s.reportsWithOutputs(ctx, rows)
}

// ClaimPendingReportBatch atomically claims every pending report for a project.
// A due, attention, or piggyback trigger therefore includes earlier batch work.
func (s *Store) ClaimPendingReportBatch(ctx context.Context, projectID domain.ProjectID, token string, at time.Time) ([]domain.ReportRecord, error) {
	if projectID == "" || token == "" || at.IsZero() {
		return nil, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var rows []gen.Report
	err := s.inTx(ctx, "claim project report batch", func(q *gen.Queries) error {
		scheduled, err := q.ListPendingReportsByProjectSchedule(ctx, gen.ListPendingReportsByProjectScheduleParams{
			ProjectID: sql.NullString{String: string(projectID), Valid: true}, Limit: 1,
		})
		if err != nil || len(scheduled) == 0 {
			return err
		}
		batchID := scheduled[0].DeliveryBatchID
		if batchID == "" {
			batchID = "report-batch:" + scheduled[0].ID
			if err := q.AssignPendingReportsToBatch(ctx, gen.AssignPendingReportsToBatchParams{
				DeliveryBatchID: batchID, ProjectID: sql.NullString{String: string(projectID), Valid: true},
			}); err != nil {
				return err
			}
		}
		rows, err = q.ClaimPendingReportsByBatch(ctx, gen.ClaimPendingReportsByBatchParams{
			ProjectID: sql.NullString{String: string(projectID), Valid: true}, DeliveryBatchID: batchID,
			ClaimToken: token, ClaimedAt: nullTime(at),
		})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("claim project report batch: %w", err)
	}
	return s.reportsWithOutputs(ctx, orderedReportRows(rows))
}

// AcknowledgeReportBatch completes only the project batch holding token.
func (s *Store) AcknowledgeReportBatch(ctx context.Context, projectID domain.ProjectID, token string, at time.Time) (int, error) {
	if projectID == "" || token == "" || at.IsZero() {
		return 0, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.AcknowledgeReportBatch(ctx, gen.AcknowledgeReportBatchParams{
		ProjectID: sql.NullString{String: string(projectID), Valid: true}, ClaimToken: token, AcknowledgedAt: nullTime(at),
	})
	if err != nil {
		return 0, fmt.Errorf("acknowledge project report batch: %w", err)
	}
	return len(rows), nil
}

// ReleaseReportBatch returns the matching project batch to pending without
// moving any report's original deadline later.
func (s *Store) ReleaseReportBatch(ctx context.Context, projectID domain.ProjectID, token string, at time.Time, lastError string) (int, error) {
	if projectID == "" || token == "" || at.IsZero() {
		return 0, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.ReleaseReportBatch(ctx, gen.ReleaseReportBatchParams{
		ProjectID: sql.NullString{String: string(projectID), Valid: true}, ClaimToken: token, AvailableAt: at, LastError: lastError,
	})
	if err != nil {
		return 0, fmt.Errorf("release project report batch: %w", err)
	}
	return len(rows), nil
}

// DeferReportBatch coalesces a throttled urgent retry until it may request the
// next interruption. Unlike failure release, this intentionally moves its
// delivery availability later.
func (s *Store) DeferReportBatch(ctx context.Context, projectID domain.ProjectID, token string, at time.Time, lastError string) (int, error) {
	if projectID == "" || token == "" || at.IsZero() {
		return 0, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.qw.DeferReportBatch(ctx, gen.DeferReportBatchParams{
		ProjectID: sql.NullString{String: string(projectID), Valid: true}, ClaimToken: token, AvailableAt: at, LastError: lastError,
	})
	if err != nil {
		return 0, fmt.Errorf("defer project report batch: %w", err)
	}
	return len(rows), nil
}

// AcquireReportInterrupt durably enforces the per-worker interruption window.
func (s *Store) AcquireReportInterrupt(ctx context.Context, sessionID domain.SessionID, at time.Time, window time.Duration) (bool, time.Time, error) {
	if sessionID == "" || at.IsZero() || window <= 0 {
		return false, time.Time{}, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	acquired := false
	next := at
	err := s.inTx(ctx, "acquire report interrupt", func(q *gen.Queries) error {
		last, err := q.GetReportInterrupt(ctx, string(sessionID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && last.After(at.Add(-window)) {
			next = last.Add(window)
			return nil
		}
		if err := q.PutReportInterrupt(ctx, gen.PutReportInterruptParams{SessionID: string(sessionID), LastInterruptedAt: at}); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return acquired, next, err
}

// ListPendingReports returns reports eligible for a later delivery batch.
func (s *Store) ListPendingReports(ctx context.Context, at time.Time, limit int64) ([]domain.ReportRecord, error) {
	rows, err := s.qr.ListPendingReports(ctx, gen.ListPendingReportsParams{AvailableAt: at, Limit: limit})
	if err != nil {
		return nil, fmt.Errorf("list pending reports: %w", err)
	}
	return s.reportsWithOutputs(ctx, rows)
}

// ClaimReport atomically fences one pending report to a delivery attempt token.
func (s *Store) ClaimReport(ctx context.Context, id, token string, at time.Time) (domain.ReportRecord, bool, error) {
	return s.updateReportClaim(ctx, id, token, at, "claim", func() (gen.Report, error) {
		return s.qw.ClaimReport(ctx, gen.ClaimReportParams{ID: id, ClaimToken: token, ClaimedAt: nullTime(at)})
	})
}

// AcknowledgeReport completes only the delivery attempt holding the current token.
func (s *Store) AcknowledgeReport(ctx context.Context, id, token string, at time.Time) (domain.ReportRecord, bool, error) {
	return s.updateReportClaim(ctx, id, token, at, "acknowledge", func() (gen.Report, error) {
		return s.qw.AcknowledgeReport(ctx, gen.AcknowledgeReportParams{ID: id, ClaimToken: token, AcknowledgedAt: nullTime(at)})
	})
}

// ReleaseReport returns the matching token's claim to pending with retry metadata.
func (s *Store) ReleaseReport(ctx context.Context, id, token string, availableAt time.Time, lastError string) (domain.ReportRecord, bool, error) {
	return s.updateReportClaim(ctx, id, token, availableAt, "release", func() (gen.Report, error) {
		return s.qw.ReleaseReport(ctx, gen.ReleaseReportParams{ID: id, ClaimToken: token, AvailableAt: availableAt, LastError: lastError})
	})
}

func (s *Store) updateReportClaim(ctx context.Context, id, token string, at time.Time, action string, update func() (gen.Report, error)) (domain.ReportRecord, bool, error) {
	if id == "" || token == "" || at.IsZero() {
		return domain.ReportRecord{}, false, domain.ErrInvalidReport
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	row, err := update()
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ReportRecord{}, false, nil
	}
	if err != nil {
		return domain.ReportRecord{}, false, fmt.Errorf("%s report %s: %w", action, id, err)
	}
	rec, err := s.reportWithOutputs(ctx, row)
	return rec, err == nil, err
}

// RequeueClaimedReports atomically recovers claims owned by a prior daemon process.
func (s *Store) RequeueClaimedReports(ctx context.Context) (int64, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	count, err := s.qw.RequeueClaimedReports(ctx)
	if err != nil {
		return 0, fmt.Errorf("requeue claimed reports: %w", err)
	}
	return count, nil
}

func (s *Store) reportWithOutputs(ctx context.Context, row gen.Report) (domain.ReportRecord, error) {
	outputs, err := s.qr.ListReportOutputs(ctx, row.ID)
	if err != nil {
		return domain.ReportRecord{}, fmt.Errorf("list report %s outputs: %w", row.ID, err)
	}
	rec := reportFromGen(row)
	rec.Outputs = make([]domain.ReportOutput, len(outputs))
	for i, output := range outputs {
		rec.Outputs[i] = domain.ReportOutput{Kind: domain.ReportOutputKind(output.Kind), Reference: output.Reference, Label: output.Label}
	}
	return rec, nil
}

func (s *Store) reportsWithOutputs(ctx context.Context, rows []gen.Report) ([]domain.ReportRecord, error) {
	out := make([]domain.ReportRecord, 0, len(rows))
	for _, row := range rows {
		rec, err := s.reportWithOutputs(ctx, row)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func orderedReportRows(rows []gen.Report) []gen.Report {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].CreatedAt.Equal(rows[j].CreatedAt) {
			return rows[i].ID < rows[j].ID
		}
		return rows[i].CreatedAt.Before(rows[j].CreatedAt)
	})
	return rows
}

func reportFromGen(r gen.Report) domain.ReportRecord {
	return domain.ReportRecord{
		StoryID: r.StoryID.String, ClientReportID: r.ClientReportID.String,
		ID: r.ID, SessionID: domain.SessionID(r.SessionID), ProjectID: domain.ProjectID(r.ProjectID),
		RecipientProjectID: domain.ProjectID(r.RecipientProjectID.String),
		State:              domain.ReportState(r.State), Note: r.Note, Message: r.Message, CreatedAt: r.CreatedAt,
		DeliveryState: domain.ReportDeliveryState(r.DeliveryState), AvailableAt: r.AvailableAt,
		SettlementDeadline: timeFromNull(r.SettlementDeadline), RepeatCount: r.RepeatCount,
		ClaimToken: r.ClaimToken, ClaimedAt: timeFromNull(r.ClaimedAt), DeliveryAttempts: r.DeliveryAttempts,
		AcknowledgedAt: timeFromNull(r.AcknowledgedAt), LastError: r.LastError,
		DeliveryBatchID: r.DeliveryBatchID,
	}
}
