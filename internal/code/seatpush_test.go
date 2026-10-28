package code

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/period"
)

// A SEAT'S PUSH THROUGH THE DECIDE: its grant (rule 2b), the protections that
// take a person's push alone (rule 5), its share of a repository outside its
// owners (rules 7 and repo_size) and its company day's quota — each over the
// rows an applier wrote, decided by decideBatch as the committer decides one,
// with the chart and the calendar of the deciding epoch.

// seatChart is the chart the cases judge a seat by, as the decide reads one
// ([Roster]): decideRepo's owner ENG, led by eng-lead, with dev at
// full_access and reader at read_only in it; Sales, led by vp-sales, with
// outsider at full_access in it; and founder, a person's human seat. Two
// top-level units, each referenced by its key.
type seatChart struct{}

func (seatChart) Seat(handle string) (RosterSeat, bool) {
	s, ok := map[string]RosterSeat{
		"eng-lead": {Handle: "eng-lead", Tier: TierFullAccess, Unit: "ENG"},
		"dev":      {Handle: "dev", Tier: TierFullAccess, Unit: "ENG"},
		"reader":   {Handle: "reader", Tier: TierReadOnly, Unit: "ENG"},
		"vp-sales": {Handle: "vp-sales", Tier: TierFullAccess, Unit: "Sales"},
		"outsider": {Handle: "outsider", Tier: TierFullAccess, Unit: "Sales"},
		"founder":  {Handle: "founder", Human: true, Tier: TierFullAccess, Unit: "ENG"},
	}[handle]
	return s, ok
}

func (seatChart) Lead(unit string) string {
	return map[string]string{"eng": "eng-lead", "sales": "vp-sales"}[strings.ToLower(unit)]
}

func (c seatChart) Leads(unit string) []string {
	if lead := c.Lead(unit); lead != "" {
		return []string{lead}
	}
	return nil
}

func (seatChart) Within(unit, ancestor string) bool {
	return unit != "" && strings.EqualFold(unit, ancestor)
}

// onTheChart is the deciding epoch's chart and calendar the cases decide a
// seat's push under: seatChart, on UTC's.
var onTheChart = SeatRules{Roster: seatChart{}, Zone: time.UTC}

// seatOf is the principal of seat handle's push, with the run the engine
// stamps on it.
func seatOf(handle string) Principal {
	return Principal{Kind: PrincipalAgent, Seat: handle,
		Run: RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001", WorkBackend: "native", WorkKey: "ENG-7"}}
}

// founderOf is the founder's principal: a person, through their own token.
var founderOf = Principal{Kind: PrincipalOperator, OperatorID: "pat:founder-laptop", Seat: "founder"}

// pushBy is one undecided push of cmds to decideRepo by p, with no pack.
func pushBy(p Principal, cmds ...Command) Proposal {
	out := proposalOf(cmds...)
	out.Principal = p
	return out
}

// decideSeats runs one round of the decide over e's rows on the chart's
// epoch at the decide's clock, failing t on an error.
func decideSeats(t *testing.T, e *estate, limits Limits, batch ...Proposal) ([]PushEntry, []decided) {
	t.Helper()
	entries, answers, err := decideErr(e, decideRepo, limits, DefaultDeletedRetention, onTheChart, decideAt,
		&verdicts{}, batch...)
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	return entries, answers
}

// THE DECIDE REFUSES WHAT THE GRANT REFUSES (decide rule 2b, ADR-0058): every
// command of every kind — a branch created, fast-forwarded and deleted, a tag
// created and deleted, a command under the pusher's own namespace, one under
// another seat's, and one under refs/heads/<its handle>/, which is NOT its
// namespace — is answered exactly as [Grant.RefRule] answers it for the
// principal on the deciding epoch's chart: refused not_permitted with the
// rule's own sentence, or decided by the rules after it, which every command
// here passes. A refusal is RECORDED — on its command, in the entry of a push
// that has an ok command left — and a push left with none is no entry. The
// principals are a full_access seat of the owning unit, which writes; a
// read_only seat of it, which reads; a full_access seat of another unit,
// which writes its own namespace on a company repository and nothing on a
// `unit` one; the owning unit's lead, which maintains; and a person, who
// maintains every repository with no chart.
//
// Mutation: skip rule 2b — the seats' deletions and the read_only seat's
// creates are ok, red.
func TestTheDecideRefusesWhatTheGrantRefuses(t *testing.T) {
	t.Parallel()
	commands := func(handle string) []Command {
		return []Command{
			cmd("refs/heads/new", ZeroOID, oid("N")),
			cmd("refs/heads/feature", oid("F"), oid("G")),
			cmd("refs/heads/old", oid("O"), ZeroOID),
			cmd("refs/tags/v2", ZeroOID, oid("T2")),
			cmd("refs/tags/v0", oid("V"), ZeroOID),
			cmd(SeatNamespace(handle)+"work", ZeroOID, oid("S")),
			cmd(SeatNamespace("somebody")+"work", ZeroOID, oid("S2")),
			cmd("refs/heads/"+handle+"/work", ZeroOID, oid("H")),
		}
	}
	for _, tc := range []struct {
		name       string
		principal  Principal
		visibility Visibility
		// ok is the commands the grant admits, by their place in commands.
		ok []int
	}{
		{"a full_access seat of the owning unit", seatOf("dev"), VisibilityCompany, []int{0, 1, 3, 5, 7}},
		{"a read_only seat of the owning unit", seatOf("reader"), VisibilityCompany, nil},
		{"a full_access seat outside a company repository's unit", seatOf("outsider"), VisibilityCompany,
			[]int{5}},
		{"a full_access seat outside a unit repository's unit", seatOf("outsider"), VisibilityUnit, nil},
		{"the owning unit's lead", seatOf("eng-lead"), VisibilityCompany, []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"a person", founderOf, VisibilityUnit, []int{0, 1, 2, 3, 4, 5, 6, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := decideEstate(t)
			e.apply(pushOf(t, decideRepo, pushEntry(uuid.Must(uuid.NewV7()).String(),
				move("refs/heads/old", ZeroOID, oid("O")), move("refs/tags/v0", ZeroOID, oid("V")))))
			e.exec(`UPDATE code_repos SET visibility = ? WHERE id = ?`, string(tc.visibility), decideRepo)
			ref := RepoRef{ID: decideRepo, Name: "web", Owner: Owner{Kind: OwnerUnit, Ref: "ENG"},
				Visibility: tc.visibility}
			g := GrantOf(tc.principal, ref, seatChart{})
			cmds := commands(tc.principal.Seat)
			entries, answers := decideSeats(t, e, DefaultLimits(), pushBy(tc.principal, cmds...))
			for i, c := range cmds {
				got := answers[0].commands[i]
				reason, why := g.RefRule(c)
				switch {
				case slices.Contains(tc.ok, i) != (reason == ""):
					t.Errorf("%s: the grant admits it %v, and the case says %v — the case is not the table it "+
						"names", c.Ref, reason == "", slices.Contains(tc.ok, i))
				case reason == "" && got.Result != ResultOK:
					t.Errorf("%s, which the grant admits, is %s %s (%q), want ok", c.Ref, got.Result, got.Reason,
						got.Detail)
				case reason != "" && (got.Result != ResultNG || got.Reason != RefusalNotPermitted || got.Detail != why):
					t.Errorf("%s is %s %s (%q), want not_permitted with the grant's own sentence %q", c.Ref,
						got.Result, got.Reason, got.Detail, why)
				}
			}
			if len(tc.ok) == 0 {
				if len(entries) != 0 || answers[0].inRecord {
					t.Fatalf("a push the grant refuses whole is in the record (%d entries)", len(entries))
				}
				return
			}
			if len(entries) != 1 {
				t.Fatalf("%d entries, want the push's one", len(entries))
			}
			for i, c := range entries[0].Commands {
				if !slices.Contains(tc.ok, i) && (c.Result != ResultNG || c.Reason != RefusalNotPermitted) {
					t.Errorf("the record's %s is %s %s, want the grant's refusal recorded", c.Ref, c.Result, c.Reason)
				}
			}
		})
	}
}

// A SEAT OUTSIDE A REPOSITORY'S OWNERS FILLS AT MOST HALF OF IT (W29): a
// full_access seat of another unit pushes only into its own namespace there,
// and holds the repository to OutsideSeatShare of its live refs and of
// code.limits.repo_max_bytes, judged on the repository's whole counts — so
// at one live ref short of half its two creates in one push are one ok and
// one repo_refs naming the half and whose the rest is, and at a mebibyte
// short of half its 2 MiB pack is repo_size naming the half; while a person's
// create and pack, and those of a seat of the owning unit, are judged at the
// whole bounds and land.
//
// Mutation: judge every principal at the whole bounds — the outside seat's
// second create and its pack land, red.
func TestASeatOutsideTheUnitFillsAtMostHalfARepository(t *testing.T) {
	t.Parallel()
	refs := func(handle string, n int) []Command {
		var out []Command
		for i := range n {
			out = append(out, cmd(fmt.Sprintf("%swork-%d", SeatNamespace(handle), i), ZeroOID, oid(fmt.Sprint(handle, i))))
		}
		return out
	}
	t.Run("live refs", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			who     string
			p       Principal
			creates int
			ok      int
		}{
			{"a seat outside the owning unit", seatOf("outsider"), 2, 1},
			{"a person", founderOf, 2, 2},
			{"a seat of the owning unit", seatOf("dev"), 2, 2},
		} {
			e := decideEstate(t)
			half := MaxRefsPerRepo / OutsideSeatShare
			e.exec(`UPDATE code_repos SET refs = ? WHERE id = ?`, half-1, decideRepo)
			_, answers := decideSeats(t, e, DefaultLimits(), pushBy(tc.p, refs(tc.p.Seat, tc.creates)...))
			ok := 0
			for _, c := range answers[0].commands {
				switch {
				case c.Result == ResultOK:
					ok++
				case c.Reason != RefusalRepoRefs || !strings.Contains(c.Detail, fmt.Sprintf("fewer than %d", half)) ||
					!strings.Contains(c.Detail, "outside unit ENG") || !strings.Contains(c.Detail, "its owners"):
					t.Errorf("%s: %s is %s %s (%q), want ok or repo_refs naming the half and the owners",
						tc.who, c.Ref, c.Result, c.Reason, c.Detail)
				}
			}
			if ok != tc.ok {
				t.Errorf("%s: %d of %d creates landed at %d live refs, want %d", tc.who, ok, tc.creates, half-1,
					tc.ok)
			}
		}
	})
	t.Run("packed bytes", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		half := limits.RepoMaxBytes / OutsideSeatShare
		for _, tc := range []struct {
			who  string
			p    Principal
			land bool
		}{
			{"a seat outside the owning unit", seatOf("outsider"), false},
			{"a person", founderOf, true},
			{"a seat of the owning unit", seatOf("dev"), true},
		} {
			e := decideEstate(t)
			e.exec(`UPDATE code_repos SET live_bytes = ? WHERE id = ?`, half-1<<20, decideRepo)
			_, answers := decideSeats(t, e, limits, withPack(pushBy(tc.p, refs(tc.p.Seat, 1)...), 2<<20, decideAt))
			got := answers[0].commands[0]
			switch {
			case tc.land && got.Result != ResultOK:
				t.Errorf("%s: its 2 MiB pack is %s %s (%q), want it to land under the whole bound", tc.who,
					got.Result, got.Reason, got.Detail)
			case !tc.land && (got.Reason != RefusalRepoSize || !strings.Contains(got.Detail, gib(half)) ||
				!strings.Contains(got.Detail, "half of code.limits.repo_max_bytes")):
				t.Errorf("%s: its 2 MiB pack is %s %s (%q), want repo_size naming the half (%s)", tc.who,
					got.Result, got.Reason, got.Detail, gib(half))
			}
		}
	})
}

// DIRECT_PUSH PERSONS TAKES A PERSON'S PUSH ALONE (W3): on main, which a
// persons rule protects, a person's update lands and a seat's — one whose
// grant writes the branch — is refused protected_direct, saying the rule takes
// a person's push only; under direct_push none, both are refused, the rule
// naming none.
//
// Mutation: drop the persons arm of ProtectionRefusal — the seat's update of
// main lands, red.
func TestPersonsRefusesASeatsDirectPush(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		who    string
		p      Principal
		c      Command
		reason Refusal
		detail string
	}{
		{"a person on main", founderOf, cmd("refs/heads/main", oid("A"), oid("B")), "", ""},
		{"a seat on main", seatOf("dev"), cmd("refs/heads/main", oid("A"), oid("B")), RefusalProtectedDirect,
			"takes a person's push only (protection refs/heads/main: direct_push persons)"},
		{"a person on a frozen ref", founderOf, cmd("refs/heads/locked", ZeroOID, oid("L")), RefusalProtectedDirect,
			"takes no direct push (protection refs/heads/locked: direct_push none)"},
		{"a seat on a frozen ref", seatOf("dev"), cmd("refs/heads/locked", ZeroOID, oid("L")),
			RefusalProtectedDirect, "takes no direct push (protection refs/heads/locked: direct_push none)"},
	} {
		e := decideEstate(t)
		_, answers := decideSeats(t, e, DefaultLimits(), pushBy(tc.p, tc.c))
		got := answers[0].commands[0]
		switch {
		case tc.reason == "" && got.Result != ResultOK:
			t.Errorf("%s: %s %s (%q), want ok", tc.who, got.Result, got.Reason, got.Detail)
		case tc.reason != "" && (got.Reason != tc.reason || got.Detail != tc.detail):
			t.Errorf("%s: %s %s (%q), want %s (%q)", tc.who, got.Result, got.Reason, got.Detail, tc.reason, tc.detail)
		}
	}
}

// A SEAT PAST ITS DAY'S QUOTA IS REFUSED, UNRECORDED (W13): a seat that has
// pushed one short of code.limits.pushes_per_seat_day today lands the first
// of three pushes in one batch, and the other two are refused push_quota WHOLE
// — every command under the one sentence naming how many it has pushed, the
// bound and the instant its count starts again — and are no entry of the
// record, which holds one push of the seat's, stamped with the day it was
// counted on; another seat's push in the same batch, and a person's, land.
//
// Mutation: record the quota's refusal — the record holds the seat's three
// pushes, red.
func TestASeatPastItsQuotaIsRefusedUnrecorded(t *testing.T) {
	t.Parallel()
	e := decideEstate(t)
	limits := DefaultLimits()
	limits.PushesPerSeatDay = 3
	e.exec(`INSERT INTO code_seat_days (seat, day, pushes) VALUES ('dev', '2031-04-02', 2)`)
	first := pushBy(seatOf("dev"), cmd("refs/heads/seats/dev/one", ZeroOID, oid("1")))
	other := pushBy(seatOf("outsider"), cmd("refs/heads/seats/outsider/four", ZeroOID, oid("4")))
	person := pushBy(founderOf, cmd("refs/heads/five", ZeroOID, oid("5")))
	entries, answers := decideSeats(t, e, limits, first,
		pushBy(seatOf("dev"), cmd("refs/heads/seats/dev/two", ZeroOID, oid("2"))),
		pushBy(seatOf("dev"), cmd("refs/heads/seats/dev/three", ZeroOID, oid("3")),
			cmd("refs/heads/seats/dev/three-b", ZeroOID, oid("3b"))),
		other, person)
	if got := answers[0]; !got.inRecord || got.commands[0].Result != ResultOK {
		t.Fatalf("the seat's push one short of its quota is %s %s (whole: %q), want it in the record",
			got.commands[0].Result, got.commands[0].Reason, got.refused)
	}
	for i, pushed := range map[int]int{1: 3, 2: 3} {
		got := answers[i]
		want := quotaDetail("dev", pushed, 3, decideDay(t, "2031-04-02"))
		if got.refused != RefusalPushQuota || got.detail != want || got.inRecord {
			t.Fatalf("push %d: answered %q (%q), in the record: %v; want push_quota (%q), out of it", i,
				got.refused, got.detail, got.inRecord, want)
		}
		for _, c := range got.commands {
			if c.Result != ResultNG || c.Reason != RefusalPushQuota || c.Detail != want {
				t.Errorf("push %d: %s is %s %s (%q), want ng push_quota under the push's sentence", i, c.Ref,
					c.Result, c.Reason, c.Detail)
			}
		}
	}
	if !strings.Contains(answers[1].detail, "2031-04-03T00:00:00Z") {
		t.Errorf("the refusal %q does not say when the count starts again", answers[1].detail)
	}
	if !answers[3].inRecord || !answers[4].inRecord {
		t.Fatalf("another seat's push and a person's are out of the record (%v, %v)", answers[3].inRecord,
			answers[4].inRecord)
	}
	if len(entries) != 3 || entries[0].PushID != first.PushID || entries[1].PushID != other.PushID ||
		entries[2].PushID != person.PushID {
		t.Fatalf("the record holds %d entries, want the seat's first push, the other seat's and the person's",
			len(entries))
	}
	for _, entry := range entries {
		want := "2031-04-02"
		if entry.Principal.Kind != PrincipalAgent {
			want = ""
		}
		if entry.Day != want {
			t.Errorf("the entry of %s's push is stamped day %q, want %q", entry.Principal.Seat, entry.Day, want)
		}
	}
}

// decideDay is the company day label names, on UTC's calendar.
func decideDay(t *testing.T, label string) period.Window {
	t.Helper()
	w, err := period.Parse(period.Day, label, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// THE QUOTA'S DAY IS THE COMPANY'S DAY, on its own clock: a seat pushes at
// 23:30 in Santiago on the night the zone skips its midnight, and again an
// hour later, at 01:30 of the next day — the same day in UTC — so with one
// push a day the second push lands, counted on its own company day, and a
// third on that day is refused, the count starting again at that day's end in
// the company's zone. Each decide's record is applied, so what the next
// decide counts is the push apply's own counter.
//
// Mutation: count on UTC's calendar — the second push meets the first's count,
// red.
func TestTheQuotaDayIsTheCompanysDay(t *testing.T) {
	t.Parallel()
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	e := decideEstate(t)
	limits := DefaultLimits()
	limits.PushesPerSeatDay = 1
	seats := SeatRules{Roster: seatChart{}, Zone: santiago}
	push := func(at time.Time, name string) (PushEntry, decided) {
		t.Helper()
		entries, answers, err := decideErr(e, decideRepo, limits, DefaultDeletedRetention, seats, at, &verdicts{},
			pushBy(seatOf("dev"), cmd(SeatNamespace("dev")+name, ZeroOID, oid(name))))
		if err != nil {
			t.Fatalf("decide %s: %v", name, err)
		}
		if len(entries) == 1 {
			e.apply(pushOf(t, decideRepo, entries...))
			return entries[0], answers[0]
		}
		return PushEntry{}, answers[0]
	}
	late := time.Date(2024, 9, 8, 3, 30, 0, 0, time.UTC) // 23:30 on the 7th in Santiago
	after := late.Add(time.Hour)                         // 01:30 on the 8th: its midnight never happened
	if late.In(santiago).Day() != 7 || after.In(santiago).Day() != 8 || late.Day() != after.Day() {
		t.Fatalf("the premise: %s and %s are two days in Santiago and one in UTC", late.In(santiago),
			after.In(santiago))
	}
	if entry, got := push(late, "late"); !got.inRecord || entry.Day != "2024-09-07" {
		t.Fatalf("the push at 23:30 in Santiago: in the record %v, counted on %q (%q); want it, on 2024-09-07",
			got.inRecord, entry.Day, got.detail)
	}
	if entry, got := push(after, "after"); !got.inRecord || entry.Day != "2024-09-08" {
		t.Fatalf("the push an hour later: in the record %v, counted on %q (%q); want it, on 2024-09-08 — its "+
			"own company day", got.inRecord, entry.Day, got.detail)
	}
	_, got := push(after.Add(time.Minute), "third")
	if got.refused != RefusalPushQuota || !strings.Contains(got.detail, "since 2024-09-08 began") ||
		!strings.Contains(got.detail, "2024-09-09T00:00:00-03:00") {
		t.Fatalf("a third push on the 8th is %q (%q), want push_quota counted since the 8th began and ending at "+
			"its midnight in Santiago", got.refused, got.detail)
	}
	if n := e.num(`SELECT COUNT(*) FROM code_seat_days WHERE seat = 'dev'`); n != 1 {
		t.Errorf("the counter holds %d of dev's days, want the 8th's alone: a newer day forgets the older", n)
	}
}

// A SEAT'S PUSH IS NEVER DECIDED WITH NO CHART: one reaching the decide with
// none is the caller's bug, refused as one, rather than recorded refused by a
// grant nobody judged — while a batch of persons' pushes, whose decide reads
// no chart, is decided with none.
func TestASeatsPushIsNeverDecidedWithNoChart(t *testing.T) {
	t.Parallel()
	e := decideEstate(t)
	_, _, err := decideErr(e, decideRepo, DefaultLimits(), DefaultDeletedRetention, SeatRules{}, decideAt,
		&verdicts{}, pushBy(founderOf, cmd("refs/heads/new", ZeroOID, oid("N"))),
		pushBy(seatOf("dev"), cmd(SeatNamespace("dev")+"x", ZeroOID, oid("X"))))
	if err == nil || !strings.Contains(err.Error(), "no chart") {
		t.Fatalf("a seat's push decided with no chart: %v, want the decide's refusal of its caller's bug", err)
	}
	if _, answers := decideIn(t, e, DefaultLimits(), pushBy(founderOf, cmd("refs/heads/new", ZeroOID,
		oid("N")))); answers[0].commands[0].Result != ResultOK {
		t.Fatalf("a person's push with no chart is %s %s, want ok", answers[0].commands[0].Result,
			answers[0].commands[0].Reason)
	}
}

// THE SEAT COUNTER IS A FUNCTION OF THE LOG (W24): three appliers given one
// seat's pushes on two days — the second day's interleaved with a record
// decided late on the first, a person's push beside one, and a record
// redelivering a push already applied beside a new one — hold identical
// code_seat_days rows after every record: the late record re-creates its day's
// row and the seat's next entry of the newer day forgets it, the person's push
// is counted nowhere, and the redelivered push id is counted once. And the
// rows a count touches are counted in the record's: a seat's first push of a
// new day reports two rows more than a person's push of the same shape, the
// day's row and the older one it forgot.
//
// Mutation: count every entry the record carries rather than the ones it
// inserts — the redelivery is counted twice, red.
func TestTheSeatCounterIsAFunctionOfTheLog(t *testing.T) {
	t.Parallel()
	seatPush := func(id, day string) PushEntry {
		e := pushEntry(id, move("refs/heads/seats/dev/"+id, ZeroOID, oid(id)))
		e.Principal, e.Via, e.Day = seatOf("dev"), ViaEngine, day
		return e
	}
	records := []built{
		create(t, "repo-a", "a"),
		pushOf(t, "repo-a", seatPush("p1", "2031-04-01")),
		pushOf(t, "repo-a", seatPush("p2", "2031-04-02")),
		pushOf(t, "repo-a", seatPush("p3", "2031-04-01")),
		pushOf(t, "repo-a", seatPush("p4", "2031-04-02"), pushEntry("person", move("refs/heads/x", ZeroOID, oid("x")))),
		pushOf(t, "repo-a", seatPush("p2", "2031-04-02"), seatPush("p5", "2031-04-02")),
	}
	// want is the seat's rows after each record, "day:pushes" in day order.
	want := [][]string{
		nil,
		{"2031-04-01:1"},
		{"2031-04-02:1"},
		{"2031-04-01:1", "2031-04-02:1"},
		{"2031-04-02:2"},
		{"2031-04-02:3"},
	}
	appliers := []*estate{openApplyEstate(t), openApplyEstate(t), openApplyEstate(t)}
	for i, r := range records {
		for n, e := range appliers {
			e.apply(r)
			if got := seatDays(t, e); !slices.Equal(got, want[i]) {
				t.Fatalf("applier %d after record %d holds %v, want %v", n, i, got, want[i])
			}
		}
	}
	if n := appliers[0].num(`SELECT COUNT(*) FROM code_seat_days WHERE seat != 'dev'`); n != 0 {
		t.Errorf("the counter holds %d rows of a seat that never pushed: the person's push was counted", n)
	}

	person, seat := openApplyEstate(t), openApplyEstate(t)
	for _, e := range []*estate{person, seat} {
		e.apply(create(t, "repo-a", "a"))
	}
	seat.apply(pushOf(t, "repo-a", seatPush("p0", "2031-04-01")))
	person.apply(pushOf(t, "repo-a", pushEntry("q0", move("refs/heads/seats/dev/p0", ZeroOID, oid("p0")))))
	ofPerson := person.applyCounting(pushOf(t, "repo-a", pushEntry("q1",
		move("refs/heads/seats/dev/p1", ZeroOID, oid("p1")))))
	ofSeat := seat.applyCounting(pushOf(t, "repo-a", seatPush("p1", "2031-04-02")))
	if ofSeat != ofPerson+seatDayRows {
		t.Errorf("a seat's first push of a new day reports %d rows and a person's of the same shape %d, want "+
			"%d more: its day's row and the older one it forgot", ofSeat, ofPerson, seatDayRows)
	}
}

// seatDays is dev's rows of the counter on e, "day:pushes" in day order.
func seatDays(t *testing.T, e *estate) []string {
	t.Helper()
	var out []string
	if err := e.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(t.Context(),
			`SELECT day, pushes FROM code_seat_days WHERE seat = 'dev' ORDER BY day`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var day string
			var pushes int
			if err := rows.Scan(&day, &pushes); err != nil {
				return err
			}
			out = append(out, fmt.Sprintf("%s:%d", day, pushes))
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the seat's days: %v", err)
	}
	return out
}
