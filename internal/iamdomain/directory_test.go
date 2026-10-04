package iamdomain_test

import (
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY UNIQUE COLUMN IS WRITTEN FROM THE DIRECTORY'S APPLY AND NOWHERE ELSE.
//
// A login, an address and a seat are decided on ONE subject, in one snapshot
// of every row, and that is the whole of what keeps two people off one of
// them: there is no unique index in this estate and there cannot be one. A
// statement anywhere else that set one of the three columns would be a write
// no directory decision ever saw — the duplicate nothing refuses.
//
// A WALK OVER THE SOURCE, because the property is about which statements
// exist rather than about what any one record does: a behavioural case could
// only show the paths somebody thought to exercise.
//
// Mutation: add `login = ?` to the person record's UPDATE in apply_person.go
// and this fails naming that file.
func TestEveryUniqueColumnIsWrittenOnlyFromTheDirectory(t *testing.T) {
	t.Parallel()
	const home = "apply_directory.go"
	write := regexp.MustCompile(`(?is)^\s*(INSERT\s+INTO|UPDATE)\s+iam_people\b(.*)$`)
	unique := regexp.MustCompile(`\b(login|email_blind|seat_id)\b`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			m := write.FindStringSubmatch(text)
			if m == nil {
				return true
			}
			// WHAT THE STATEMENT WRITES: an INSERT's column list, an
			// UPDATE's SET clause — never its WHERE, which may read one.
			written := m[2]
			if strings.EqualFold(strings.Fields(m[1])[0], "UPDATE") {
				if i := strings.Index(strings.ToUpper(written), "WHERE"); i >= 0 {
					written = written[:i]
				}
			} else if open, shut := strings.Index(written, "("),
				strings.Index(written, ")"); open >= 0 && shut > open {
				written = written[open:shut]
			}
			columns := unique.FindAllString(written, -1)
			if len(columns) == 0 {
				return true
			}
			if name != home {
				t.Errorf("%s writes %v on iam_people — only the directory's "+
					"apply (%s) may, because only its decide read every row "+
					"the value could collide with", name, columns, home)
				return true
			}
			found++
			return true
		})
	}
	// THE WALK READ SOMETHING: the enrolment's INSERT and the identity
	// change's UPDATE both write the columns, so fewer than two is a walk
	// that looked in the wrong place and would pass anything.
	if found < 2 {
		t.Fatalf("the walk found %d statements in %s writing a unique column, "+
			"want at least the enrolment's and the identity change's", found, home)
	}
}

// A DIRECTORY DECISION IS REFUSED WHILE THIS NODE RETAINS A RECORD — any
// record, in any bucket.
//
// A login, an address or a seat is decided against every row that could hold
// it, and a record this node holds but cannot decode may be the very rename or
// enrolment that took the value. The framework's own probe asks only about the
// buckets the new record declares, which is the people it writes rather than
// the people it compares against, so the decide asks the whole index and is
// refused `deferred` — another node can decide it.
//
// THE CONTROL IS THE REMOVAL BESIDE IT: it frees values rather than taking
// one, so the same retained record does not refuse it — which says the
// refusal comes from the directory's own question and not from a node that
// fails every write.
//
// Mutation: drop the wholeDirectory call from SetIdentity's decide and the
// rename lands.
func TestADirectoryWriteRefusesWhileThisNodeRetainsARecord(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)

	// A BUCKET NONE OF THE CASE'S WRITES DECLARES, so the framework's own
	// probe over each record's scope is not what refuses it.
	near := []iamdomain.Bucket{
		iamdomain.BucketOf(rig.machine), iamdomain.BucketOf(rig.sarah),
		iamdomain.BucketOf(rig.dana),
		iamdomain.BucketOf(blindOf(t, "dana@example.com")),
		iamdomain.BucketOf(blindOf(t, "new.hire@example.com")),
	}
	away := iamdomain.Bucket(0)
	for slices.Contains(near, away) {
		away++
	}
	somebody := "018f3a9c-0000-7000-8000-0000000006ff"
	at := statelog.Position{Stream: iamdomain.Domain{}.Stream().Name, Seq: 1 << 40}
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(), `
			INSERT INTO iam_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, ?, ?, ?, ?, x'00', 0)`,
			at.Packed(), iamdomain.PersonSubject(somebody).Wire(),
			string(iamdomain.KindPerson), somebody,
			iamdomain.RecordVersion+1); err != nil {
			return err
		}
		for _, path := range iamdomain.BucketScope(away).
			Resolve(iamdomain.SweepSubject(away)).Paths {
			if _, err := tx.ExecContext(t.Context(), `
				INSERT INTO iam_log_deferred_scope (position, path) VALUES (?, ?)`,
				at.Packed(), path); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("retain a record in bucket %d: %v", away, err)
	}

	deferred := func(what string, err error) {
		t.Helper()
		var refused *statelog.Unavailable
		if !errors.As(err, &refused) || refused.Reason != statelog.ReasonDeferred {
			t.Errorf("%s while this node retains a record answered %v, want "+
				"`deferred` — the record it cannot read may hold the value", what, err)
		}
	}
	rename := "sarah.c"
	_, err := rig.writer.SetIdentity(t.Context(), iamdomain.IdentityEdit{
		PersonID: rig.sarah, Login: &rename, OpID: "op-rename", Reason: "a rename",
	})
	deferred("a rename", err)
	deferred("an enrolment", rig.enrol(iamdomain.Enrolment{
		PersonID: "018f3a9c-0000-7000-8000-0000000006b1", Kind: iam.KindPerson,
		Stage: iam.StageActive, Name: "New Hire", Email: "new.hire@example.com",
		Login: "new.hire", OpID: "op-new-hire", Reason: "a hire",
	}))
	_, err = inviteFor(t, rig.writeRig, "new.hire@example.com", "")
	deferred("an invitation", err)
	if got, _ := rig.held(rig.sarah); got != "sarah.chen" {
		t.Errorf("the refused rename moved Sarah to %q", got)
	}

	// THE CONTROL: a removal takes no value.
	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), rig.dana, "op-remove-dana", "left")
		return err
	}); err != nil {
		t.Errorf("a removal while this node retains a record in another "+
			"bucket was refused: %v — it frees values, so nothing it decides "+
			"can collide with what that record holds", err)
	}
}

// A REDEMPTION IS ONE RECORD: the person, their address, their login and the
// spent link, together.
//
// It was a sequence — an address claim, a login claim, the person, and a spend
// the sign-in surface published afterwards — so a redemption that stopped
// part way left a claimed address with nobody behind it, or a person whose
// link still opened. One record lands all of it or none of it.
//
// Mutation: drop the invitation's spend from the enrolment's apply and the
// link is still open once the person exists.
func TestARedemptionIsOneRecord(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	const address = "jo.joiner@example.com"
	issued, err := inviteFor(t, rig, address, "")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	rig.drain()
	before, err := rig.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if _, err := redeemAs(t, rig, issued, address, issued.Secret, ""); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	after, err := rig.end(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != before+1 {
		t.Fatalf("the redemption published %d records, want one", after-before)
	}
	_, payload, _, ok, err := rig.log.At(t.Context(), after)
	if err != nil || !ok {
		t.Fatalf("read the redemption's record: %v (present %v)", err, ok)
	}
	body, verdict := rig.verifier.Open(payload)
	if verdict != statelog.Verified {
		t.Fatalf("the redemption's record did not verify: %s", verdict)
	}
	env, err := iamdomain.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode the redemption's record: %v", err)
	}
	if env.Subject != iamdomain.DirectorySubject() || env.Op != iamdomain.OpEnrol {
		t.Errorf("the redemption is %s on %s, want an enrolment on the "+
			"directory", env.Op, env.Subject.Wire())
	}

	rig.drain()
	spentFor := rig.column(`SELECT person_id FROM iam_invites WHERE id = ?`, issued.ID)
	if len(spentFor) != 1 || spentFor[0] == "" {
		t.Fatalf("after the redemption's one record the link is spent for %v, "+
			"want its person", spentFor)
	}
	logins := rig.column(`SELECT login FROM iam_people WHERE id = ?`, spentFor[0])
	blinds := rig.column(`SELECT email_blind FROM iam_people WHERE id = ?`, spentFor[0])
	if !slices.Equal(logins, []string{iam.LoginFromAddress(address)}) ||
		!slices.Equal(blinds, []string{blindOf(t, address)}) {
		t.Errorf("the person the link was spent for holds login %v and address "+
			"%v, want what the redemption asked for", logins, blinds)
	}
}

// A CONTENT RECORD NEVER CREATES A PERSON.
//
// The directory creates a row — its decide is the one that read every login,
// address and seat — and a record on the person's own subject only changes a
// row that exists. A content record about somebody this node holds no row for
// writes nothing: no person, and no credential rows hanging off nobody.
//
// Mutation: turn writePerson's UPDATE back into an upsert and the record
// creates the person, credentials and all.
func TestAContentRecordNeverCreatesAPerson(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	nobody := "018f3a9c-0000-7000-8000-0000000006c1"
	document := mustJSON(t, iamdomain.Person{
		V: iamdomain.DocumentVersion, Kind: iam.KindPerson, Stage: iam.StageActive,
		NameSealed: "sealed:name", EmailSealed: "sealed:email",
		Credentials: []iamdomain.Credential{{V: iamdomain.DocumentVersion,
			ID: "cred-" + nobody, Method: iamdomain.MethodToken, Verifier: "h1"}},
	})
	payload, err := iamdomain.Encode(iamdomain.MutationRecord{
		RecordEnvelope: iamdomain.RecordEnvelope{
			V: iamdomain.BaseRecordVersion, OpID: "op-content-" + nobody,
			Subject: iamdomain.PersonSubject(nobody), Op: iamdomain.OpUpdate,
			Writer: "node-a", Gen: 1, CreatedAt: brokerAt,
			Scope: iamdomain.PeopleScope(nobody),
		},
		Person: nobody, Actor: "ana.admin", ActorKind: iam.KindPerson,
		Mutation: document,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	record := statelog.Record{
		Envelope: statelog.Envelope{
			V: iamdomain.BaseRecordVersion, Kind: string(iamdomain.KindPerson),
			Subject: statelog.Subject{Kind: string(iamdomain.KindPerson), ID: nobody},
			Op:      string(iamdomain.OpUpdate), OpID: "op-content-" + nobody,
		},
		Position: statelog.Position{Stream: iamdomain.Domain{}.Stream().Name, Seq: 1 << 30},
		Payload:  payload, StoredAt: brokerAt,
	}
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := rig.applier.Apply(t.Context(), tx, record,
			statelog.ApplyOptions{Now: brokerAt, StoredAt: brokerAt})
		return err
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := rig.column(`SELECT id FROM iam_people WHERE id = ?`, nobody); len(got) != 0 {
		t.Errorf("a content record created person %v — only the directory's "+
			"decide read the values a new row could collide with", got)
	}
	if got := rig.column(`SELECT id FROM iam_credentials WHERE person_id = ?`,
		nobody); len(got) != 0 {
		t.Errorf("a content record about nobody left credentials %v", got)
	}
}

// TWO ROWS HOLDING ONE LOGIN ARE THE UNKNOWN ARM, never an arbitrary pick.
//
// Ordinary traffic never puts one login on two rows. A node that retained the
// record that moved somebody off a login, and then applied the later one that
// gave it to somebody else, holds both until the first is reprocessed — and
// so does a restore that copied a duplicate back. Answering either row would
// sign somebody in as somebody else; the answer is "ask another node".
//
// Mutation: read the login with `LIMIT 1` and the first row is answered as
// its holder.
func TestTwoRowsHoldingOneLoginAreTheUnknownArm(t *testing.T) {
	t.Parallel()
	rig := newIdentityRig(t)
	if err := rig.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE iam_people SET login = 'sarah.chen' WHERE id = ?`, rig.dana)
		return err
	}); err != nil {
		t.Fatalf("put Sarah's login on Dana's row: %v", err)
	}
	reader := rig.reader(t)
	got, err := reader.PersonByLogin(t.Context(), "sarah.chen")
	if !errors.Is(err, statelog.ErrUnavailable) {
		t.Errorf("a login on two rows answered %+v, %v — want the unknown arm, "+
			"which every caller serves as `ask another node`", got.ID, err)
	}
	// THE CONTROL: a login on one row is answered.
	if got, err := reader.PersonByLogin(t.Context(), "token:ops"); err != nil ||
		got.ID != rig.machine {
		t.Errorf("a login on one row answered %q, %v, want %s", got.ID, err,
			rig.machine)
	}
}
