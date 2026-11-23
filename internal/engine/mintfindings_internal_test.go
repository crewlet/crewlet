package engine

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
)

// onCallersPath marks the context of a reading whose window must keep the
// read term: the beat's, a boot's.
type onCallersPath struct{}

// findingsFleet is the coordination twin, counting every findings read and
// holding each until released; one made on a caller's path ([onCallersPath])
// is refused and counted apart, never held.
type findingsFleet struct {
	innerFleet
	findingsReads, onPath atomic.Int32
	release               chan struct{}
}

func (f *findingsFleet) IntegrationFindings(ctx context.Context) (map[string][]byte, error) {
	if err := f.findingsRead(ctx); err != nil {
		return nil, err
	}
	return f.innerFleet.IntegrationFindings(ctx)
}

func (f *findingsFleet) IntegrationFindingsOf(ctx context.Context, kind string) ([]byte, bool, error) {
	if err := f.findingsRead(ctx); err != nil {
		return nil, false, err
	}
	return f.innerFleet.IntegrationFindingsOf(ctx, kind)
}

func (f *findingsFleet) findingsRead(ctx context.Context) error {
	f.findingsReads.Add(1)
	if ctx.Value(onCallersPath{}) != nil {
		f.onPath.Add(1)
		return errors.New("a findings record read on a caller's path")
	}
	select {
	case <-f.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// THE MINT JUDGEMENT READS NO FINDINGS ON ITS CALLER'S PATH: the beat's MCP
// rows, and the boot's and an apply's shared servers through the same
// [Engine.mintingStates], read each minting integration's status by its key
// and keep the read term. A status naming findings a person owes is judged
// from the copy [mintFindings] holds; one it does not hold is read on the
// holder's own context, this reading holding the child as it cannot say
// (never waiting), and the next reading, once the read has landed, judges it
// stopped. Decided by signals: the context a read
// carries, and the holder telling a read's end.
//
// Mutation: read the findings in mintingStates on its caller's context
// (store.LoadFindings(ctx, state)), and a read on the beat's path is counted.
func TestTheMintJudgementReadsNoFindingsOnItsCallersPath(t *testing.T) {
	t.Parallel()
	fleet := &findingsFleet{innerFleet: coordmemory.NewFleet(), release: make(chan struct{})}
	store, err := integration.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	state, _ := integration.Observe(integration.State{}, integration.KindGitHub, []integration.Finding{
		{Kind: integration.FindingIdentityFailed, Subject: "pm", Detail: "its app could not be created"},
	}, nil, at)
	if err := store.RecordPass(t.Context(), state); err != nil {
		t.Fatalf("record a pass a person owes a finding of: %v", err)
	}
	pm := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	e := &Engine{backends: &Backends{Fleet: fleet}, mcpSeats: map[string][]mcpOutcome{
		"pm": {{server: "tracker", seat: pm, err: &credentialMissing{seat: "pm", server: "tracker",
			minting: map[string]integration.Kind{"TRACKER_TOKEN": integration.KindGitHub}}}},
	}}
	fetched := make(chan integration.Kind, 1)
	e.mintFindings.fetched = func(kind integration.Kind) { fetched <- kind }
	t.Cleanup(e.mintFindings.stop)

	beat := context.WithValue(t.Context(), onCallersPath{}, true)
	rows := e.mcpStatus(beat)
	if len(rows) != 1 || rows[0].Waiting != 0 || !strings.Contains(rows[0].Error, "cannot be read") {
		t.Fatalf("the first reading's rows = %+v, want the child held: its findings are not in hand", rows)
	}
	if n := fleet.onPath.Load(); n != 0 {
		t.Fatalf("the judgement read a findings record %d times on its caller's path, want never", n)
	}
	close(fleet.release)
	if kind := <-fetched; kind != integration.KindGitHub {
		t.Fatalf("the holder read %s's findings, want github's", kind)
	}
	rows = e.mcpStatus(beat)
	if len(rows) != 1 || rows[0].Waiting != 0 || rows[0].Failed != 1 {
		t.Errorf("the next reading's rows = %+v, want the child failed: a person owes github a finding about pm",
			rows)
	}
	if n := fleet.onPath.Load(); n != 0 {
		t.Errorf("the next reading read a findings record %d times on its caller's path, want never", n)
	}
	if n := fleet.findingsReads.Load(); n != 1 {
		t.Errorf("the findings were read %d times, want once, held under their digest after", n)
	}
}

// statusesFleet is the coordination twin, counting every findings read.
type statusesFleet struct {
	innerFleet
	findingsReads atomic.Int32
}

func (f *statusesFleet) IntegrationFindings(ctx context.Context) (map[string][]byte, error) {
	f.findingsReads.Add(1)
	return f.innerFleet.IntegrationFindings(ctx)
}

func (f *statusesFleet) IntegrationFindingsOf(ctx context.Context, kind string) ([]byte, bool, error) {
	f.findingsReads.Add(1)
	return f.innerFleet.IntegrationFindingsOf(ctx, kind)
}

// THE ATLAS READS THE INTEGRATIONS' STATUSES ALONE: a turn's prompt prefetch
// lists them every turn, and reads the address, outcome and fault on each,
// never the findings, which would put every surface's crossing at its bound
// on the turn.
//
// Mutation: hand the atlas a reader that attaches the findings
// (LoadIntegrationsWithFindings), and a findings read is counted.
func TestTheAtlasReadsTheIntegrationsStatusesAlone(t *testing.T) {
	t.Parallel()
	fleet := &statusesFleet{innerFleet: coordmemory.NewFleet()}
	store, err := integration.NewCoordStore(fleet)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := integration.Observe(integration.State{}, integration.KindGitHub, []integration.Finding{
		{Kind: integration.FindingIdentityFailed, Subject: "pm"},
	}, nil, time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC))
	if err := store.RecordPass(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	src := (&Engine{backends: &Backends{Fleet: fleet}}).atlasRecords(t.Context())
	states, err := src.Integrations.LoadIntegrations(t.Context())
	if err != nil || len(states) != 1 || states[0].Kind != integration.KindGitHub {
		t.Fatalf("the atlas reads %+v (%v), want github's status", states, err)
	}
	if n := fleet.findingsReads.Load(); n != 0 {
		t.Errorf("the atlas read a findings record %d times, want none", n)
	}
}
