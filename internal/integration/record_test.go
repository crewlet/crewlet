package integration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
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
			Count: math.MaxInt, PersonOwes: math.MaxInt, Naming: math.MaxInt, Expiring: math.MaxInt,
			Omitted: math.MaxInt, OwedOmitted: math.MaxInt, SeatOmitted: math.MaxInt, WordsOmitted: math.MaxInt, NamesOmitted: math.MaxInt},
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

// A SURFACE'S FINDINGS ARE CUT TO WHAT THEIR RECORD KEEPS, every word before
// any name and what nobody owes before what a person does, each step only as
// far as the bound needs: agents' own words, then the vendor's, then a
// person's; then findings about nothing, the vendor's first; then subject
// lists, the reported finding's last; then the rest, the vendor's first. Each
// case is sized so the step before it is not enough and its own is, and each
// finding is counted once, by what the cut left of it. The first finding, even
// at its widest as stored, stays first and keeps its words.
//
// Mutation: swap any two adjacent steps of fitList, cut the first list
// with the others, or count a finding in two counters, and a case goes red.
func TestAPassesFindingsAreCutToTheirRecordFirstFindingWhole(t *testing.T) {
	t.Parallel()
	words := func(n int) string { return strings.Repeat("w", n) }
	agents := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s-%04d", prefix, i)
		}
		return out
	}
	refused := func(subjects int) Finding {
		return Finding{Kind: FindingCredentialMissing, Title: words(100), Detail: words(400),
			Remedy: words(400), ActionURL: "https://example.com/token", Subjects: agents("agent", subjects)}
	}
	// many is n findings f makes of each subject, or of nothing where
	// prefix is empty.
	many := func(n int, prefix string, f func(subject string) Finding) []Finding {
		out := make([]Finding, n)
		for i := range out {
			subject := ""
			if prefix != "" {
				subject = fmt.Sprintf("%s-%04d", prefix, i)
			}
			out[i] = f(subject)
		}
		return out
	}
	own := func(s string) Finding {
		return Finding{Kind: FindingApprovalRequired, Seat: true, Subject: s, Detail: words(400), Remedy: words(400)}
	}
	ownWordless := func(s string) Finding { return Finding{Kind: FindingApprovalRequired, Seat: true, Subject: s} }
	vendor := func(s string) Finding {
		return Finding{Kind: FindingGrantPending, Subject: s, Title: "grant", Detail: words(400)}
	}
	vendorWordless := func(s string) Finding { return Finding{Kind: FindingGrantPending, Subject: s} }
	admin := func(s string) Finding {
		return Finding{Kind: FindingGrantShort, Subject: s, Detail: words(300), Remedy: words(150),
			ActionURL: "https://example.com/grant"}
	}
	expiring := func(string) Finding { return Finding{Kind: FindingCredentialExpiring} }
	widest := widestFinding(FindingCredentialMissing, false, 0)
	widest.Subjects = agents("agent", 400)
	listed := func(n int) Finding {
		return Finding{Kind: FindingApprovalRequired, Subjects: agents("seat", n)}
	}
	// keepsWords is whether every kept finding of kind keeps its words.
	keepsWords := func(kept []Finding, kind FindingKind, want bool) error {
		for _, f := range kept[1:] {
			if f.Kind == kind && (f.Detail != "") != want {
				return fmt.Errorf("a kept %s keeps its words: %t, want %t", kind, !want, want)
			}
		}
		return nil
	}
	of := func(kept []Finding, kind FindingKind) int {
		n := 0
		for _, f := range kept {
			if f.Kind == kind {
				n++
			}
		}
		return n
	}
	for _, c := range []struct {
		name     string
		findings []Finding
		check    func(kept []Finding, rec FindingsRecord) error
	}{
		{"agents' own words go before the vendor's",
			slices.Concat([]Finding{refused(10)}, many(150, "agent", own), many(50, "vendor", vendor)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.SeatOmitted == 0 || rec.WordsOmitted != 0 {
					return fmt.Errorf("the cut counts %+v, want agents' own words alone left out", rec)
				}
				return keepsWords(kept, FindingGrantPending, true)
			}},
		{"the vendor's words go before a person's",
			slices.Concat([]Finding{refused(10)}, many(60, "seat", admin), many(200, "vendor", vendor)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.WordsOmitted == 0 || rec.Omitted != 0 {
					return fmt.Errorf("the cut counts %+v, want only words left out", rec)
				}
				return keepsWords(kept, FindingGrantShort, true)
			}},
		{"a person's words go before a name nobody owes",
			slices.Concat([]Finding{refused(10)}, many(150, "seat", admin), many(1, "new", vendor)),
			func(kept []Finding, rec FindingsRecord) error {
				if of(kept, FindingGrantPending) != 1 || rec.NamesOmitted != 0 || rec.WordsOmitted < 2 {
					return fmt.Errorf("the vendor's one name is kept %d times, the cut counting %+v, want it "+
						"kept and a person's words left out", of(kept, FindingGrantPending), rec)
				}
				return nil
			}},
		{"a person's words go before findings about nothing",
			slices.Concat([]Finding{refused(10)}, many(120, "seat", admin), many(700, "", vendor)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.Omitted != 0 {
					return fmt.Errorf("the cut dropped %d findings, want only words left out", rec.Omitted)
				}
				return nil
			}},
		{"findings about nothing go before any name, the vendor's first",
			slices.Concat([]Finding{refused(400)}, many(2000, "", vendorWordless), many(1000, "", expiring)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.Omitted == 0 || rec.OwedOmitted != 0 || rec.NamesOmitted != 0 || len(kept[0].Subjects) != 400 {
					return fmt.Errorf("the cut counts %+v, the first keeping %d of 400 names, want the vendor's "+
						"findings about nothing alone dropped", rec, len(kept[0].Subjects))
				}
				return nil
			}},
		{"a person's findings about nothing go before any name",
			slices.Concat([]Finding{refused(400)}, many(3000, "", expiring)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.OwedOmitted == 0 || rec.NamesOmitted != 0 || len(kept[0].Subjects) != 400 {
					return fmt.Errorf("the cut counts %+v, the first keeping %d of 400 names, want findings "+
						"about nothing dropped and every name kept", rec, len(kept[0].Subjects))
				}
				return nil
			}},
		{"subject lists are cut before a name is dropped",
			slices.Concat([]Finding{refused(10), listed(6000)}, many(100, "vendor", vendorWordless)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.Omitted != 0 || rec.NamesOmitted != 1 || kept[1].SubjectsOmitted == 0 {
					return fmt.Errorf("the cut counts %+v, the list losing %d, want one list cut and no finding "+
						"dropped", rec, kept[1].SubjectsOmitted)
				}
				return nil
			}},
		{"the reported finding's list is cut last",
			[]Finding{refused(3000), listed(3000)},
			func(kept []Finding, rec FindingsRecord) error {
				if len(kept[0].Subjects) != 3000 || kept[1].SubjectsOmitted == 0 {
					return fmt.Errorf("the first keeps %d of 3000 names and the second lost %d, want the second's "+
						"cut alone", len(kept[0].Subjects), kept[1].SubjectsOmitted)
				}
				return nil
			}},
		{"the vendor's names go before a person's",
			slices.Concat([]Finding{refused(10)}, many(400, "vendor", vendorWordless), many(2000, "agent", ownWordless)),
			func(kept []Finding, rec FindingsRecord) error {
				if of(kept, FindingGrantPending) != 0 || rec.Omitted-rec.OwedOmitted != 400 || rec.OwedOmitted == 0 {
					return fmt.Errorf("the cut counts %+v with %d of the vendor's kept, want every one of the "+
						"vendor's dropped before a person's", rec, of(kept, FindingGrantPending))
				}
				return nil
			}},
		{"a finding whose list is cut and then dropped is counted once",
			slices.Concat([]Finding{refused(10)}, many(3000, "", func(string) Finding { return listed(4) })),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.Omitted == 0 || rec.NamesOmitted != len(kept)+rec.Omitted {
					return fmt.Errorf("the cut counts %+v of %d findings, want every one hiding a name once",
						rec, len(kept)+rec.Omitted)
				}
				return nil
			}},
		{"the widest reported finding beside the vendor's findings about nothing",
			slices.Concat([]Finding{widest}, many(3000, "", vendor)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.Omitted == 0 || rec.NamesOmitted != 0 || len(kept[0].Subjects) != 400 {
					return fmt.Errorf("the cut counts %+v, the first keeping %d of 400 names, want the vendor's "+
						"dropped and the widest finding whole", rec, len(kept[0].Subjects))
				}
				return nil
			}},
		{"more agents waiting on a person than the record holds",
			slices.Concat([]Finding{refused(400)}, many(2000, "agent", own)),
			func(kept []Finding, rec FindingsRecord) error {
				if rec.NamesOmitted != rec.Omitted+1 || rec.Omitted == 0 || len(kept[0].Subjects) == 400 {
					return fmt.Errorf("the cut counts %+v, the first keeping %d names, want the first's list cut "+
						"too", rec, len(kept[0].Subjects))
				}
				return nil
			}},
	} {
		_, all := bound(Classify(c.findings), Promote(c.findings))
		if all[0].Kind != FindingCredentialMissing {
			t.Fatalf("%s: premise: the promoted finding is %s, want the credential's", c.name, all[0].Kind)
		}
		if !newListSize(all).over() {
			t.Fatalf("%s: premise: the pass fits its record whole", c.name)
		}
		kept, rec := FitFindings(all)
		raw, err := encodeFindings(kept)
		if err != nil || len(raw) > coord.MaxIntegrationFindingsBytes {
			t.Fatalf("%s: stored as %d bytes (%v), want within %d", c.name, len(raw), err,
				coord.MaxIntegrationFindingsBytes)
		}
		first := kept[0]
		first.Subjects, first.SubjectsOmitted = all[0].Subjects, 0
		if !first.Same(all[0]) {
			t.Errorf("%s: the first finding kept is %s, want the promoted one with its words", c.name, kept[0].Kind)
		}
		if want := cutOf(all, kept); rec != want {
			t.Errorf("%s: the cut counts %+v, want each finding once by what was left of it: %+v", c.name, rec, want)
		}
		if err := c.check(kept, rec); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}

// cutOf is what a cut of all to kept left out, each finding counted once by
// what is left of it, matched in order by kind, subject and seat.
func cutOf(all, kept []Finding) FindingsRecord {
	var rec FindingsRecord
	j := 0
	for _, f := range all {
		if j < len(kept) && kept[j].Kind == f.Kind && kept[j].Subject == f.Subject && kept[j].Seat == f.Seat {
			k := kept[j]
			j++
			listCut := len(k.Subjects) < len(f.Subjects)
			if f.Title+f.Detail+f.Remedy+f.ActionURL != "" && k.Title+k.Detail+k.Remedy+k.ActionURL == "" {
				if f.Seat {
					rec.SeatOmitted++
				} else {
					rec.WordsOmitted++
				}
			}
			if listCut && mayNameSeat(f) {
				rec.NamesOmitted++
			}
			continue
		}
		rec.Omitted++
		if personOwed(f) {
			rec.OwedOmitted++
		}
		if mayNameSeat(f) {
			rec.NamesOmitted++
		}
	}
	return rec
}

// A PASS OF ANY SIZE IS CUT IN LINEAR WORK: nothing bounds a company's seats,
// and the cut asks whether the list is past its bound after every finding it
// cuts, so each ask reads the list's running length, never every finding's
// again. 100,000 agents' own findings are cut reading a few lengths apiece,
// and the running length is the stored one.
//
// Mutation: have over() sum every length, and the lengths read grow with the
// square of the findings; drop no comma with a finding, and the length is off.
func TestAPassOfAnySizeIsCutInLinearWork(t *testing.T) {
	t.Parallel()
	findings := []Finding{{Kind: FindingCredentialMissing}}
	for i := range 100_000 {
		findings = append(findings, Finding{Kind: FindingApprovalRequired, Seat: true,
			Subject: fmt.Sprintf("agent-%06d", i), Detail: strings.Repeat("d", 200)})
	}
	l := newListSize(findings)
	kept, rec := fitList(findings, l)
	raw, err := encodeFindings(kept)
	if err != nil || len(raw) > coord.MaxIntegrationFindingsBytes || rec.Omitted == 0 {
		t.Fatalf("stored as %d bytes (%v), %d dropped; want a cut within %d", len(raw), err, rec.Omitted,
			coord.MaxIntegrationFindingsBytes)
	}
	if l.total != len(raw) {
		t.Errorf("the running length is %d, the stored one %d", l.total, len(raw))
	}
	if most := 4 * len(findings); l.read > most {
		t.Errorf("the cut read %d lengths of %d findings, want at most %d", l.read, len(findings), most)
	}
}

// A PASS THAT FITS IS KEPT WHOLE, uncut and uncounted: an agent's own finding
// after the first is kept, its record having room.
//
// Mutation: cut a list that fits, and the agent's finding is folded away.
func TestAPassThatFitsItsRecordIsKeptWhole(t *testing.T) {
	t.Parallel()
	findings := []Finding{{Kind: FindingIdentityMissing, Subjects: []string{"a", "b"}},
		{Kind: FindingGrantShort, Subject: "sre", Seat: true, Detail: "sre needs more"}}
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

// A FOLD THAT FOUND NOTHING REMOVES THE SURFACE'S FINDINGS, named by its
// status or not: a disconnect asked for over a pass with findings leaves none
// behind it, nor does a pass over a record a failed write left unnamed; and a
// status write alone (an address stamped) keeps the findings it names.
//
// Mutation: write the status alone where the fold found nothing and the
// status named no record, and the unnamed record stays.
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
	// A RECORD NO STATUS NAMES, as a write that failed between its two
	// keys leaves one, goes with the next pass that found nothing.
	if err := fleet.RecordIntegrationPass(ctx, "slack", []byte(`{}`), []byte(`[{"kind":"grant_short"}]`)); err != nil {
		t.Fatal(err)
	}
	unnamed, _, err := store.LoadIntegration(ctx, KindSlack)
	if err != nil || unnamed.FindingsRecord.Named() {
		t.Fatalf("premise: the status reads %+v (%v), want it naming no record", unnamed, err)
	}
	quiet, _ := Observe(unnamed, KindSlack, nil, nil, widestInstant)
	if err := store.RecordPass(ctx, quiet); err != nil {
		t.Fatalf("RecordPass: %v", err)
	}
	if got, found, _ := fleet.IntegrationFindingsOf(ctx, "slack"); found {
		t.Errorf("a pass that found nothing left a record no status names: %s", got)
	}
}

// failingFindings is the coordination twin whose findings pass fails.
type failingFindings struct{ *memory.Fleet }

func (failingFindings) IntegrationFindings(context.Context) (map[string][]byte, error) {
	return nil, errors.New("the findings pass outlasted its crossing")
}

// A FINDINGS PASS THAT FAILS LEAVES THE STATUSES STANDING: each surface's
// phase is answered as read, and its findings, where it names some, read as
// unread, never as none and never as a status nobody could read.
//
// Mutation: fail the whole read where the findings pass fails, or leave the
// unread mark off, and a check below fails.
func TestAFailedFindingsPassReadsTheFindingsUnread(t *testing.T) {
	t.Parallel()
	fleet := failingFindings{memory.NewFleet()}
	store := newCoordStore(t, fleet)
	ctx := t.Context()
	named, _ := Observe(State{}, KindGitLab, []Finding{{Kind: FindingGrantShort, Subject: "sre"}}, nil,
		widestInstant)
	quiet, _ := Observe(State{}, KindSlack, nil, nil, widestInstant)
	for _, state := range []State{named, quiet} {
		if err := store.RecordPass(ctx, state); err != nil {
			t.Fatalf("RecordPass: %v", err)
		}
	}
	states, err := store.LoadIntegrationsWithFindings(ctx)
	if err != nil || len(states) != 2 {
		t.Fatalf("the statuses with a failed findings pass read %+v (%v), want both", states, err)
	}
	for _, state := range states {
		if want := state.Kind == KindGitLab; state.FindingsUnread != want || state.Report.Phase == "" {
			t.Errorf("%s reads unread %t with phase %q, want unread %t and its phase", state.Kind,
				state.FindingsUnread, state.Report.Phase, want)
		}
	}
}
