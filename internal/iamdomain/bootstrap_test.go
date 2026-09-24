package iamdomain_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FIRST PERSON'S CODE, AND EVERY PARTY THAT ASKS WHETHER THE COMPANY HAS
// STARTED.
//
// A founding is a sequence — the take, the releases, the address, the login,
// then the person — and every one of these cases is about a rule the sequence
// used to break at one of its joints: a dead code refused only after the
// founder's address was claimed; two founders landing because person subjects
// never contend; an abandoned attempt's reservation holding the founder's own
// address against the fresh code the documented remedy handed them; a
// re-issue that promised exactly one live code and left two.

// bootstrapRig is the write rig with the gestures a founding is made of.
type bootstrapRig struct {
	*writeRig
}

func newBootstrapRig(t *testing.T) bootstrapRig {
	return bootstrapRig{newWriteRig(t)}
}

// mint publishes one boot's code expiring at expires and applies it.
func (r bootstrapRig) mint(id string, expires time.Time) error {
	r.t.Helper()
	err := r.draining(func() error {
		_, err := r.writer.MintBootstrap(r.t.Context(), iamdomain.BootstrapMint{
			ID: id, Verifier: id, MintedBy: "node-a", ExpiresAt: expires,
			OpID: "op-mint-" + id, Reason: "a fresh estate",
		})
		return err
	})
	r.drain()
	return err
}

// nodeAt is the node's own writer — the party every founding and re-issue is
// published by — with its clock at now.
func (r bootstrapRig) nodeAt(now time.Time) *iamdomain.Writer {
	w := nodeWriter(r.writeRig)
	w.Now = func() time.Time { return now }
	return w
}

// reissue re-issues the code as `crewlet iam bootstrap-code` does, from a
// node whose clock reads now.
func (r bootstrapRig) reissue(id string, now time.Time) (statelog.Result, error) {
	r.t.Helper()
	var result statelog.Result
	err := r.draining(func() error {
		var err error
		result, err = r.nodeAt(now).ReissueBootstrap(r.t.Context(),
			iamdomain.BootstrapMint{
				ID: id, Verifier: id, MintedBy: "node-a",
				ExpiresAt: now.Add(24 * time.Hour),
				OpID:      "op-reissue-" + id, Reason: "re-issued",
			})
		return err
	})
	r.drain()
	return result, err
}

// founding is what one founder types.
type founding struct {
	code, email, login string
}

// the founder every case is about unless it says otherwise.
func jane(code string) founding {
	return founding{code: code, email: "jane@example.com", login: "jane.founder"}
}

// enrolment is the enrolment the route forms for a founding: the person is the
// one the code creates, never one the caller chose.
func (r bootstrapRig) enrolment(f founding) iamdomain.Enrolment {
	r.t.Helper()
	held, err := r.reader(r.t).BootstrapCode(r.t.Context(), f.code)
	if err != nil {
		r.t.Fatalf("read code %s: %v", f.code, err)
	}
	return iamdomain.Enrolment{
		PersonID: held.FounderID(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Name: "The founder", Email: f.email,
		Login: f.login, Grants: iam.AllGrants,
		Colleague: iam.ColleagueWrite, BootstrapCode: f.code,
		OpID: "bootstrap:" + f.code, Reason: "the first operator",
	}
}

// foundAt enrols the founder a code names, the way the route does, from a
// node whose clock reads now, and applies what it wrote.
func (r bootstrapRig) foundAt(now time.Time, f founding) (statelog.Result, error) {
	r.t.Helper()
	in := r.enrolment(f)
	var result statelog.Result
	err := r.draining(func() error {
		var err error
		result, err = r.nodeAt(now).Enrol(r.t.Context(), in)
		return err
	})
	r.drain()
	return result, err
}

// found is [bootstrapRig.foundAt] at the rig's own instant.
func (r bootstrapRig) found(code string) error {
	r.t.Helper()
	_, err := r.foundAt(brokerAt, jane(code))
	return err
}

// started is the one-bit answer every surface reads.
func (r bootstrapRig) started() bool {
	r.t.Helper()
	held, err := r.reader(r.t).AnyPerson(r.t.Context())
	if err != nil {
		r.t.Fatalf("AnyPerson: %v", err)
	}
	return held
}

// stateOf is what the log says a code is at now.
func (r bootstrapRig) stateOf(code string, now time.Time) iamdomain.CodeState {
	r.t.Helper()
	held, err := r.reader(r.t).BootstrapCode(r.t.Context(), code)
	if err != nil {
		r.t.Fatalf("read code %s: %v", code, err)
	}
	return held.State(now)
}

// enrolled is every person or machine this estate holds as enrolled.
func (r bootstrapRig) enrolled() []string {
	r.t.Helper()
	return r.column(`SELECT id FROM iam_people WHERE kind <> '' ORDER BY id`)
}

// A DEAD CODE IS REFUSED BEFORE ANYTHING IS CLAIMED, AND THE FRESH ONE LETS
// THE SAME FOUNDER IN.
//
// Mutation: take a code whatever its state (drop the take's refusal of a dead
// one) and the aged-out attempt reaches the claims — the rows check below
// fails and the founder's address is held against the fresh code.
func TestADeadCodeClaimsNothingAndTheReissuedOneLetsTheFounderIn(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)

	if err := rig.mint("aged-out", brokerAt.Add(-time.Minute)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if err := rig.mint("withdrawn", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	// A RE-ISSUE WITHDRAWS every live code and mints its own.
	if _, err := rig.reissue("reissued", brokerAt); err != nil {
		t.Fatalf("re-issue: %v", err)
	}

	for _, code := range []string{"aged-out", "withdrawn", "never-minted"} {
		err := rig.found(code)
		if !errors.Is(err, iamdomain.ErrRefused) ||
			!errors.Is(err, iamdomain.ErrBootstrapCodeDead) {
			t.Errorf("a %s code answered %v, want a refusal naming a dead "+
				"code — never the closed company", code, err)
		}
		if errors.Is(err, iamdomain.ErrBootstrapClosed) {
			t.Errorf("a %s code was answered as a company that has started "+
				"(%v), which sends its founder away from a company still "+
				"waiting for them", code, err)
		}
	}
	// NOTHING WAS CLAIMED: no reservation holds the founder's address or
	// login, and the company has not started.
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("a refused bootstrap left rows behind: %v — the founder's "+
			"own address is now held against their next code", rows)
	}
	if rig.started() {
		t.Error("a refused bootstrap started the company")
	}
	if err := rig.found("reissued"); err != nil {
		t.Fatalf("the founder was refused on a fresh code after a dead one: %v",
			err)
	}
	if !rig.started() {
		t.Error("the founder's enrolment did not start the company")
	}
}

// A RESERVATION IS NOBODY TO EVERY PARTY THAT ASKS, AND A PERSON IS SOMEBODY
// TO ALL OF THEM.
//
// The readers counted every row and the record counted only enrolled ones, so
// the route read "closed" from a reservation the record would have admitted a
// founder past. One predicate answers both now, and the mint asks it too.
//
// Mutation: count a reservation in the shared predicate (drop its kind filter)
// and the reservation below closes the company — AnyPerson answers true and
// the mint and the founder are both refused. Remove the mint's own decide and
// a code is minted for a company that has started.
func TestEveryPartyAgreesWhetherTheCompanyHasStarted(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)

	// SOMEBODY ELSE'S HALF-FINISHED ENROLMENT: a claimed address and no
	// person behind it.
	if err := rig.claim(iamdomain.KindEmail, blindOf(t, "someone@example.com"),
		uuid.Must(uuid.NewV7()).String(), "op-reserve"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rig.drain()
	if rig.started() {
		t.Error("a reservation reads as somebody enrolled")
	}
	if err := rig.mint("first", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("a reservation refused the mint of the company's first "+
			"code: %v", err)
	}
	if err := rig.found("first"); err != nil {
		t.Fatalf("a reservation closed the first-person exemption: %v", err)
	}

	// NOW SOMEBODY IS: every party says so, and says it with the closed
	// answer rather than the dead-code one.
	if !rig.started() {
		t.Error("the founder does not read as somebody enrolled")
	}
	err := rig.mint("second", brokerAt.Add(time.Hour))
	if !errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("a code was minted for a company that has started (%v)", err)
	}
	if got := rig.column(`SELECT id FROM iam_bootstrap_codes WHERE id = 'second'`); len(got) != 0 {
		t.Errorf("the refused mint landed a row: %v", got)
	}
	if _, err := rig.reissue("third", brokerAt); !errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("a re-issue on a company that has started answered %v", err)
	}
}

// NOBODY IS AN ABSENCE, AND A NODE HOLDING A RECORD IT COULD NOT APPLY CANNOT
// PROVE ONE.
//
// A node that retained the first person's enrolment — a newer build's record,
// one signed under a key it was not restarted with — reads an empty directory
// that is not empty. Answered as nobody, it opened the founder route and let a
// second founder take the whole ceiling. It is the unknown arm now, on the
// reader and in every founding decide, until the node applies what it holds.
//
// The retained record is planted the way the framework's runner writes one:
// the parent row and its scope path, in one transaction.
//
// Mutation: drop the deferral check from the shared predicate and AnyPerson
// answers "nobody" beside the retained record, and the mint lands.
func TestNobodyIsUnknownWhileARecordIsRetained(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	somebody := uuid.Must(uuid.NewV7()).String()
	retain := func(on bool) {
		t.Helper()
		if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			if !on {
				if _, err := tx.ExecContext(t.Context(),
					`DELETE FROM iam_log_deferred_scope`); err != nil {
					return err
				}
				_, err := tx.ExecContext(t.Context(), `DELETE FROM iam_log_deferred`)
				return err
			}
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO iam_log_deferred (position, subject, subject_kind,
					subject_id, version, payload, stored_at)
				VALUES (9999, ?, 'person', ?, 99, x'00', 0)`,
				"person."+somebody, somebody); err != nil {
				return err
			}
			_, err := tx.ExecContext(t.Context(), `
				INSERT INTO iam_log_deferred_scope (position, path) VALUES (9999, ?)`,
				iamdomain.BucketOf(somebody).Path())
			return err
		}); err != nil {
			t.Fatalf("plant the retained record: %v", err)
		}
	}

	retain(true)
	if _, err := rig.reader(t).AnyPerson(t.Context()); !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a node retaining a record answered whether anybody is "+
			"enrolled with %v, want the unknown arm", err)
	}
	if err := rig.mint("while-retained", brokerAt.Add(time.Hour)); !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a node retaining a record minted a founder code (%v)", err)
	}

	// THE CONTROL: the same node, once it holds nothing it could not apply.
	retain(false)
	if held, err := rig.reader(t).AnyPerson(t.Context()); err != nil || held {
		t.Errorf("an empty estate answered %v, %v — want nobody", held, err)
	}
	if err := rig.mint("after", brokerAt.Add(time.Hour)); err != nil {
		t.Errorf("an empty estate refused a mint: %v", err)
	}
}

// A MINT THAT STATES NO EXPIRY IS REFUSED, because a code that never ages is a
// superuser claim in a file for as long as the file survives.
func TestACodeWithNoExpiryIsNeverMinted(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	if err := rig.mint("eternal", time.Time{}); !errors.Is(err, iamdomain.ErrInvalid) {
		t.Errorf("a code with no expiry was minted (%v)", err)
	}
}

// WHAT A CODE IS, ONCE, FOR EVERY READER.
//
// The route, the boot path, the re-issue and the founding's decides each
// decided liveness for themselves, and two of them disagreed about a row with
// no expiry. The cases walk every state and the boundary instant.
func TestACodesStateIsOnePredicate(t *testing.T) {
	t.Parallel()
	now := brokerAt
	minted := now.Add(-time.Hour)
	live := iamdomain.BootstrapCode{ID: "c", MintedAt: minted,
		ExpiresAt: now.Add(time.Hour)}
	with := func(edit func(*iamdomain.BootstrapCode)) iamdomain.BootstrapCode {
		code := live
		edit(&code)
		return code
	}
	cases := []struct {
		name string
		code iamdomain.BootstrapCode
		want iamdomain.CodeState
	}{
		{"absent", iamdomain.BootstrapCode{}, iamdomain.CodeAbsent},
		{"live", live, iamdomain.CodeLive},
		// THE EXPIRY IS EXCLUSIVE: at the instant itself the code is
		// over, the reading every other lifetime here takes.
		{"at its expiry", with(func(c *iamdomain.BootstrapCode) {
			c.ExpiresAt = now
		}), iamdomain.CodeAgedOut},
		{"no expiry", with(func(c *iamdomain.BootstrapCode) {
			c.ExpiresAt = time.Time{}
		}), iamdomain.CodeAgedOut},
		{"withdrawn by a re-issue", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt = now
		}), iamdomain.CodeWithdrawn},
		{"taken by a founding in progress", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt, c.Person = now, "p"
		}), iamdomain.CodeTaken},
		// A TAKE LAPSES WITH ITS CODE: the lifetime bounds finishing as
		// well as starting, or a taken code would never stop working.
		{"taken, and its lifetime over", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt, c.Person = now.Add(-2*time.Hour), "p"
			c.ExpiresAt = now.Add(-time.Minute)
		}), iamdomain.CodeAgedOut},
		{"taken, and its attempt released by a later one", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt, c.Person, c.FounderReleased = now, "p", true
		}), iamdomain.CodeWithdrawn},
		// A LIVE CODE WHOSE FOUNDER WAS RELEASED can create nobody: the
		// person it derives is gone for good.
		{"live, and its founder released", with(func(c *iamdomain.BootstrapCode) {
			c.FounderReleased = true
		}), iamdomain.CodeWithdrawn},
		{"redeemed", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt, c.Person, c.FounderEnrolled = now, "p", true
		}), iamdomain.CodeRedeemed},
		// A FOUNDER WHO EXISTS OUTRANKS A CLOCK: the code that created the
		// company's first person stays redeemed once its hours run out.
		{"redeemed and aged out", with(func(c *iamdomain.BootstrapCode) {
			c.SpentAt, c.Person, c.FounderEnrolled = now.Add(-time.Hour), "p", true
			c.ExpiresAt = now.Add(-time.Minute)
		}), iamdomain.CodeRedeemed},
	}
	seen := map[iamdomain.CodeState]bool{}
	for _, c := range cases {
		got := c.code.State(now)
		if got != c.want {
			t.Errorf("%s: state %q, want %q", c.name, got, c.want)
		}
		if !got.Valid() {
			t.Errorf("%s: state %q is not one this build knows", c.name, got)
		}
		seen[got] = true
	}
	for _, s := range iamdomain.CodeStates {
		if !seen[s] {
			t.Errorf("no case reaches %q", s)
		}
	}
	if iamdomain.CodeState("spent").Valid() {
		t.Error("a state this build does not know reads as valid")
	}
}

// THE READER ANSWERS EVERY CODE, WITH ITS FOUNDER'S STANDING.
//
// A code that no longer works still resolves to its row, which is what lets a
// founder holding one be told why rather than told they typed it wrong — and
// the row comes back with what became of the person it creates, read in the
// same snapshot, because a taken code and a redeemed one differ only there.
func TestTheReaderSaysWhatBecameOfEveryCode(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	for _, c := range []struct {
		id      string
		expires time.Time
	}{
		{"live", brokerAt.Add(time.Hour)},
		{"aged-out", brokerAt.Add(-time.Minute)},
		{"withdrawn", brokerAt.Add(time.Hour)},
	} {
		if err := rig.mint(c.id, c.expires); err != nil {
			t.Fatalf("mint %s: %v", c.id, err)
		}
	}
	if _, err := rig.reissue("reissued", brokerAt); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if err := rig.found("reissued"); err != nil {
		t.Fatalf("found: %v", err)
	}

	reader := rig.reader(t)
	for id, want := range map[string]iamdomain.CodeState{
		// THE RE-ISSUE WITHDREW both codes that were live, and the one
		// that had aged out is left as it was.
		"live": iamdomain.CodeWithdrawn, "withdrawn": iamdomain.CodeWithdrawn,
		"aged-out": iamdomain.CodeAgedOut, "reissued": iamdomain.CodeRedeemed,
		"nobody": iamdomain.CodeAbsent,
	} {
		code, err := reader.BootstrapCode(t.Context(), id)
		if err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got := code.State(brokerAt); got != want {
			t.Errorf("code %s reads %q, want %q", id, got, want)
		}
		if want != iamdomain.CodeAbsent && code.MintedAt.IsZero() {
			t.Errorf("code %s carries no mint instant, so the founder it "+
				"names cannot be derived", id)
		}
	}
	redeemed, _ := reader.BootstrapCode(t.Context(), "reissued")
	if !redeemed.FounderEnrolled || redeemed.Person != redeemed.FounderID() {
		t.Errorf("the redeemed code reads %+v, want its founder enrolled and "+
			"named on the row", redeemed)
	}
}

// A SECOND CODE WAITS WHILE A FOUNDING IS IN PROGRESS, AND CLAIMS NOTHING.
//
// A fleet offers one code per node, and person subjects never contend — so two
// founders redeeming two codes each passed their own person record and the
// company got two founders carrying the ceiling. The exemption is TAKEN on the
// company's one bootstrap subject now: the second founding is refused before
// it has claimed a thing, and the first finishes with the code it began with.
//
// Mutation: drop the take's refusal of another code's current take and the
// second founding goes ahead — it releases the first, whose own code can then
// never finish, and the assertion that the second was refused goes red.
func TestASecondCodeWaitsWhileAFoundingIsInProgress(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := bootstrapRig{newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".person."}
		return broker
	})}
	for _, id := range []string{"node-a-code", "node-b-code"} {
		if err := rig.mint(id, brokerAt.Add(24*time.Hour)); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
	}

	// THE FIRST FOUNDER TAKES node A's code and stops before the person
	// record lands: the take and both claims are on the log.
	broker.silent.Store(true)
	first, err := rig.foundAt(brokerAt, jane("node-a-code"))
	if err != nil || first.Outcome != statelog.OutcomeUnknown {
		t.Fatalf("the stopped founding answered %+v, %v — want unknown", first, err)
	}
	broker.silent.Store(false)
	if got := rig.stateOf("node-a-code", brokerAt); got != iamdomain.CodeTaken {
		t.Fatalf("the first founding's code reads %q, want taken", got)
	}

	// THE SECOND, with node B's code: refused in progress, having claimed
	// nothing — its founder has no row at all.
	second := founding{code: "node-b-code", email: "sam@example.com",
		login: "sam.second"}
	secondPerson := rig.enrolment(second).PersonID
	_, err = rig.foundAt(brokerAt, second)
	if !errors.Is(err, iamdomain.ErrBootstrapInProgress) {
		t.Fatalf("a second founding answered %v while another was in "+
			"progress, want in-progress", err)
	}
	if rows := rig.column(`SELECT id FROM iam_people WHERE id = ?`,
		secondPerson); len(rows) != 0 {
		t.Errorf("the refused founding left a row behind: %v", rows)
	}

	// THE FIRST FINISHES with its own code, and the company has one founder.
	if _, err := rig.foundAt(brokerAt, jane("node-a-code")); err != nil {
		t.Fatalf("the first founder could not finish with their own code: %v", err)
	}
	if got := rig.enrolled(); len(got) != 1 {
		t.Fatalf("the company holds %v enrolled, want exactly the first founder",
			got)
	}
	if _, err := rig.foundAt(brokerAt, second); !errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("a founding after the company started answered %v, want closed", err)
	}
}

// TWO FOUNDERS AT ONCE: EXACTLY ONE LANDS.
//
// Two live codes redeemed at the same instant from two goroutines — the race
// that produced two founders when the only arbitration was each founder's own
// person subject. Whichever takes first is the founder; the other is refused
// on the one subject, in progress or closed, and claims nothing.
func TestExactlyOneFounderLandsFromTwoCodesAtOnce(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	codes := []string{"node-a-code", "node-b-code"}
	foundings := make([]founding, len(codes))
	for i, id := range codes {
		if err := rig.mint(id, brokerAt.Add(24*time.Hour)); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
		foundings[i] = founding{code: id,
			email: fmt.Sprintf("founder%d@example.com", i),
			login: fmt.Sprintf("founder.number%d", i)}
	}
	enrolments := make([]iamdomain.Enrolment, len(foundings))
	for i, f := range foundings {
		enrolments[i] = rig.enrolment(f)
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		landed  int
		refused []error
	)
	for _, in := range enrolments {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := rig.draining(func() error {
				_, err := rig.nodeAt(brokerAt).Enrol(t.Context(), in)
				return err
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				landed++
				return
			}
			refused = append(refused, err)
		}()
	}
	wg.Wait()
	rig.drain()

	if landed != 1 || len(refused) != 1 {
		t.Fatalf("%d foundings landed and %d were refused (%v), want exactly "+
			"one of each", landed, len(refused), refused)
	}
	if err := refused[0]; !errors.Is(err, iamdomain.ErrBootstrapInProgress) &&
		!errors.Is(err, iamdomain.ErrBootstrapClosed) {
		t.Errorf("the loser was refused with %v, want in progress or closed", err)
	}
	if got := rig.enrolled(); len(got) != 1 {
		t.Errorf("the company holds %v enrolled, want exactly one founder", got)
	}
	if got := rig.column(`SELECT id FROM iam_people`); len(got) != 1 {
		t.Errorf("the rows are %v — the loser claimed something before it "+
			"was refused", got)
	}
}

// AN ABANDONED FOUNDING NEVER HOLDS THE FOUNDER'S NAMES.
//
// The reported case, exactly: the founder's attempt stopped after it claimed
// their address and login; their code aged out; the documented remedy handed
// them a fresh code — and the founder is derived from the code, so the fresh
// one derived a DIFFERENT person, whose claims the old reservation refused as
// "that address belongs to somebody" although nobody was enrolled.
//
// The fresh founding releases the old attempt before it claims, whether the
// log still carries the code the old attempt took (found through it) or has
// swept it (found by its founder shape).
//
// Mutations: drop the releases from the take and the fresh founding is refused
// its own address in both cases; drop the founder-shape half and only the
// swept case goes red.
func TestAnAbandonedFoundingNeverHoldsTheFoundersNames(t *testing.T) {
	t.Parallel()
	for _, swept := range []bool{false, true} {
		name := "its code still on the log"
		if swept {
			name = "its code swept off the log"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var broker *silentBroker
			rig := bootstrapRig{newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
				broker = &silentBroker{Appender: inner, on: ".person."}
				return broker
			})}
			if err := rig.mint("friday", brokerAt.Add(24*time.Hour)); err != nil {
				t.Fatalf("mint: %v", err)
			}
			abandoned := rig.enrolment(jane("friday")).PersonID

			// FRIDAY EVENING: the attempt claims both names and stops.
			broker.silent.Store(true)
			if result, err := rig.foundAt(brokerAt, jane("friday")); err != nil ||
				result.Outcome != statelog.OutcomeUnknown {
				t.Fatalf("the stopped founding answered %+v, %v", result, err)
			}
			broker.silent.Store(false)
			held := rig.column(`SELECT login FROM iam_people WHERE id = ? AND kind = ''`,
				abandoned)
			if len(held) != 1 || held[0] != "jane.founder" {
				t.Fatalf("the abandoned attempt holds %v, want its reservation "+
					"holding the founder's login", held)
			}

			// A WEEK AND MORE LATER: the code aged out, and for the swept
			// case the retention sweep has collected its row.
			monday := brokerAt.Add(72 * time.Hour)
			if swept {
				sweeper := rig.sweeper(time.Now().Add(9 * 24 * time.Hour))
				if err := rig.draining(func() error {
					_, err := sweeper.Sweep(t.Context(), defaultHorizons)
					return err
				}); err != nil {
					t.Fatalf("sweep: %v", err)
				}
				rig.drain()
				if got := rig.stateOf("friday", monday); got != iamdomain.CodeAbsent {
					t.Fatalf("the swept code reads %q, want absent", got)
				}
			}

			// THE DOCUMENTED REMEDY: a re-issue, and the same founder with
			// the same address and login.
			if _, err := rig.reissue("monday", monday); err != nil {
				t.Fatalf("re-issue: %v", err)
			}
			if _, err := rig.foundAt(monday, jane("monday")); err != nil {
				t.Fatalf("the founder was refused their own names on the "+
					"re-issued code: %v", err)
			}
			founder := rig.enrolment(jane("monday")).PersonID
			if got := rig.enrolled(); len(got) != 1 || got[0] != founder {
				t.Errorf("enrolled %v, want exactly the re-issued code's founder %s",
					got, founder)
			}
			if got := rig.column(`SELECT login FROM iam_people WHERE id = ?`,
				founder); len(got) != 1 || got[0] != "jane.founder" {
				t.Errorf("the founder holds logins %v, want jane.founder", got)
			}
			// THE ABANDONED ATTEMPT IS GONE FOR GOOD, and its code — where
			// the log still has it — can create nobody.
			if got := rig.column(`SELECT person_id FROM iam_removed WHERE person_id = ?`,
				abandoned); len(got) != 1 {
				t.Errorf("the abandoned attempt was not released: %v", got)
			}
			if !swept {
				if got := rig.stateOf("friday", monday); got != iamdomain.CodeWithdrawn {
					t.Errorf("the abandoned attempt's code reads %q, want withdrawn", got)
				}
			}
		})
	}
}

// ONLY A FOUNDING'S OWN RESERVATION IS RELEASED.
//
// What a founding ends is an earlier FOUNDING — a code's take, or a
// reservation with the founder shape. A reservation somebody else's unfinished
// enrolment left is not one: an administrator's half-created colleague holding
// the founder's address is refused as held, naming its holder, and nothing is
// removed.
//
// Mutation: release every reservation rather than the founder-shaped ones and
// the colleague's reservation is removed and the founder takes the address.
func TestAReservationThatIsNotAFoundersIsNotReleased(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	colleague := uuid.Must(uuid.NewV7()).String()
	if iamdomain.FounderAttempt(colleague) {
		t.Fatal("a minted person id carries the founder shape")
	}
	if err := rig.claim(iamdomain.KindEmail, blindOf(t, "jane@example.com"),
		colleague, "op-reserve"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	rig.drain()
	if err := rig.mint("code", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	var claimed *iamdomain.ErrClaimed
	if err := rig.found("code"); !errors.As(err, &claimed) ||
		claimed.Holder != colleague {
		t.Fatalf("the founding answered %v, want the login refused as held by %s",
			err, colleague)
	}
	if got := rig.column(`SELECT person_id FROM iam_removed`); len(got) != 0 {
		t.Errorf("a founding released somebody else's reservation: %v", got)
	}
}

// heldBroker holds the first append on a subject containing `on` until told to
// let it through — a request that formed its record and had not reached the
// broker yet, which is what a slow founding looks like from anywhere else.
type heldBroker struct {
	statelog.Appender

	on      atomic.Value // string
	once    sync.Once
	held    chan struct{}
	release chan struct{}

	// after, when set, runs once the first append on a subject containing
	// afterOn has landed and before its caller hears it did.
	afterOn string
	after   func()
	fired   sync.Once
}

func newHeldBroker(inner statelog.Appender) *heldBroker {
	b := &heldBroker{Appender: inner, held: make(chan struct{}),
		release: make(chan struct{})}
	b.on.Store("")
	return b
}

func (b *heldBroker) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	if on := b.on.Load().(string); on != "" && strings.Contains(subject, on) {
		hold := false
		b.once.Do(func() { hold = true })
		if hold {
			close(b.held)
			select {
			case <-b.release:
			case <-ctx.Done():
				return 0, false, ctx.Err()
			}
		}
	}
	seq, dup, err := b.Appender.Append(ctx, subject, msgID, expect, body)
	if err == nil && b.after != nil && strings.Contains(subject, b.afterOn) {
		b.fired.Do(b.after)
	}
	return seq, dup, err
}

// A FOUNDING THAT LAPSED MID-REQUEST NEVER LANDS BESIDE THE NEXT ONE.
//
// A take lapses on a clock, and the request that made it may still be
// publishing. The next founding releases it first — a removal on its person's
// own subject — and that release and the lapsed founding's person record
// contend on the one subject where the two could otherwise both land. The
// cases hold the lapsed attempt's person record in flight and let it reach
// the broker on either side of the next founding:
//
//   - AFTER the next founding released it: its expectation is stale, it
//     re-decides, and its take is no longer current — refused, and the next
//     founding is the company's founder.
//   - BEFORE the next founding's release decides: it lands, the company has
//     started, and the release refuses — the next founding stops, and the
//     lapsed attempt is the founder.
//
// Either way exactly one founder, which is the whole of the exemption.
//
// Mutations: drop the releases and the first case enrols two founders; drop
// the release's refusal of an enrolled person and the second case removes
// the founder the company already has.
func TestAFoundingThatLapsedMidRequestNeverLandsBesideTheNext(t *testing.T) {
	t.Parallel()
	for _, lateLands := range []string{"after the release", "before the release"} {
		t.Run(lateLands, func(t *testing.T) {
			t.Parallel()
			var broker *heldBroker
			rig := bootstrapRig{newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
				broker = newHeldBroker(inner)
				return broker
			})}
			// THE LAPSING CODE LIVES AN HOUR; the next node's lives a day.
			if err := rig.mint("lapsing", brokerAt.Add(time.Hour)); err != nil {
				t.Fatalf("mint: %v", err)
			}
			if err := rig.mint("next", brokerAt.Add(24*time.Hour)); err != nil {
				t.Fatalf("mint: %v", err)
			}
			slow := rig.enrolment(jane("lapsing"))
			next := founding{code: "next", email: "sam@example.com",
				login: "sam.second"}
			nextIn := rig.enrolment(next)

			// A CONSUMER FOR THE WHOLE CASE, because the slow founding's
			// steps wait for their own applies from another goroutine.
			stop := make(chan struct{})
			consumed := make(chan struct{})
			go func() {
				defer close(consumed)
				for {
					select {
					case <-stop:
						rig.drainSafely()
						return
					case <-time.After(5 * time.Millisecond):
						rig.drainSafely()
					}
				}
			}()
			defer func() { close(stop); <-consumed }()

			// THE SLOW FOUNDING, decided while its take is current, held
			// at its person record.
			broker.on.Store(".person." + slow.PersonID)
			slowErr := make(chan error, 1)
			go func() {
				_, err := rig.nodeAt(brokerAt).Enrol(t.Context(), slow)
				slowErr <- err
			}()
			select {
			case <-broker.held:
			case err := <-slowErr:
				t.Fatalf("the slow founding finished before its person record "+
					"was held: %v", err)
			}

			if lateLands == "before the release" {
				// THE LATE RECORD REACHES THE BROKER the moment the next
				// founding's take has, and is applied here before its
				// release decides.
				broker.afterOn = ".bootstrap"
				broker.after = func() {
					close(broker.release)
					if err := <-slowErr; err != nil {
						t.Errorf("the slow founding was refused: %v", err)
					}
					slowErr <- nil
				}
			}

			// TWO HOURS LATER, on the next node's code.
			_, nextErr := func() (statelog.Result, error) {
				return rig.nodeAt(brokerAt.Add(2*time.Hour)).Enrol(t.Context(), nextIn)
			}()
			if lateLands == "after the release" {
				if nextErr != nil {
					t.Fatalf("the next founding was refused: %v", nextErr)
				}
				close(broker.release)
				if err := <-slowErr; !errors.Is(err, iamdomain.ErrBootstrapCodeDead) {
					t.Errorf("the lapsed founding's late record answered %v, "+
						"want its code dead — released by the next one", err)
				}
			} else {
				<-slowErr
				if !errors.Is(nextErr, iamdomain.ErrBootstrapClosed) {
					t.Errorf("the next founding answered %v after the lapsed "+
						"one landed, want the company closed", nextErr)
				}
			}
			rig.drainSafely()

			want := nextIn.PersonID
			if lateLands == "before the release" {
				want = slow.PersonID
			}
			if got := rig.enrolled(); len(got) != 1 || got[0] != want {
				t.Errorf("enrolled %v, want exactly %s", got, want)
			}
		})
	}
}

// A RE-ISSUE LEAVES EXACTLY ONE CODE, AND NO FOUNDING IN PROGRESS.
//
// It withdrew what one read of the log called outstanding and then minted, so
// a code minted between the two — a node booting, a second operator
// re-issuing — was live beside the new one, and a founding in progress went on
// holding the exemption the fresh code needed. The mint decides it now, in its
// own snapshot, and a taken code's attempt is released with the rest.
//
// Mutation: mint without the outstanding check and a boot mint landed during
// the re-issue survives it; skip the release of a taken code and the
// re-issued code is refused as in progress.
func TestAReissueLeavesExactlyOneCodeAndNoFoundingInProgress(t *testing.T) {
	t.Parallel()
	var broker *silentBroker
	rig := bootstrapRig{newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		broker = &silentBroker{Appender: inner, on: ".person."}
		return broker
	})}
	for _, id := range []string{"boot-a", "boot-b", "taken"} {
		if err := rig.mint(id, brokerAt.Add(24*time.Hour)); err != nil {
			t.Fatalf("mint %s: %v", id, err)
		}
	}
	// A FOUNDING IN PROGRESS on one of them.
	broker.silent.Store(true)
	if _, err := rig.foundAt(brokerAt, jane("taken")); err != nil {
		t.Fatalf("the stopped founding: %v", err)
	}
	broker.silent.Store(false)

	if _, err := rig.reissue("fresh", brokerAt); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	for id, want := range map[string]iamdomain.CodeState{
		"boot-a": iamdomain.CodeWithdrawn, "boot-b": iamdomain.CodeWithdrawn,
		"taken": iamdomain.CodeWithdrawn, "fresh": iamdomain.CodeLive,
	} {
		if got := rig.stateOf(id, brokerAt); got != want {
			t.Errorf("after the re-issue %s reads %q, want %q", id, got, want)
		}
	}
	// THE ABANDONED FOUNDING CANNOT FINISH, and the fresh code founds.
	if _, err := rig.foundAt(brokerAt, jane("taken")); !errors.Is(err, iamdomain.ErrBootstrapCodeDead) {
		t.Errorf("the founding the re-issue released could still finish (%v)", err)
	}
	if _, err := rig.foundAt(brokerAt, jane("fresh")); err != nil {
		t.Fatalf("the re-issued code was refused: %v", err)
	}
}

// hookBroker runs `before` once, ahead of the first append whose message id
// contains `on`, and then lets that append through as it was formed — how a
// case puts a write between another write's decide and its append.
type hookBroker struct {
	statelog.Appender
	on     string
	before func()
	once   sync.Once
}

func (b *hookBroker) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	if b.before != nil && strings.Contains(msgID, b.on) {
		b.once.Do(b.before)
	}
	return b.Appender.Append(ctx, subject, msgID, expect, body)
}

// A WITHDRAWAL NEVER ERASES A TAKE.
//
// A withdrawal is a spend that names nobody. Published on a code a founding
// took a moment after the re-issue read it as live, it erased the person that
// take named — the one fact the code's row keeps about the attempt, and the
// one the next founding finds it by to end it. It is guarded in its own
// snapshot now: the take lands first, the withdrawal's expectation is stale,
// and its re-decide finds a code that is no longer live and writes nothing;
// the re-issue releases the attempt instead.
//
// Mutation: drop the withdrawal's guard and the code's row names nobody.
func TestAWithdrawalNeverErasesATake(t *testing.T) {
	t.Parallel()
	var (
		silent *silentBroker
		hook   *hookBroker
	)
	rig := bootstrapRig{newWriteRigWith(t, func(inner statelog.Appender) statelog.Appender {
		silent = &silentBroker{Appender: inner, on: ".person."}
		hook = &hookBroker{Appender: silent, on: ":withdraw:"}
		return hook
	})}
	if err := rig.mint("code", brokerAt.Add(24*time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	founder := rig.enrolment(jane("code"))
	// BETWEEN THE RE-ISSUE'S READ AND ITS WITHDRAWAL, a founder takes the
	// code and stops before their person record.
	hook.before = func() {
		silent.silent.Store(true)
		defer silent.silent.Store(false)
		if result, err := rig.nodeAt(brokerAt).Enrol(t.Context(), founder); err != nil ||
			result.Outcome != statelog.OutcomeUnknown {
			t.Errorf("the founding between the two answered %+v, %v", result, err)
		}
	}
	if _, err := rig.reissue("fresh", brokerAt); err != nil {
		t.Fatalf("re-issue: %v", err)
	}
	if got := rig.column(`SELECT person_id FROM iam_bootstrap_codes WHERE id = 'code'`); len(got) != 1 || got[0] != founder.PersonID {
		t.Errorf("the taken code's row names %v, want the founder %s its take "+
			"named", got, founder.PersonID)
	}
	if got := rig.column(`SELECT person_id FROM iam_removed WHERE person_id = ?`,
		founder.PersonID); len(got) != 1 {
		t.Errorf("the re-issue did not release the founding it ended: %v", got)
	}
	if got := rig.stateOf("fresh", brokerAt); got != iamdomain.CodeLive {
		t.Errorf("the re-issued code reads %q, want live", got)
	}
}

// TWO RE-ISSUES AT ONCE LEAVE ONE LIVE CODE.
//
// Each operator is told a path; the one whose mint lands second withdraws the
// first one's code before its own mint may land, so what is live afterwards is
// exactly one code — the last.
func TestTwoReissuesAtOnceLeaveOneLiveCode(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{"operator-one", "operator-two"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = rig.draining(func() error {
				_, err := rig.nodeAt(brokerAt).ReissueBootstrap(t.Context(),
					iamdomain.BootstrapMint{
						ID: id, Verifier: id, MintedBy: "node-a",
						ExpiresAt: brokerAt.Add(24 * time.Hour),
						OpID:      "op-reissue-" + id, Reason: "re-issued",
					})
				return err
			})
		}()
	}
	wg.Wait()
	rig.drain()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("re-issue %d: %v", i, err)
		}
	}
	live := 0
	for _, id := range []string{"operator-one", "operator-two"} {
		if rig.stateOf(id, brokerAt) == iamdomain.CodeLive {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d codes are live after two concurrent re-issues, want one", live)
	}
}

// A RE-ISSUE IS AN ADMINISTRATOR'S GESTURE: it ends codes somebody may be
// holding, so a party without people:manage is refused before it reads a
// thing, while a boot's mint needs no grant at all.
func TestAReissueTakesTheAdministrativeGrant(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	operator := rig.writer.As("svc:ops", iam.KindMachine,
		[]iam.Grant{iam.GrantFleetOperate})
	err := rig.draining(func() error {
		_, err := operator.ReissueBootstrap(t.Context(), iamdomain.BootstrapMint{
			ID: "code", Verifier: "code", ExpiresAt: brokerAt.Add(time.Hour),
			OpID: "op-reissue", Reason: "re-issued",
		})
		return err
	})
	if !errors.Is(err, iamdomain.ErrRefused) {
		t.Errorf("a re-issue by a party without %s answered %v",
			iamdomain.AdminGrant, err)
	}
	if err := rig.mint("boot", brokerAt.Add(time.Hour)); err != nil {
		t.Errorf("a boot's mint was refused: %v", err)
	}
}

// A FOUNDER'S GRANT THIS BUILD CANNOT NAME IS REFUSED BEFORE ANYTHING IS
// WRITTEN.
//
// The founder's grants are bounded by no writer, so the one bound they meet —
// that this build can name every one of them — is a property of the values,
// and it was checked only at the person record: after the take and both
// claims had landed, so the refusal left the code taken and a reservation
// holding the founder's address and login.
//
// Mutation: drop the check from the enrolment's validation and the founding
// lands carrying a spelling nothing can check.
func TestAFounderGrantThisBuildCannotNameClaimsNothing(t *testing.T) {
	t.Parallel()
	rig := newBootstrapRig(t)
	if err := rig.mint("first", brokerAt.Add(time.Hour)); err != nil {
		t.Fatalf("mint: %v", err)
	}
	in := rig.enrolment(jane("first"))
	in.Grants = append(append([]iam.Grant(nil), in.Grants...),
		iam.Grant("people:everything"))
	err := rig.draining(func() error {
		_, err := rig.nodeAt(brokerAt).Enrol(t.Context(), in)
		return err
	})
	rig.drain()
	if !errors.Is(err, iamdomain.ErrRefused) {
		t.Fatalf("a founding carrying an unknown grant answered %v, want a "+
			"refusal", err)
	}
	if rows := rig.column(`SELECT id FROM iam_people`); len(rows) != 0 {
		t.Errorf("the refused founding left rows behind: %v", rows)
	}
	if got := rig.stateOf("first", brokerAt); got != iamdomain.CodeLive {
		t.Errorf("the refused founding left its code %q, want live", got)
	}
}
