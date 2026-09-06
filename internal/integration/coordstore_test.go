package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
)

// A status survives the round trip through the real coordination twin with
// every field the API renders intact. A phase that came back empty would
// render as an integration nobody has ever looked at.
func TestCoordStoreRoundTrip(t *testing.T) {
	store := NewCoordStore(memory.NewFleet())
	ctx := context.Background()
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	want := State{
		Kind: KindGitLab,
		Report: Report{
			Phase: PhaseDegraded, Actor: ActorAdmin,
			Detail: "ceo needs maintainer on api-gateway", ActionURL: "https://example.com/x",
		},
		Findings:      []Finding{{Kind: FindingGrantShort, Subject: "ceo", Detail: "needs maintainer"}},
		Outcome:       OutcomeBlocked,
		Attempts:      3,
		LastError:     "gitlab: 502 from the instance",
		LastAttemptAt: now,
		SettledAt:     now.Add(-time.Hour),
		NextAttemptAt: now.Add(time.Minute),
	}
	if err := store.SaveIntegration(ctx, want); err != nil {
		t.Fatalf("SaveIntegration: %v", err)
	}

	rows, err := store.LoadIntegrations(ctx)
	if err != nil {
		t.Fatalf("LoadIntegrations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read back %d rows, want 1", len(rows))
	}
	got := rows[0]
	switch {
	case got.Kind != want.Kind:
		t.Errorf("kind is %q, want %q", got.Kind, want.Kind)
	case got.Report != want.Report:
		t.Errorf("report is %+v, want %+v", got.Report, want.Report)
	case got.Outcome != want.Outcome:
		t.Errorf("outcome is %q, want %q", got.Outcome, want.Outcome)
	case got.Attempts != want.Attempts:
		t.Errorf("attempts is %d, want %d", got.Attempts, want.Attempts)
	case got.LastError != want.LastError:
		t.Errorf("last error is %q, want %q", got.LastError, want.LastError)
	case len(got.Findings) != 1 || got.Findings[0] != want.Findings[0]:
		t.Errorf("findings are %+v, want %+v", got.Findings, want.Findings)
	case !got.LastAttemptAt.Equal(want.LastAttemptAt):
		t.Errorf("last attempt is %s, want %s", got.LastAttemptAt, want.LastAttemptAt)
	case !got.SettledAt.Equal(want.SettledAt):
		t.Errorf("settled at is %s, want %s", got.SettledAt, want.SettledAt)
	case !got.NextAttemptAt.Equal(want.NextAttemptAt):
		t.Errorf("next attempt is %s, want %s", got.NextAttemptAt, want.NextAttemptAt)
	}
}

// A status this build cannot decode does not stop every other surface from
// being reconciled. It is dropped, the loop reads that surface as never
// reconciled, and the next pass overwrites it.
func TestCoordStoreSkipsAnUndecodableStatus(t *testing.T) {
	fleet := memory.NewFleet()
	ctx := context.Background()
	if err := fleet.PutIntegrationStatus(ctx, "gitlab", []byte("{not json")); err != nil {
		t.Fatalf("PutIntegrationStatus: %v", err)
	}
	store := NewCoordStore(fleet)
	if err := store.SaveIntegration(ctx, State{Kind: KindSlack, Report: Ready()}); err != nil {
		t.Fatalf("SaveIntegration: %v", err)
	}

	rows, err := store.LoadIntegrations(ctx)
	if err != nil {
		t.Fatalf("LoadIntegrations: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != KindSlack {
		t.Fatalf("read back %+v, want only the slack row", rows)
	}
}

// The key the value was looked up by names the surface, not the field inside
// the document. Trusting the field would let a status written under the wrong
// key rename another surface's row.
func TestCoordStoreTrustsTheKeyOverTheDocument(t *testing.T) {
	fleet := memory.NewFleet()
	ctx := context.Background()
	if err := fleet.PutIntegrationStatus(ctx, "slack", []byte(`{"kind":"gitlab"}`)); err != nil {
		t.Fatalf("PutIntegrationStatus: %v", err)
	}

	rows, err := NewCoordStore(fleet).LoadIntegrations(ctx)
	if err != nil {
		t.Fatalf("LoadIntegrations: %v", err)
	}
	if len(rows) != 1 || rows[0].Kind != KindSlack {
		t.Fatalf("read back %+v, want the row named by its key", rows)
	}
}

// A store that cannot be reached raises rather than answering empty. An empty
// answer reads as "no surface has ever been reconciled", which sends the loop
// to re-provision a company it cannot currently see.
func TestCoordStoreRaisesOnAnUnreachableStore(t *testing.T) {
	_, err := NewCoordStore(brokenStatuses{}).LoadIntegrations(context.Background())
	if err == nil {
		t.Fatal("an unreachable store answered with no error")
	}
}

// A surface with no name is refused rather than written under an empty key,
// where nothing would ever read it back.
func TestCoordStoreRefusesAnUnnamedSurface(t *testing.T) {
	err := NewCoordStore(memory.NewFleet()).SaveIntegration(context.Background(), State{})
	if err == nil {
		t.Fatal("a status with no surface was accepted")
	}
}

type brokenStatuses struct{}

func (brokenStatuses) IntegrationStatuses(context.Context) (map[string][]byte, error) {
	return nil, errors.New("the coordination store could not be reached")
}
func (brokenStatuses) PutIntegrationStatus(context.Context, string, []byte) error { return nil }
func (brokenStatuses) DeleteIntegrationStatus(context.Context, string) error      { return nil }
