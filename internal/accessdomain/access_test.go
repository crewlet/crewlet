package accessdomain_test

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY ACCESS RECORD IS SIGNED: what each writer publishes opens under the
// domain's verifier, and a record whose bytes are not the signer's does not.
// A told record, a certificate, a command and a seat's removal are each read
// back off the log as the broker holds them.
//
// Mutation: publish through a publisher with no signer and every record on the
// log fails to open.
func TestEveryAccessRecordIsSigned(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	turn := uuid.NewString()
	writes := []func() (statelog.Result, error){
		func() (statelog.Result, error) {
			return node.writer.Tell(t.Context(), engineCall(), told(t, "sre", turn, "operate", true))
		},
		func() (statelog.Result, error) {
			return node.writer.Issued(t.Context(), engineCall(), certificate("sre", turn, 7))
		},
		func() (statelog.Result, error) {
			return node.writer.Ran(t.Context(), engineCall(), command("sre", turn, "systemctl restart api"))
		},
		func() (statelog.Result, error) { return node.writer.Forget(t.Context(), engineCall(), "gone") },
	}
	for i, write := range writes {
		if res, err := write(); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("write %d: %+v, %v", i, res, err)
		}
	}
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if end < uint64(len(writes)) {
		t.Fatalf("the log holds %d records, want at least %d", end, len(writes))
	}
	verifier, err := recordseal.NewVerifier(accessdomain.Domain{}.Name(), recordKey)
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, ok, err := log.At(t.Context(), seq)
		if err != nil || !ok {
			t.Fatalf("read record %d: %v", seq, err)
		}
		body, verdict := verifier.Open(payload)
		if verdict != recordseal.Verified {
			t.Fatalf("record %d opens %s, want it signed", seq, verdict)
		}
		if _, err := accessdomain.Decode(body); err != nil {
			t.Fatalf("record %d decodes as %v", seq, err)
		}
		// A RECORD WITH ONE BYTE CHANGED IS NOT THE SIGNER'S.
		forged := bytes.Clone(payload)
		forged[len(forged)/2] ^= 0x01
		if _, verdict := verifier.Open(forged); verdict == recordseal.Verified {
			t.Errorf("record %d with a byte changed still opens as signed", seq)
		}
		// AND ANOTHER DOMAIN'S SIGNATURE IS NOT THIS ONE'S.
		other, err := recordseal.NewVerifier("executor", recordKey)
		if err != nil {
			t.Fatal(err)
		}
		if _, verdict := other.Open(payload); verdict == recordseal.Verified {
			t.Errorf("record %d opens as another domain's", seq)
		}
	}
}

// AN ACCESS RECORD APPLIES ONCE ON EVERY NODE: two data nodes applying one log
// hold byte-identical rows; a second write for the same turn, serial or command
// is recorded already and changes nothing; and a redelivery of the whole log
// writes nothing it has not written. The newest told record is each seat's
// baseline on both.
//
// Mutation: drop ON CONFLICT DO NOTHING from applyTold and the redelivery
// fails; move the baseline by `at` rather than by log order and the two nodes'
// rows still agree while the baseline names the older turn.
func TestAnAccessRecordAppliesOnceOnEveryNode(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	a, b := newWriteNode(t, log, "data-1"), newWriteNode(t, log, "data-2")
	first, second := uuid.NewString(), uuid.NewString()
	if _, err := a.writer.Tell(t.Context(), engineCall(), told(t, "sre", first, "operate", true)); err != nil {
		t.Fatal(err)
	}
	b.drain()
	if _, err := b.writer.Tell(t.Context(), engineCall(), told(t, "sre", second, "secure", false)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.writer.Issued(t.Context(), engineCall(), certificate("sre", second, 41)); err != nil {
		t.Fatal(err)
	}
	ran := command("sre", second, "uptime")
	ran.Level, ran.Approver, ran.Decision = accessdomain.LevelRead, "", accessdomain.DecisionNone
	if _, err := b.writer.Ran(t.Context(), engineCall(), ran); err != nil {
		t.Fatal(err)
	}
	a.drain()
	b.drain()

	// THE SAME TURN, SERIAL AND COMMAND AGAIN, under fresh operations and from
	// either node: each was recorded, and nothing moves.
	before := a.dump()
	again := []error{}
	_, err := a.writer.Tell(t.Context(), engineCall(), told(t, "sre", second, "admin", false))
	again = append(again, err)
	_, err = b.writer.Issued(t.Context(), engineCall(), certificate("sre", second, 41))
	again = append(again, err)
	_, err = a.writer.Ran(t.Context(), engineCall(), ran)
	again = append(again, err)
	for i, err := range again {
		if !errors.Is(err, accessdomain.ErrRecorded) {
			t.Errorf("write %d again answers %v, want ErrRecorded", i, err)
		}
	}
	a.drain()
	b.drain()
	if after := a.dump(); !equalDumps(before, after) {
		t.Errorf("a second write of recorded objects moved the rows:\nbefore %v\nafter  %v", before, after)
	}

	// EVERY NODE HOLDS THE SAME BYTES, and a redelivery writes nothing.
	if da, db := a.dump(), b.dump(); !equalDumps(da, db) {
		t.Fatalf("the two nodes' rows differ:\n%v\n%v", da, db)
	}
	a.redeliver()
	if da, db := a.dump(), b.dump(); !equalDumps(da, db) {
		t.Fatalf("a redelivered log moved a node's rows:\n%v\n%v", da, db)
	}
	for _, n := range []*writeNode{a, b} {
		recent, err := n.reader().Recent(t.Context(), "sre", time.Time{}, statelog.Freshness{Level: statelog.ReadStale})
		if err != nil {
			t.Fatal(err)
		}
		if recent.Baseline == nil || recent.Baseline.Turn != second || len(recent.Since) != 2 {
			t.Fatalf("the baseline is %+v with %d recent, want turn %s with 2", recent.Baseline,
				len(recent.Since), second)
		}
		if n.count(`SELECT COUNT(*) FROM access_told WHERE latest = 1`) != 1 {
			t.Error("a seat holds more than one baseline")
		}
	}
}

// equalDumps reports whether two nodes' tables hold the same rows.
func equalDumps(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for table, rows := range a {
		if !slices.Equal(rows, b[table]) {
			return false
		}
	}
	return true
}

// A COMMAND IS SEALED AT REST: its text is in no record on the log and no row
// in the estate, in the clear; the row opens to it only under the record it was
// sealed for; and only its size and digest stand for its stdin and output.
//
// Mutation: write CommandSealed as the plain text and the log and the row both
// carry the command.
func TestACommandIsSealedAtRest(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	const secretCommand = "sudo systemctl restart billing-worker-7731"
	ran := command("sre", uuid.NewString(), secretCommand)
	ran.StdinBytes, ran.StdinSHA256 = 12, strings.Repeat("ab", 32)
	if _, err := node.writer.Ran(t.Context(), engineCall(), ran); err != nil {
		t.Fatal(err)
	}
	end, err := log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= end; seq++ {
		_, payload, _, _, err := log.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(payload, []byte("billing-worker-7731")) {
			t.Fatalf("record %d on the log carries the command in the clear", seq)
		}
	}
	for table, rows := range node.dump() {
		for _, row := range rows {
			if strings.Contains(row, "billing-worker-7731") {
				t.Fatalf("a row of %s holds the command in the clear: %s", table, row)
			}
		}
	}
	page, err := node.reader().Commands(t.Context(), accessdomain.AuditQuery{Handle: "sre"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("the audit answers %+v, %v", page, err)
	}
	row := page.Rows[0]
	if row.ContentKey != "k1" || row.StdinBytes != 12 || row.Host != "api-1" || !row.Ran {
		t.Errorf("the command's row is %+v", row)
	}
	opened, err := row.Open(node.sealer)
	if err != nil || opened != secretCommand {
		t.Fatalf("the row opens to %q, %v; want the command", opened, err)
	}
	// MOVED TO ANOTHER RECORD it does not open at all.
	moved := row
	moved.ID = uuid.NewString()
	if _, err := moved.Open(node.sealer); !errors.Is(err, accessdomain.ErrUnreadable) {
		t.Errorf("a command moved to another record opens: %v", err)
	}
	if _, err := row.Open(nil); err == nil {
		t.Error("a command opened with no keyring")
	}
	counts, err := node.reader().SealedKeys(t.Context())
	if err != nil || counts["k1"] != 1 {
		t.Errorf("the sealed census is %v, %v; want one command under k1", counts, err)
	}
}

// ACCESS RECORDS ARE KEPT FOR THEIR RETENTION: an expiry through a cutoff takes
// every certificate, command, removal and told record broker-stamped before it,
// keeps what is newer, and never takes a seat's baseline; the hours past the
// cutoff are what the duty nominates; and an hour with nothing left past it is
// not due.
//
// Mutation: drop `latest = 0` from the told expiry and the seat's baseline goes
// with the retention; compare `at <= ?` and the record stamped at the cutoff
// itself goes.
func TestAccessRecordsAreKeptForTheirRetention(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	old, baseline, fresh := uuid.NewString(), uuid.NewString(), uuid.NewString()
	writes := []func() error{
		func() error {
			_, err := node.writer.Tell(t.Context(), engineCall(), told(t, "sre", old, "operate", true))
			return err
		},
		func() error {
			_, err := node.writer.Tell(t.Context(), engineCall(), told(t, "sre", baseline, "secure", false))
			return err
		},
		func() error {
			_, err := node.writer.Issued(t.Context(), engineCall(), certificate("sre", old, 3))
			return err
		},
		func() error {
			_, err := node.writer.Ran(t.Context(), engineCall(), command("sre", old, "uptime"))
			return err
		},
		func() error {
			_, err := node.writer.Forget(t.Context(), engineCall(), "gone")
			return err
		},
	}
	for i, write := range writes {
		if err := write(); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// THE CUTOFF IS THE INSTANT OF A RECORD WRITTEN AFTER THEM, which stays.
	if _, err := node.writer.Issued(t.Context(), engineCall(), certificate("sre", fresh, 4)); err != nil {
		t.Fatal(err)
	}
	cutoff := certificateAt(t, node, 4)
	hours, err := node.reader().ExpirableHours(t.Context(), cutoff, 10)
	if err != nil || len(hours) == 0 {
		t.Fatalf("the duty nominates %v, %v", hours, err)
	}
	for _, hour := range hours {
		if !hour.Equal(hour.Truncate(accessdomain.ExpiryBucket)) || !hour.Before(cutoff) {
			t.Errorf("the duty nominates %s, which is not an hour before %s", hour, cutoff)
		}
		if _, err := node.writer.Expire(t.Context(), engineCall(), hour, cutoff); err != nil {
			t.Fatalf("expire %s: %v", hour, err)
		}
	}
	checks := map[string]int{
		`SELECT COUNT(*) FROM access_told WHERE turn_id = '` + old + `'`:             0,
		`SELECT COUNT(*) FROM access_told WHERE turn_id = '` + baseline + `'`:        1,
		`SELECT COUNT(*) FROM access_certificates WHERE serial = '0000000000000003'`: 0,
		`SELECT COUNT(*) FROM access_certificates WHERE serial = '0000000000000004'`: 1,
		`SELECT COUNT(*) FROM access_commands`:                                       0,
		`SELECT COUNT(*) FROM access_seat_removals`:                                  0,
	}
	for statement, want := range checks {
		if got := node.count(statement); got != want {
			t.Errorf("%s = %d after the retention, want %d", statement, got, want)
		}
	}
	if left, err := node.reader().ExpirableHours(t.Context(), cutoff, 10); err != nil || len(left) != 0 {
		t.Errorf("after the retention the duty still nominates %v, %v", left, err)
	}
	if _, err := node.writer.Expire(t.Context(), engineCall(), hours[0], cutoff); !errors.Is(err, accessdomain.ErrNotDue) {
		t.Errorf("an expiry of an hour with nothing left answers %v, want ErrNotDue", err)
	}
}

// certificateAt is the broker instant of the certificate with serial.
func certificateAt(t *testing.T, n *writeNode, serial uint64) time.Time {
	t.Helper()
	page, err := n.reader().Certificates(t.Context(), accessdomain.AuditQuery{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Rows {
		if row.Serial == accessdomain.SerialString(serial) {
			return row.At
		}
	}
	t.Fatalf("no certificate %d", serial)
	return time.Time{}
}
