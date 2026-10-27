package store_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
)

func TestListChatHibernationCandidatesFiltersInSQL(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	var want []domain.SessionID
	for i, change := range []func(*domain.SessionRecord){
		func(*domain.SessionRecord) {},
		func(r *domain.SessionRecord) { r.Kind = domain.KindOrchestrator },
		func(r *domain.SessionRecord) { r.Mode = domain.SessionModeTUI },
		func(r *domain.SessionRecord) { r.IsTerminated = true },
		func(r *domain.SessionRecord) { r.IsTaskPreparation = true },
		func(r *domain.SessionRecord) { r.ProvisionState = domain.SessionProvisionProvisioning },
		func(r *domain.SessionRecord) { r.Activity.State = domain.ActivityActive },
		func(r *domain.SessionRecord) { r.Metadata.ProviderConversationID = "" },
	} {
		rec := sampleRecord("mer")
		rec.Mode, rec.Activity.State, rec.Metadata.ProviderConversationID = domain.SessionModeChat, domain.ActivityIdle, "native-id"
		change(&rec)
		created, err := s.CreateSession(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			want = append(want, created.ID)
		}
	}
	got, err := s.ListChatHibernationCandidates(ctx)
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("candidates = %v, %v; want %v", got, err, want)
	}
	current, _, _ := s.GetSession(ctx, want[0])
	now := time.Now().UTC()
	if applied, err := s.SetSessionHibernated(ctx, current.ID, current.Revision, &now); err != nil || !applied {
		t.Fatalf("mark asleep = %v, %v", applied, err)
	}
	if got, err := s.ListChatHibernationCandidates(ctx); err != nil || len(got) != 0 {
		t.Fatalf("already sleeping candidates = %v, %v", got, err)
	}
}

func TestSessionHibernationMarkerUsesRevisionAndSurvivesOrdinaryUpdate(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	rec.Mode = domain.SessionModeChat
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	stale, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get session = %v, %v", ok, err)
	}

	// A later session write invalidates the idle snapshot used to choose sleep.
	current := stale
	current.DisplayName = "new activity"
	if err := s.UpdateSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if applied, err := s.SetSessionHibernated(ctx, created.ID, stale.Revision, &when); err != nil || applied {
		t.Fatalf("stale hibernate = %v, %v", applied, err)
	}

	current, ok, err = s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("reload session = %v, %v", ok, err)
	}
	if applied, err := s.SetSessionHibernated(ctx, created.ID, current.Revision, &when); err != nil || !applied {
		t.Fatalf("hibernate = %v, %v", applied, err)
	}
	waking, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok || waking.HibernatedAt == nil || !waking.HibernatedAt.Equal(when) {
		t.Fatalf("hibernated session = %+v, %v, %v", waking.HibernatedAt, ok, err)
	}
	current.DisplayName = "another update"
	if err := s.UpdateSession(ctx, current); err != nil {
		t.Fatal(err)
	}
	waking, ok, err = s.GetSession(ctx, created.ID)
	if err != nil || !ok || waking.HibernatedAt == nil {
		t.Fatalf("marker after ordinary update = %+v, %v, %v", waking.HibernatedAt, ok, err)
	}
	if applied, err := s.SetSessionHibernated(ctx, created.ID, waking.Revision, nil); err != nil || !applied {
		t.Fatalf("wake = %v, %v", applied, err)
	}
	waking, ok, err = s.GetSession(ctx, created.ID)
	if err != nil || !ok || waking.HibernatedAt != nil {
		t.Fatalf("awake session = %+v, %v, %v", waking.HibernatedAt, ok, err)
	}
}

func TestSessionHibernationMarkerEmitsOneChangePerTransition(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	rec.Mode = domain.SessionModeChat
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateConversation(ctx, "hibernate-cdc", domain.ConversationScopeSession, created.ProjectID, created.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	current, ok, err := s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("get session = %v, %v", ok, err)
	}
	base, err := s.LatestSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	when := time.Now().UTC().Truncate(time.Second)
	if applied, err := s.SetSessionHibernated(ctx, created.ID, current.Revision, &when); err != nil || !applied {
		t.Fatalf("hibernate = %v, %v", applied, err)
	}
	events, err := s.EventsAfter(ctx, base, 10)
	if err != nil || len(events) != 1 || string(events[0].Type) != "session_updated" || events[0].SessionID != string(created.ID) {
		t.Fatalf("hibernate events = %+v, %v", events, err)
	}
	var payload struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil || payload.ID != string(created.ID) || payload.ConversationID != "hibernate-cdc" {
		t.Fatalf("hibernate event payload = %+v, %v", payload, err)
	}

	current, ok, err = s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("reload session = %v, %v", ok, err)
	}
	if applied, err := s.SetSessionHibernated(ctx, created.ID, current.Revision, &when); err != nil || !applied {
		t.Fatalf("repeat hibernate = %v, %v", applied, err)
	}
	events, err = s.EventsAfter(ctx, events[0].Seq, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("repeat hibernate events = %+v, %v", events, err)
	}

	current, ok, err = s.GetSession(ctx, created.ID)
	if err != nil || !ok {
		t.Fatalf("reload session = %v, %v", ok, err)
	}
	if applied, err := s.SetSessionHibernated(ctx, created.ID, current.Revision, nil); err != nil || !applied {
		t.Fatalf("wake = %v, %v", applied, err)
	}
	events, err = s.EventsAfter(ctx, base, 10)
	if err != nil || len(events) != 2 || string(events[1].Type) != "session_updated" || events[1].SessionID != string(created.ID) {
		t.Fatalf("wake events = %+v, %v", events, err)
	}
	payload.ID, payload.ConversationID = "", ""
	if err := json.Unmarshal(events[1].Payload, &payload); err != nil || payload.ConversationID != "hibernate-cdc" {
		t.Fatalf("wake event payload = %+v, %v", payload, err)
	}
}

// Parking replaces an exited row with the shape the idle sweeper leaves: idle plus
// the sleep marker, in one revision-checked write.
func TestParkSessionHibernatedWritesIdleAndTheMarkerTogether(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	rec := sampleRecord("mer")
	rec.Mode, rec.Activity.State, rec.Metadata.ProviderConversationID = domain.SessionModeChat, domain.ActivityExited, "native-id"
	created, err := s.CreateSession(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Second)

	// A stale revision parks nothing.
	if applied, err := s.ParkSessionHibernated(ctx, created.ID, created.Revision-1, at); err != nil || applied {
		t.Fatalf("park with a stale revision = %v, %v; want not applied", applied, err)
	}
	if got, _, _ := s.GetSession(ctx, created.ID); got.HibernatedAt != nil || got.Activity.State != domain.ActivityExited {
		t.Fatalf("a refused park changed the row: %+v", got)
	}

	if applied, err := s.ParkSessionHibernated(ctx, created.ID, created.Revision, at); err != nil || !applied {
		t.Fatalf("park = %v, %v; want applied", applied, err)
	}
	got, _, _ := s.GetSession(ctx, created.ID)
	if got.HibernatedAt == nil || !got.HibernatedAt.Equal(at) || got.Activity.State != domain.ActivityIdle {
		t.Fatalf("parked row = hibernated %v, activity %q; want the marker and idle together", got.HibernatedAt, got.Activity.State)
	}
	// Parked twice is a no-op: the marker is already there.
	if applied, err := s.ParkSessionHibernated(ctx, created.ID, got.Revision, at.Add(time.Minute)); err != nil || applied {
		t.Fatalf("second park = %v, %v; want not applied", applied, err)
	}
}

func TestParkSessionHibernatedRefusesTerminatedAndTerminalSessions(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "mer")
	for name, change := range map[string]func(*domain.SessionRecord){
		"terminated": func(r *domain.SessionRecord) { r.IsTerminated = true },
		"tui":        func(r *domain.SessionRecord) { r.Mode = domain.SessionModeTUI },
	} {
		rec := sampleRecord("mer")
		rec.Mode, rec.Activity.State = domain.SessionModeChat, domain.ActivityExited
		change(&rec)
		created, err := s.CreateSession(ctx, rec)
		if err != nil {
			t.Fatal(err)
		}
		if applied, err := s.ParkSessionHibernated(ctx, created.ID, created.Revision, time.Now().UTC()); err != nil || applied {
			t.Fatalf("%s: park = %v, %v; want not applied", name, applied, err)
		}
	}
}
