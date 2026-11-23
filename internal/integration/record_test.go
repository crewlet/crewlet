package integration

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// widestInstant is an instant at its longest as a record encodes it: UTC, to
// the nanosecond.
var widestInstant = time.Date(2026, 11, 22, 10, 0, 0, 123456789, time.UTC)

// sixFold is n bytes of a control character, which JSON writes as six each
// whatever its escaping: the most a byte of text grows as a record stores it.
func sixFold(n int) string { return strings.Repeat("\x01", n) }

// widestLink is the longest link a record keeps ([MaxActionURLBytes]), as
// encoded.
func widestLink() string { return strings.Repeat("a", MaxActionURLBytes) }

// longest is the longest of names.
func longest[T ~string](names []T) T {
	var most T
	for _, n := range names {
		if len(n) > len(most) {
			most = n
		}
	}
	return most
}

// THE WIDEST STATUS FITS ITS BOUND, as the store encodes it, keeping exact the
// address and the registration's name a reader compares. Every field at the
// most its writer keeps: a fault of MaxLastErrorLength and a report's sentence
// of MaxDetailLength, each in bytes JSON writes as six, the longest link a
// report keeps, an address at config.MaxExternalURLLength (2,048) and a name
// at config.MaxWebhookNameLength (255) in characters JSON writes as six, a requester at iam.MaxLogin, every instant to the nanosecond and every
// count at its most. Whole, it is past the bound; the writer's cut (the
// fault, then the sentence, then the link) brings it within.
//
// Mutation: drop a step of encodeStatus's cut, or cut the address, and the
// status is refused or the address is not kept.
func TestTheWidestStatusFitsItsBound(t *testing.T) {
	t.Parallel()
	report, _ := bound(Report{Phase: longest(Phases), Actor: ActorProvider,
		Detail: sixFold(MaxDetailLength + 10), ActionURL: widestLink()}, nil)
	address := strings.Repeat("\u2028", config.MaxExternalURLLength)
	name := strings.Repeat("\u2028", config.MaxWebhookNameLength)
	state := State{
		Kind: longest(Kinds), Report: report, Outcome: OutcomeSettled, Attempts: math.MaxInt,
		LastError:      truncateError(sixFold(MaxLastErrorLength + 10)),
		TeardownHeldBy: TeardownHeldBySettings,
		LastAttemptAt:  widestInstant, SettledAt: widestInstant, NextAttemptAt: widestInstant,
		Endpoint: address, Registration: name, Disconnecting: true, RemoveSeats: true,
		RequestedBy: Requester{Name: strings.Repeat("n", iam.MaxLogin), Kind: iam.ActorOperator,
			OperatorID: "pat:" + strings.Repeat("o", iam.MaxLogin)},
		FindingsRecord: FindingsRecord{Bytes: coord.MaxIntegrationFindingsBytes, SHA256: strings.Repeat("f", 64),
			Count: math.MaxInt, PersonOwes: math.MaxInt, Omitted: math.MaxInt, SeatOmitted: math.MaxInt},
	}
	whole, err := encodeRecord(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(whole) <= coord.MaxIntegrationStatusBytes {
		t.Fatalf("premise: the widest status whole is %d bytes, within its bound %d, so no cut is held",
			len(whole), coord.MaxIntegrationStatusBytes)
	}
	stored, cut, err := encodeStatus(state)
	if err != nil || !cut || len(stored) > coord.MaxIntegrationStatusBytes {
		t.Fatalf("the widest status is stored as %d bytes (cut %t, %v), want within its bound %d", len(stored),
			cut, err, coord.MaxIntegrationStatusBytes)
	}
	read, ok := decodeStatus(t.Context(), state.Kind, stored)
	if !ok || read.Endpoint != address || read.Registration != name || read.RequestedBy != state.RequestedBy ||
		read.FindingsRecord != state.FindingsRecord {
		t.Errorf("the widest status lost what a reader compares: endpoint kept %t, registration kept %t, "+
			"requester %+v, findings record %+v", read.Endpoint == address, read.Registration == name,
			read.RequestedBy, read.FindingsRecord)
	}
	t.Logf("the widest status is %d bytes whole and %d stored", len(whole), len(stored))
}

// widestFinding is a finding with every field at the most its record keeps
// ([bound]): its texts in bytes JSON writes as six, its link at its longest,
// and subjects many.
func widestFinding(kind FindingKind, seat bool, subjects int) Finding {
	f := Finding{Kind: kind, Subject: sixFold(MaxDetailLength + 10), Seat: seat,
		Title: sixFold(MaxDetailLength + 10), Detail: sixFold(MaxDetailLength + 10),
		Remedy: sixFold(MaxDetailLength + 10), ActionURL: widestLink(), ExpiresAt: widestInstant}
	for i := range subjects {
		f.Subjects = append(f.Subjects, fmt.Sprintf("%s-%06d", sixFold(MaxDetailLength-7), i))
	}
	return f
}

// A SURFACE'S FINDINGS ARE CUT TO WHAT THEIR RECORD KEEPS, and say what they
// left out. A pass over 400 agents at their widest (each finding of its
// fields at what [bound] keeps), the worst finding about every one of them,
// is kept within coord.MaxIntegrationFindingsBytes as stored: the promoted
// finding first, its subject list cut and counted; every agent's own finding
// folded into a count; the rest dropped from the tail and counted. Every
// finding is kept, left out or counted, never lost uncounted.
//
// Mutation: let the tail cut drop the first finding, skip folding an agent's
// own, or count none of what is cut, and a check below fails.
func TestAPassesFindingsAreCutToTheirRecordFirstFindingWhole(t *testing.T) {
	t.Parallel()
	const agents = 400
	var findings []Finding
	for i := range agents {
		findings = append(findings, widestFinding(FindingGrantPending, true, 0))
		findings[len(findings)-1].Subject = fmt.Sprintf("agent-%03d", i)
		findings = append(findings, widestFinding(FindingGrantShort, false, 3))
	}
	findings = append(findings, widestFinding(FindingCredentialMissing, false, agents))
	report, all := bound(Classify(findings), Promote(findings))
	if all[0].Kind != FindingCredentialMissing {
		t.Fatalf("premise: the promoted finding is %s, want the credential's", all[0].Kind)
	}
	kept, rec := FitFindings(all)
	raw, err := encodeFindings(kept)
	if err != nil || len(raw) > coord.MaxIntegrationFindingsBytes {
		t.Fatalf("the cut findings are stored as %d bytes (%v), want within their bound %d", len(raw), err,
			coord.MaxIntegrationFindingsBytes)
	}
	if len(kept) == 0 || kept[0].Kind != all[0].Kind || kept[0].Detail != all[0].Detail ||
		kept[0].Remedy != all[0].Remedy || kept[0].ActionURL != all[0].ActionURL {
		t.Fatalf("the first finding kept is %+v, want the promoted one whole", kept[0])
	}
	if got := len(kept[0].Subjects) + kept[0].SubjectsOmitted; got != agents {
		t.Errorf("the promoted finding keeps %d subjects and counts %d left out, want %d in all",
			len(kept[0].Subjects), kept[0].SubjectsOmitted, agents)
	}
	if rec.SeatOmitted != agents {
		t.Errorf("the cut folded %d agents' own findings, want all %d", rec.SeatOmitted, agents)
	}
	if got := len(kept) + rec.Omitted + rec.SeatOmitted; got != len(all) {
		t.Errorf("kept %d, dropped %d and folded %d of %d findings: some were lost uncounted", len(kept),
			rec.Omitted, rec.SeatOmitted, len(all))
	}
	for _, f := range kept {
		if f.Seat {
			t.Errorf("an agent's own finding about %q was kept past the cut", f.Subject)
		}
	}
	if rec.Omitted == 0 {
		t.Error("premise: the pass fit without dropping from the tail, so that step is not held")
	}
	// CLASSIFIED BEFORE THE CUT: the report names the promoted finding.
	if report.Phase != PhaseUnconfigured {
		t.Errorf("the report is %s, want the promoted finding's phase", report.Phase)
	}
}

// A PASS THAT FITS IS KEPT WHOLE, uncut and uncounted: an agent's own finding
// after the first is kept, its record having room.
//
// Mutation: cut a list that fits, and the agent's finding is folded away.
func TestAPassThatFitsItsRecordIsKeptWhole(t *testing.T) {
	t.Parallel()
	findings := []Finding{{Kind: FindingIdentityMissing, Subjects: []string{"a", "b"}},
		{Kind: FindingGrantShort, Subject: "sre", Seat: true}}
	kept, rec := FitFindings(findings)
	if !SameAll(kept, findings) || rec != (FindingsRecord{}) {
		t.Errorf("a pass that fits was kept as %+v with %+v, want it whole and nothing counted", kept, rec)
	}
}

// countingFleet is the coordination twin, counting every read of a findings
// record.
type countingFleet struct {
	*memory.Fleet
	findingsReads int
}

func (f *countingFleet) IntegrationFindings(ctx context.Context) (map[string][]byte, error) {
	f.findingsReads++
	return f.Fleet.IntegrationFindings(ctx)
}

func (f *countingFleet) IntegrationFindingsOf(ctx context.Context, kind string) ([]byte, bool, error) {
	f.findingsReads++
	return f.Fleet.IntegrationFindingsOf(ctx, kind)
}

// A STATUS READ READS NO FINDINGS, by the pass a duty's tick, a turn's atlas
// and every reader of the statuses alone make, or by its key as a write's
// re-read and the mint judgement make; only the readers that show them read
// the findings, attached where the status names them.
//
// Mutation: have LoadIntegrations or LoadIntegration attach the findings, and
// a findings read is counted.
func TestAStatusReadReadsNoFindings(t *testing.T) {
	t.Parallel()
	fleet := &countingFleet{Fleet: memory.NewFleet()}
	store := newCoordStore(t, fleet)
	ctx := t.Context()
	state, _ := Observe(State{}, KindGitLab, []Finding{{Kind: FindingGrantShort, Subject: "sre"}}, nil,
		widestInstant)
	if err := store.RecordPass(ctx, state); err != nil {
		t.Fatalf("RecordPass: %v", err)
	}
	rows, err := store.LoadIntegrations(ctx)
	if err != nil || len(rows) != 1 || rows[0].Findings != nil || !rows[0].FindingsRecord.Named() {
		t.Fatalf("the status pass reads %+v (%v), want gitlab's status naming its findings", rows, err)
	}
	one, found, err := store.LoadIntegration(ctx, KindGitLab)
	if err != nil || !found || one.Findings != nil || one.FindingsRecord.PersonOwes != 1 {
		t.Fatalf("the status by key reads %+v (%t, %v), want it alone, a person owing one finding", one, found,
			err)
	}
	if fleet.findingsReads != 0 {
		t.Errorf("reading the statuses read a findings record %d times, want none", fleet.findingsReads)
	}
	shown, err := store.LoadIntegrationsWithFindings(ctx)
	if err != nil || len(shown) != 1 || len(shown[0].Findings) != 1 || shown[0].Findings[0].Subject != "sre" {
		t.Errorf("the statuses with their findings read %+v (%v), want gitlab's finding attached", shown, err)
	}
	if fleet.findingsReads != 1 {
		t.Errorf("reading the findings took %d reads, want one pass", fleet.findingsReads)
	}
}

// FINDINGS THEIR STATUS DOES NOT NAME READ AS UNREAD, never as another pass's:
// a record whose bytes differ from the size and digest the status names (a
// pass wrote between a reader's two reads) is not attached.
//
// Mutation: attach the findings without comparing their digest, and the other
// pass's findings are read as this status's.
func TestFindingsTheirStatusDoesNotNameReadAsUnread(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := newCoordStore(t, fleet)
	ctx := t.Context()
	state, _ := Observe(State{}, KindGitLab, []Finding{{Kind: FindingGrantShort, Subject: "sre"}}, nil,
		widestInstant)
	if err := store.RecordPass(ctx, state); err != nil {
		t.Fatalf("RecordPass: %v", err)
	}
	status, _, err := fleet.IntegrationStatus(ctx, "gitlab")
	if err != nil {
		t.Fatal(err)
	}
	other := []byte(`[{"kind":"grant_short","subject":"cto"}]`)
	if err := fleet.RecordIntegrationPass(ctx, "gitlab", status, other); err != nil {
		t.Fatal(err)
	}
	shown, err := store.LoadIntegrationsWithFindings(ctx)
	if err != nil || len(shown) != 1 || shown[0].Findings != nil || !shown[0].FindingsUnread {
		t.Errorf("findings the status does not name read as %+v (%v), want them unread", shown, err)
	}
	read, err := store.LoadFindings(ctx, shown[0])
	if err != nil || read.Findings != nil || !read.FindingsUnread {
		t.Errorf("findings the status does not name read by key as %+v (%v), want them unread", read, err)
	}
}

// A FOLD THAT FOUND NOTHING REMOVES THE FINDINGS ITS STATUS NAMED: a
// disconnect asked for over a pass with findings leaves none behind it, and a
// status write alone (an address stamped) keeps the findings it names.
//
// Mutation: let a fold forget the record its state was read naming
// (FindingsRecord.refold keeping only the counts), and the findings stay.
func TestAFoldThatFoundNothingRemovesTheFindingsItsStatusNamed(t *testing.T) {
	t.Parallel()
	fleet := memory.NewFleet()
	store := newCoordStore(t, fleet)
	ctx := t.Context()
	state, _ := Observe(State{}, KindGitLab, []Finding{{Kind: FindingGrantShort, Subject: "sre"}}, nil,
		widestInstant)
	if err := store.RecordPass(ctx, state); err != nil {
		t.Fatalf("RecordPass: %v", err)
	}
	read, _, err := store.LoadIntegration(ctx, KindGitLab)
	if err != nil {
		t.Fatal(err)
	}
	read.Endpoint = "https://hooks.example.com"
	if err := store.SaveIntegration(ctx, read); err != nil {
		t.Fatalf("SaveIntegration: %v", err)
	}
	if _, found, _ := fleet.IntegrationFindingsOf(ctx, "gitlab"); !found {
		t.Fatal("a status written alone dropped the findings it names")
	}
	asked := AskTeardown(read, KindGitLab, false, iam.Actor{Name: "ana", Kind: iam.ActorOperator})
	if err := store.RecordPass(ctx, asked); err != nil {
		t.Fatalf("RecordPass: %v", err)
	}
	if got, found, _ := fleet.IntegrationFindingsOf(ctx, "gitlab"); found {
		t.Errorf("a disconnect over a pass with findings left them: %s", got)
	}
	if err := store.SaveIntegration(ctx, State{Kind: KindGitLab, Findings: []Finding{{}}}); err == nil {
		t.Error("a status carrying a fold's findings was written alone, naming a record holding others")
	}
}
