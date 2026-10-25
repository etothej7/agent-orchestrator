package report

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/httpd/apierr"
	"github.com/aoagents/agent-orchestrator/backend/internal/service/coordination"
)

// Store is the report service's persistence and ownership boundary.
type Store interface {
	GetSession(context.Context, domain.SessionID) (domain.SessionRecord, bool, error)
	CreateReport(context.Context, domain.ReportRecord) (domain.ReportRecord, error)
}

// CreateInput is the validated caller payload before AO-derived ownership and deadlines.
type CreateInput struct {
	StoryID        string
	ClientReportID string
	SessionID      domain.SessionID
	State          domain.ReportState
	Note           string
	Message        string
	Outputs        []domain.ReportOutput
}

// ArtifactRecorder shows a reported artifact in the worker's chat thread. It
// is best effort: it skips what it cannot show and never fails the report.
type ArtifactRecorder interface {
	RecordReportedArtifact(ctx context.Context, id domain.SessionID, reference string)
}

// Service validates and creates durable worker reports.
type Service struct {
	store        Store
	now          func() time.Time
	newID        func() string
	onCreated    func(domain.ReportRecord)
	artifacts    ArtifactRecorder
	coordination func(context.Context) (domain.CoordinationConfig, error)
}

// Deps configures a Service. Artifacts is optional.
type Deps struct {
	Store        Store
	Now          func() time.Time
	NewID        func() string
	OnCreated    func(domain.ReportRecord)
	Artifacts    ArtifactRecorder
	Coordination func(context.Context) (domain.CoordinationConfig, error)
}

// New constructs a report Service.
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.NewID == nil {
		d.NewID = func() string { return "rpt_" + uuid.NewString() }
	}
	return &Service{store: d.Store, now: d.Now, newID: d.NewID, onCreated: d.OnCreated, artifacts: d.Artifacts, coordination: d.Coordination}
}

// ListProject returns persisted report facts ordered by created_at then id,
// with each report's outputs in persisted position order. It never claims,
// acknowledges, schedules, or otherwise mutates report delivery state.
func (s *Service) ListProject(ctx context.Context, projectID domain.ProjectID) ([]domain.ReportRecord, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("report: store is required")
	}
	if projectID == "" {
		return nil, apierr.Invalid("REPORT_PROJECT_REQUIRED", "Project id is required", nil)
	}
	reader, ok := s.store.(interface {
		ListReportsByProject(context.Context, domain.ProjectID) ([]domain.ReportRecord, error)
	})
	if !ok {
		return nil, errors.New("report: project reader is required")
	}
	reports, err := reader.ListReportsByProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i].CreatedAt.Equal(reports[j].CreatedAt) {
			return reports[i].ID < reports[j].ID
		}
		return reports[i].CreatedAt.Before(reports[j].CreatedAt)
	})
	return reports, nil
}

// Create validates ownership and persists one pending report. Reports are
// coordination claims and never mutate authoritative worker lifecycle state.
func (s *Service) Create(ctx context.Context, input CreateInput) (domain.ReportRecord, error) {
	if s == nil || s.store == nil {
		return domain.ReportRecord{}, errors.New("report: store is required")
	}
	ctx, releaseCoordination := coordination.ReadLock(ctx, s.store)
	defer releaseCoordination()
	if input.SessionID == "" {
		return domain.ReportRecord{}, apierr.Invalid("INVALID_REPORT_SESSION", "Worker session id is required", nil)
	}
	if err := domain.ValidateReportContent(input.State, input.Note, input.Message, input.Outputs); err != nil {
		return domain.ReportRecord{}, apierr.Invalid("INVALID_REPORT", "Report state, note, message, or outputs are invalid", nil)
	}
	session, ok, err := s.store.GetSession(ctx, input.SessionID)
	if err != nil {
		return domain.ReportRecord{}, err
	}
	if !ok {
		return domain.ReportRecord{}, apierr.NotFound("SESSION_NOT_FOUND", "Unknown session")
	}
	var recipient domain.ProjectID
	if session.Kind != domain.KindWorker {
		if session.Kind != domain.KindOrchestrator || s.coordination == nil || session.IsTerminated || session.Activity.State == domain.ActivityExited {
			return domain.ReportRecord{}, apierr.Invalid("REPORT_WORKER_REQUIRED", "Reports require a worker or an assigned, active repo manager", nil)
		}
		cfg, err := s.coordination(ctx)
		if err != nil {
			return domain.ReportRecord{}, err
		}
		if !cfg.Manages(session.ProjectID) {
			return domain.ReportRecord{}, apierr.Invalid("REPORT_MANAGER_UNASSIGNED", "This orchestrator is not assigned to an overall coordinator", nil)
		}
		reader, ok := s.store.(interface {
			ListSessions(context.Context, domain.ProjectID) ([]domain.SessionRecord, error)
		})
		if !ok {
			return domain.ReportRecord{}, errors.New("report: manager session reader is required")
		}
		sessions, err := reader.ListSessions(ctx, session.ProjectID)
		if err != nil {
			return domain.ReportRecord{}, err
		}
		for _, other := range sessions {
			if other.Kind == domain.KindOrchestrator && !other.IsTerminated && other.Activity.State != domain.ActivityExited && other.CreatedAt.After(session.CreatedAt) {
				return domain.ReportRecord{}, apierr.Invalid("REPORT_MANAGER_REPLACED", "The current repo manager must submit this report", nil)
			}
		}
		recipient = cfg.CoordinatorProjectID
	}

	now := s.now().UTC()
	var availableAt time.Time
	var settlementDeadline time.Time
	switch input.State {
	case domain.ReportDone:
		settlementDeadline = now.Add(domain.ReportSettlementWindow)
		// Settlement metadata is historical grouping information, never a phase gate.
		availableAt = now
	case domain.ReportNeedsInput, domain.ReportStuck:
		availableAt = now
	default:
		availableAt = now.Add(domain.ReportBatchFallback)
	}
	rec := domain.ReportRecord{
		StoryID: input.StoryID, ClientReportID: input.ClientReportID,
		ID: s.newID(), SessionID: session.ID, ProjectID: session.ProjectID,
		RecipientProjectID: recipient,
		State:              input.State, Note: input.Note, Message: input.Message, Outputs: input.Outputs,
		CreatedAt: now, DeliveryState: domain.ReportPending, AvailableAt: availableAt,
		SettlementDeadline: settlementDeadline, RepeatCount: 1,
	}
	// Manager summaries and suggestions should reach the human-facing
	// coordinator promptly, rather than waiting for worker settlement windows.
	if recipient != "" {
		rec.AvailableAt = now
	}
	created, err := s.store.CreateReport(ctx, rec)
	if err != nil {
		return created, err
	}
	if s.onCreated != nil {
		s.onCreated(created)
	}
	// Synchronous on purpose: ao report --done is often the agent's last tool
	// call, so a goroutine would race the turn's completion and could drop the
	// artifact or put it on the next turn.
	if s.artifacts != nil {
		for _, output := range created.Outputs {
			if output.Kind == domain.ReportOutputArtifact {
				s.artifacts.RecordReportedArtifact(ctx, created.SessionID, output.Reference)
			}
		}
	}
	return created, nil
}
