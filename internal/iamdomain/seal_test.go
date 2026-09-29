package iamdomain_test

import (
	"database/sql"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// who is the person most cases here seal for.
const who = "018f3a9c-0000-7000-8000-000000000001"

// newSealer is a sealer over the rig's own keyring, for a case that needs no
// estate.
func newSealer(t *testing.T) *iamdomain.Sealer {
	t.Helper()
	sealer, err := iamdomain.NewSealer(testCipher(t, testKeyring))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	return sealer
}

// ONE PERSON'S SEALED VALUE DOES NOT OPEN AS ANOTHER'S.
//
// Every person's values are sealed under ONE keyring, so the key cannot tell
// two people apart: the person half of the associated data is the whole of
// what stops a row that acquired somebody else's sealed name — a restore, a
// bad migration, a bug — from rendering it as its own.
//
// Mutation: drop the person id from [iamdomain.AADFor] and this opens.
func TestOnePersonsSealedValueDoesNotOpenAsAnothers(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	const other = "018f3a9c-0000-7000-8000-000000000002"
	sealed, err := sealer.Seal(who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := sealer.Open(other, iamdomain.FieldName, sealed); !errors.Is(err,
		secrets.ErrDecrypt) {
		t.Fatalf("one person's sealed name opened under another's id as (%q, %v)",
			plain, err)
	}
	if plain, err := sealer.Open(who, iamdomain.FieldName, sealed); err != nil ||
		plain != "Sarah Chen" {
		t.Errorf("the value does not open as whose it is: (%q, %v)", plain, err)
	}
}

// ONE PERSON'S OWN VALUES DO NOT OPEN AS EACH OTHER.
//
// The field half of the associated data: a person's name and address are
// sealed under the same keyring for the same person, so an address that
// reached the name column would otherwise decrypt cleanly and every surface
// that renders a person would print it where their name goes.
func TestOneFieldsSealedValueDoesNotOpenAsAnother(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	address, err := sealer.Seal(who, iamdomain.FieldEmail, "sarah.chen@example.com")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := sealer.Open(who, iamdomain.FieldName, address); !errors.Is(err,
		secrets.ErrDecrypt) {
		t.Fatalf("a sealed address opened as a name, giving (%q, %v)", plain, err)
	}
	if plain, err := sealer.Open(who, iamdomain.FieldEmail, address); err != nil ||
		plain != "sarah.chen@example.com" {
		t.Errorf("the value does not open as what it is: (%q, %v)", plain, err)
	}
}

// A SECOND FACTOR'S SEED OPENS ONLY AS THE CREDENTIAL IT WAS ENROLLED AS.
//
// Every seed a person ever enrolled is sealed for that one person, so the
// person half cannot tell their current seed from the one they replaced,
// sitting in an old backup. The credential id in the associated data is what
// does: a seed pasted over another credential's opens as nothing, under the
// same person or another, and so does one moved into the name column.
//
// Mutation: seal under [iamdomain.AADFor] alone and the re-enrolment row opens.
func TestASecondFactorSeedOpensOnlyAsTheCredentialItWasEnrolledAs(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	const other = "018f3a9c-0000-7000-8000-000000000002"
	const seed = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	sealed, err := sealer.SealCredential(who, "cred-a", iamdomain.FieldTOTP, seed)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, seed) {
		t.Fatalf("the sealed seed carries the seed in the clear: %q", sealed)
	}
	if plain, err := sealer.OpenCredential(who, "cred-a", iamdomain.FieldTOTP,
		sealed); err != nil || plain != seed {
		t.Fatalf("the seed does not open as what it is: (%q, %v)", plain, err)
	}
	for name, open := range map[string]func() (string, error){
		"the same person's re-enrolment": func() (string, error) {
			return sealer.OpenCredential(who, "cred-b", iamdomain.FieldTOTP, sealed)
		},
		"another person's credential of the same id": func() (string, error) {
			return sealer.OpenCredential(other, "cred-a", iamdomain.FieldTOTP, sealed)
		},
		"the person's name column": func() (string, error) {
			return sealer.Open(who, iamdomain.FieldName, sealed)
		},
		"a credential with no id": func() (string, error) {
			return sealer.OpenCredential(who, "", iamdomain.FieldTOTP, sealed)
		},
	} {
		if plain, err := open(); !errors.Is(err, secrets.ErrDecrypt) {
			t.Errorf("%s opened the seed as (%q, %v), want secrets.ErrDecrypt",
				name, plain, err)
		}
	}
}

// AN INVITATION'S ADDRESS OPENS ONLY AS THAT INVITATION'S.
//
// An invitation's id is not a person's, but both are strings the one keyring
// seals under: bound in a person's grammar, an invitation's sealed address
// would open as the name or address of anybody whose id happened to be
// spelled like it — and a person's address, pasted onto an invitation row,
// would render as whom it was sent to.
func TestAnInvitationsAddressOpensOnlyAsThatInvitation(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	const invitation = who
	sealed, err := sealer.SealInvitation(invitation, "sam@example.com")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := sealer.OpenInvitation(invitation, sealed); err != nil ||
		plain != "sam@example.com" {
		t.Fatalf("the address does not open as the invitation's: (%q, %v)", plain, err)
	}
	if plain, err := sealer.Open(who, iamdomain.FieldEmail, sealed); !errors.Is(err,
		secrets.ErrDecrypt) {
		t.Errorf("an invitation's address opened as a person's of the same id: "+
			"(%q, %v)", plain, err)
	}
	personal, err := sealer.Seal(who, iamdomain.FieldEmail, "sarah.chen@example.com")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if plain, err := sealer.OpenInvitation(invitation, personal); !errors.Is(err,
		secrets.ErrDecrypt) {
		t.Errorf("a person's address opened as an invitation's: (%q, %v)", plain, err)
	}
}

// A SEALER WITH NO KEYRING, AND A CALL THAT NAMES NOBODY, ARE BOTH REFUSED.
//
// With no keyring a sealer would pass names, addresses and seeds through in
// the clear; with no owner, every unidentified value would share one
// associated data, under which any of them opens as any other.
func TestASealerRefusesWhatWouldSealForNobody(t *testing.T) {
	t.Parallel()
	if _, err := iamdomain.NewSealer(nil); err == nil {
		t.Error("a sealer was built with no keyring, so it would write " +
			"cleartext names into every node's database")
	}
	sealer := newSealer(t)
	if _, err := sealer.Seal("", iamdomain.FieldName, "Sarah Chen"); err == nil {
		t.Error("a value was sealed for nobody")
	}
	if _, err := sealer.SealInvitation("", "sam@example.com"); err == nil {
		t.Error("an address was sealed for no invitation")
	}
	if _, err := sealer.SealCredential(who, "", iamdomain.FieldTOTP, "seed"); err == nil {
		t.Error("a seed was sealed for no credential")
	}
}

// AN EMPTY SEALED VALUE OPENS AS EMPTY.
//
// Most rows have an unset optional field, and every value a removal erased is
// empty — rendering either is not a failure.
func TestAnEmptySealedValueOpensAsEmpty(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	for name, open := range map[string]func() (string, error){
		"a person's": func() (string, error) {
			return sealer.Open(who, iamdomain.FieldName, "")
		},
		"an invitation's": func() (string, error) {
			return sealer.OpenInvitation(who, "")
		},
	} {
		if plain, err := open(); err != nil || plain != "" {
			t.Errorf("opening %s unset value gave (%q, %v)", name, plain, err)
		}
	}
}

// owned is everything one person's values are sealed as: the person, the
// credentials they enrolled and the invitations issued to them.
type owned struct {
	person      string
	credentials []string
	invitations []string
}

// envelopeShape finds a keyring envelope wherever a column holds one — on its
// own, or inside a JSON document.
var envelopeShape = regexp.MustCompile(`enc:v1:[^:"\s]+:[A-Za-z0-9+/]+=*`)

// openable counts, per identity table, the sealed values one node's rows hold
// that open as one of owner's — every table, every column, every document.
//
// EVERY TABLE, found rather than listed: a list of where the sealed values are
// is the thing the erasure itself refused to keep, and a new table carrying
// one would pass a scan that did not know to read it.
func openable(t *testing.T, db *store.DB, sealer *iamdomain.Sealer,
	owner owned) map[string]int {

	t.Helper()
	opens := []func(string) error{
		func(v string) error {
			_, err := sealer.Open(owner.person, iamdomain.FieldName, v)
			return err
		},
		func(v string) error {
			_, err := sealer.Open(owner.person, iamdomain.FieldEmail, v)
			return err
		},
	}
	for _, credential := range owner.credentials {
		opens = append(opens, func(v string) error {
			_, err := sealer.OpenCredential(owner.person, credential,
				iamdomain.FieldTOTP, v)
			return err
		})
	}
	for _, invitation := range owner.invitations {
		opens = append(opens, func(v string) error {
			_, err := sealer.OpenInvitation(invitation, v)
			return err
		})
	}
	out := map[string]int{}
	if err := db.Replicated().Read(t.Context(), func(tx *sql.Tx) error {
		tables, err := namesOf(tx, t, `SELECT name FROM sqlite_master
			WHERE type = 'table' AND name LIKE 'iam\_%' ESCAPE '\'`)
		if err != nil {
			return err
		}
		if len(tables) == 0 {
			t.Fatal("the scan found no identity table to read")
		}
		for _, table := range tables {
			values, err := valuesOf(tx, t, table)
			if err != nil {
				return err
			}
			for _, value := range values {
				for _, sealed := range envelopeShape.FindAllString(value, -1) {
					for _, open := range opens {
						if open(sealed) == nil {
							out[table]++
						}
					}
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("scan the identity tables: %v", err)
	}
	return out
}

// namesOf reads one text column.
func namesOf(tx *sql.Tx, t *testing.T, query string) ([]string, error) {
	rows, err := tx.QueryContext(t.Context(), query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// valuesOf is every text or byte value one table holds, column by column.
func valuesOf(tx *sql.Tx, t *testing.T, table string) ([]string, error) {
	rows, err := tx.QueryContext(t.Context(), `SELECT * FROM `+table)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		cells := make([]any, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		for _, cell := range cells {
			switch v := cell.(type) {
			case []byte:
				out = append(out, string(v))
			case string:
				out = append(out, v)
			}
		}
	}
	return out, rows.Err()
}

// joinWithSeed enrols one person through an invitation, as everybody after the
// first operator is enrolled, then gives them a second factor and a new name —
// so every place a person's sealed values land holds one: their row, their
// credential, the invitation that was addressed to them, and a trail row per
// record.
func joinWithSeed(t *testing.T, rig *writeRig, address, name string) owned {
	t.Helper()
	issued, err := inviteFor(t, rig, address, "")
	if err != nil {
		t.Fatalf("invite %s: %v", address, err)
	}
	if _, err := redeemAs(t, rig, issued, address, issued.Secret, ""); err != nil {
		t.Fatalf("redeem %s's invitation: %v", address, err)
	}
	person, err := iamdomain.InvitedPersonID(issued.ID)
	if err != nil {
		t.Fatalf("derive the invited person: %v", err)
	}
	// AND SPENT, as the sign-in surface spends it once the redemption lands.
	if err := rig.during(func() error {
		_, err := nodeWriter(rig).SpendInvitation(t.Context(), iamdomain.InvitationSpend{
			ID: issued.ID, Blind: blindOf(t, address), Person: person,
			OpID: "op-spend-" + person, Reason: "redeemed",
		})
		return err
	}); err != nil {
		t.Fatalf("spend %s's invitation: %v", address, err)
	}
	rig.drain()
	// A CREDENTIAL ID IS THE ROW'S KEY ACROSS EVERYBODY, as the sign-in
	// surface's uuids are, so each person's is their own.
	credential := "app-" + person
	seed, err := rig.sealer.SealCredential(person, credential, iamdomain.FieldTOTP,
		"JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatalf("seal the seed: %v", err)
	}
	if err := rig.during(func() error {
		_, err := rig.writer.SetCredentials(t.Context(), iamdomain.CredentialSet{
			PersonID: person, OpID: "op-app-" + person,
			Reason: "enrolled a second factor",
			Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
				return append(held, iamdomain.Credential{
					V: iamdomain.DocumentVersion, ID: credential,
					Method: iamdomain.MethodTOTP, Verifier: seed,
				}), nil
			},
		})
		return err
	}); err != nil {
		t.Fatalf("enrol a second factor: %v", err)
	}
	if err := rig.during(func() error {
		_, err := rig.writer.UpdatePerson(t.Context(), iamdomain.PersonUpdate{
			PersonID: person, Name: &name, OpID: "op-name-" + person,
			Reason: "gave their name",
			Apply:  func(p iamdomain.Person) (iamdomain.Person, error) { return p, nil },
		})
		return err
	}); err != nil {
		t.Fatalf("name them: %v", err)
	}
	rig.drain()
	return owned{person: person, credentials: []string{credential},
		invitations: []string{issued.ID}}
}

// A REMOVAL LEAVES NO VALUE OF THEIRS THAT OPENS, ON ANY NODE.
//
// What a removal promises is that the company no longer holds anything that is
// somebody's. Their rows go, and three kinds of row outlive them on purpose —
// the invitation that was addressed to them, the authentication trail naming
// what was done to them, and the tombstone — each still holding, before the
// erasure, a sealed copy of their name, their address or their seed that the
// keyring every node holds would open. So the scan asks the only question
// that matters, of every identity table on the node: does ANY value here open
// as theirs?
//
// The CONTROL is the same scan before the removal, which finds their values
// in their row, their credential, their invitation and their trail — so a
// clean scan after says the values were erased, not that the scan read
// nothing. The colleague beside them keeps every value, because the erasure is
// about one person. And a second node that applies the same log from its first
// record writes byte-identical rows, because every node erases.
//
// Mutation: drop the call to eraseSealed from the removal's apply and the
// invitation and the trail still open.
func TestARemovalLeavesNoValueOfTheirsThatOpens(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	sarah := joinWithSeed(t, rig, "sarah.chen@example.com", "Sarah Chen")
	pat := joinWithSeed(t, rig, "pat.lee@example.com", "Pat Lee")

	before := openable(t, rig.db, rig.sealer, sarah)
	for _, table := range []string{"iam_people", "iam_credentials", "iam_invites",
		"iam_history"} {
		if before[table] == 0 {
			t.Fatalf("before the removal %s holds nothing of Sarah's that opens "+
				"(%v), so this scan cannot see what the removal must erase",
				table, before)
		}
	}
	colleague := openable(t, rig.db, rig.sealer, pat)

	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), sarah.person, "op-remove",
			"left the company")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()

	if after := openable(t, rig.db, rig.sealer, sarah); len(after) > 0 {
		t.Errorf("after the removal these tables still hold values of Sarah's "+
			"that open under the keyring every node holds: %v", after)
	}
	if got := openable(t, rig.db, rig.sealer, pat); !equalCounts(got, colleague) {
		t.Errorf("removing Sarah changed what opens as Pat's: %v, was %v",
			got, colleague)
	}
	// THE TRAIL SURVIVES, which is why its rows were erased rather than
	// deleted: who did what to whom is the audit a removal must never take.
	if got := rig.column(`SELECT op FROM iam_history WHERE person_id = ?`,
		sarah.person); len(got) == 0 {
		t.Error("the removal deleted the trail")
	}

	second := newFollower(t, "node-b", brokerAt)
	second.follow(rig)
	for _, query := range []string{
		`SELECT id || '|' || hex(document) FROM iam_history ORDER BY id`,
		`SELECT id || '|' || hex(email_sealed) || '|' || hex(document)
		   FROM iam_invites ORDER BY id`,
	} {
		mine, theirs := columnOf(t, rig.db, query), columnOf(t, second.db, query)
		if !slices.Equal(mine, theirs) {
			t.Errorf("two nodes applying one log disagree after the erasure "+
				"(%s):\n%v\n%v", query, mine, theirs)
		}
	}
}

// AND A REMOVAL REACHES THE ADDRESS THEY WERE INVITED AT, ONCE IT IS NOT THEIRS.
//
// An invitation and the trail row its record wrote are about an ADDRESS, and
// name nobody — so the erasure finds them by address. It used to look only at
// the address the removal released, so somebody whose address changed after
// they redeemed their invitation left the trail row of that invitation holding
// the old address, sealed and opening under the keyring every node holds. The
// addresses a removal erases are every one the person was invited at as well
// ([iamdomain] erasedBlinds), and its record declares all of their buckets.
//
// Mutation: erase only the released address and the invitation's trail row
// still opens.
func TestARemovalErasesTheAddressTheyWereInvitedAt(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	sarah := joinWithSeed(t, rig, "sarah.chen@example.com", "Sarah Chen")
	// HER ADDRESS MOVES: a claim of the new one takes the column off the old.
	if err := rig.claim(iamdomain.KindEmail, blindOf(t, "s.okoro@example.com"),
		sarah.person, "op-new-address"); err != nil {
		t.Fatalf("claim the new address: %v", err)
	}
	rig.drain()
	if before := openable(t, rig.db, rig.sealer, sarah); before["iam_history"] == 0 {
		t.Fatalf("before the removal the trail holds nothing of Sarah's that "+
			"opens (%v), so this case cannot see the invitation's row", before)
	}

	if err := rig.during(func() error {
		_, err := rig.writer.Remove(t.Context(), sarah.person, "op-remove",
			"left the company")
		return err
	}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	rig.drain()
	if after := openable(t, rig.db, rig.sealer, sarah); len(after) > 0 {
		t.Errorf("after removing somebody whose address changed, these tables "+
			"still hold values of theirs that open: %v", after)
	}
}

// equalCounts reports whether two per-table counts are the same.
func equalCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// NOTHING PERSONAL IS ON THE LOG IN THE CLEAR.
//
// The log is the write-ahead log every node's rows derive from, and a record
// is on the broker, on the cluster port, in every node's deferred table and in
// every backup of the streams until the trim passes it — none of which a
// removal can reach. So what a record carries must already be ciphertext when
// it is published: the name, the address and the seed are sealed at the
// WRITER, never at the applier.
//
// The CONTROL is the login, which is in the clear by design: the scan finds it
// in the same records, so a clean scan says the values were sealed rather than
// that it read the wrong bytes.
//
// Mutation: publish the enrolment's name or address unsealed and the scan
// finds it.
func TestNothingPersonalIsOnTheLogInTheClear(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	joinWithSeed(t, rig, "sarah.chen@example.com", "Sarah Chen")

	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	var sawLogin bool
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, ok, err := rig.log.At(t.Context(), seq)
		if err != nil {
			t.Fatalf("read record %d: %v", seq, err)
		}
		if !ok {
			continue
		}
		record := string(payload)
		for _, clear := range []string{"Sarah Chen", "A joiner", "example.com",
			"JBSWY3DPEHPK3PXP"} {
			if strings.Contains(record, clear) {
				t.Errorf("record %d carries %q in the clear", seq, clear)
			}
		}
		if strings.Contains(record, `"sarah.chen"`) {
			sawLogin = true
		}
	}
	if !sawLogin {
		t.Fatal("no record carries the login, which is in the clear by design — " +
			"the scan is not reading what the log holds")
	}
}

// A KEYRING ROTATION MOVES EVERYBODY'S VALUES, AND THE OLD KEY CAN THEN GO.
//
// A rotation adds a key and makes it active, re-seals what the old one
// sealed, and drops it. Until the re-seal, a node holding both keys still
// opens every value — which is what makes the rotation zero-downtime. The
// re-seal is RECORDS, because these rows are derived from the log and a node
// that rewrote its own would disagree with its peers; so it is one record per
// person, every node applies it, and after it a node holding ONLY the new key
// opens every person's name, address and seed. A second pass finds nothing to
// move and publishes nothing.
//
// The CONTROL is the new key alone before the re-seal, which opens nothing —
// so what opens afterwards is what the re-seal moved.
//
// Mutation: skip the seed in the re-seal and the new key alone cannot open it.
func TestAKeyringRotationMovesEverybodysValues(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	sarah := joinWithSeed(t, rig, "sarah.chen@example.com", "Sarah Chen")
	// AN INVITATION NOBODY HAS REDEEMED, which a re-seal does not move: it
	// is counted, so the operator knows to wait out its window.
	if _, err := inviteFor(t, rig, "sam@example.com", ""); err != nil {
		t.Fatalf("invite: %v", err)
	}
	reader := rig.reader(t)

	counted, err := reader.SealedKeys(t.Context(), brokerAt)
	if err != nil {
		t.Fatalf("SealedKeys: %v", err)
	}
	if lapsed, err := reader.SealedKeys(t.Context(),
		brokerAt.Add(169*time.Hour)); err != nil || len(lapsed.Invitations) != 0 {
		t.Errorf("a week and an hour on, the estate counts %+v (%v) — a lapsed "+
			"invitation's address is never opened again, so a rotation need "+
			"not wait for it", lapsed, err)
	}
	if counted.People["k1"] != 3 || counted.Invitations["k1"] != 1 {
		t.Fatalf("before the rotation the estate counts %+v, want Sarah's "+
			"name, address and seed and one outstanding invitation under k1",
			counted)
	}

	k2 := []byte(strings.Repeat("2", 32))
	both := secrets.Keyring{ActiveID: "k2", Keys: map[string][]byte{
		"k1": testKeyring.Keys["k1"], "k2": k2}}
	onlyNew := secrets.Keyring{ActiveID: "k2", Keys: map[string][]byte{"k2": k2}}

	rotating, err := iamdomain.NewSealer(testCipher(t, both))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	after, err := iamdomain.NewSealer(testCipher(t, onlyNew))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	if got := valuesOpenedBy(t, rig, rotating, sarah); len(got) != 3 {
		t.Fatalf("a node holding both keys opens %v of Sarah's values, want all "+
			"three — the rotation would sign her out mid-way", got)
	}
	if got := valuesOpenedBy(t, rig, after, sarah); len(got) != 0 {
		t.Fatalf("the new key alone opened %v before anything was re-sealed", got)
	}

	deps := rig.nodeDeps
	deps.Sealer = rotating
	node, err := iamdomain.NewWriter(deps)
	if err != nil {
		t.Fatalf("build the rotating node's writer: %v", err)
	}
	var report iamdomain.ResealReport
	if err := rig.during(func() error {
		var err error
		report, err = node.Reseal(t.Context(), reader)
		return err
	}); err != nil {
		t.Fatalf("Reseal: %v", err)
	}
	rig.drain()
	if !slices.Equal(report.People, []string{sarah.person}) || report.Values != 3 ||
		len(report.Unknown) != 0 {
		t.Fatalf("the re-seal reported %+v, want Sarah's three values moved",
			report)
	}
	got := valuesOpenedBy(t, rig, after, sarah)
	want := map[string]string{
		"name": "Sarah Chen", "email": "sarah.chen@example.com",
		"totp": "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP",
	}
	if len(got) != len(want) {
		t.Fatalf("after the re-seal the new key alone opens %v, want %v", got, want)
	}
	for field, value := range want {
		if got[field] != value {
			t.Errorf("after the re-seal %s opens as %q, want %q", field,
				got[field], value)
		}
	}
	counted, err = reader.SealedKeys(t.Context(), brokerAt)
	if err != nil {
		t.Fatalf("SealedKeys: %v", err)
	}
	if counted.InvitationsOutside("k2") != 1 {
		t.Errorf("after the re-seal the estate counts %+v, want Sam's live "+
			"invitation still under k1 — it is waited out, not moved", counted)
	}
	if counted.Outside("k2") != 0 || counted.People["k2"] != 3 {
		t.Errorf("after the re-seal the estate counts %+v, want every person's "+
			"value under k2", counted)
	}

	end, err := rig.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if err := rig.during(func() error {
		var err error
		report, err = node.Reseal(t.Context(), reader)
		return err
	}); err != nil {
		t.Fatalf("a second Reseal: %v", err)
	}
	if len(report.People) != 0 || report.Values != 0 {
		t.Errorf("a second pass over a moved estate moved %+v", report)
	}
	if again, err := rig.log.End(t.Context()); err != nil || again != end {
		t.Errorf("a second pass published: the log went from %d to %d (%v)",
			end, again, err)
	}
}

// valuesOpenedBy is each of one person's values their row and credential hold
// that a sealer opens, by field.
func valuesOpenedBy(t *testing.T, rig *writeRig, sealer *iamdomain.Sealer,
	owner owned) map[string]string {

	t.Helper()
	out := map[string]string{}
	for field, query := range map[iamdomain.Field]string{
		iamdomain.FieldName:  `SELECT name_sealed FROM iam_people WHERE id = ?`,
		iamdomain.FieldEmail: `SELECT email_sealed FROM iam_people WHERE id = ?`,
	} {
		for _, sealed := range rig.column(query, owner.person) {
			if plain, err := sealer.Open(owner.person, field, sealed); err == nil {
				out[string(field)] = plain
			}
		}
	}
	for _, sealed := range rig.column(`SELECT verifier FROM iam_credentials
		WHERE person_id = ? AND id = ?`, owner.person, owner.credentials[0]) {
		if plain, err := sealer.OpenCredential(owner.person, owner.credentials[0],
			iamdomain.FieldTOTP, sealed); err == nil {
			out[string(iamdomain.FieldTOTP)] = plain
		}
	}
	return out
}

// THE ACTIVE KEY A SEALER MOVES VALUES ONTO IS THE ONE ITS KEYRING SEALS
// UNDER, read off what it seals rather than stated beside it.
func TestASealerSealsUnderItsKeyringsActiveKey(t *testing.T) {
	t.Parallel()
	ring := secrets.Keyring{ActiveID: "k2", Keys: map[string][]byte{
		"k1": []byte(strings.Repeat("1", 32)), "k2": []byte(strings.Repeat("2", 32))}}
	sealer, err := iamdomain.NewSealer(testCipher(t, ring))
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}
	sealed, err := sealer.Seal(who, iamdomain.FieldName, "Sarah Chen")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if key, ok := secrets.EnvelopeKeyID(sealed); !ok || key != "k2" {
		t.Errorf("a value sealed under a ring whose active key is k2 names %q", key)
	}
}
