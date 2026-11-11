package accessdomain_test

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/secrets"
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
	if _, err := b.writer.Tell(t.Context(), engineCall(), toldAfterRead(t, b, "sre", second, "secure", false)); err != nil {
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
	if err != nil || counts.Values["k1"] != 1 || counts.Outside("k1") != 0 {
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
			_, err := node.writer.Tell(t.Context(), engineCall(), toldAfterRead(t, node, "sre", baseline, "secure", false))
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

// A TOLD RECORD READ BEFORE A NEWER ONE LANDED IS NOT THE BASELINE. Two turns
// of one seat read the same baseline; the one dispatched later, under newer
// access, launches first. The earlier turn's record, landing after it, is kept,
// but the baseline stays the record whose read was newer, so the seat's next
// turn is not told the same change again.
//
// Mutation: move the baseline to every record that lands, as the log orders
// them, and the late record becomes the baseline.
func TestAToldRecordReadBeforeANewerOneIsNotTheBaseline(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	first := uuid.NewString()
	if _, err := node.writer.Tell(t.Context(), engineCall(), told(t, "sre", first, "operate", true)); err != nil {
		t.Fatal(err)
	}
	early, late := uuid.NewString(), uuid.NewString()
	// BOTH READ THE FIRST TURN'S BASELINE.
	earlyRecord := toldAfterRead(t, node, "sre", early, "secure", false)
	lateRecord := toldAfterRead(t, node, "sre", late, "admin", false)
	if _, err := node.writer.Tell(t.Context(), engineCall(), lateRecord); err != nil {
		t.Fatal(err)
	}
	if _, err := node.writer.Tell(t.Context(), engineCall(), earlyRecord); err != nil {
		t.Fatal(err)
	}
	recent, err := node.reader().Recent(t.Context(), "sre", time.Time{}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatal(err)
	}
	if recent.Baseline == nil || recent.Baseline.Turn != late || len(recent.Since) != 3 {
		t.Fatalf("the baseline is %+v with %d records, want the later-dispatched turn's of 3",
			recent.Baseline, len(recent.Since))
	}
	for _, row := range recent.Since {
		if len(row.Snapshot) != 0 {
			t.Errorf("a recent told record carries its snapshot: %s", row.Turn)
		}
	}
	if len(recent.Baseline.Snapshot) == 0 {
		t.Error("the baseline carries no snapshot")
	}
}

// A COMMAND'S FAILURE IS A CLOSED WORD. Why a command did not run or did not
// finish is one of the failures the domain names, so no error's text, which
// could quote the command or its output, reaches a row in the clear.
//
// Mutation: accept any failure text and the quoted command is recorded.
func TestACommandsFailureIsAClosedWord(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	failed := command("sre", uuid.NewString(), "sudo systemctl restart billing-worker-7731")
	failed.Ran, failed.Failure = false, accessdomain.CommandFailure(`exec "sudo systemctl restart billing-worker-7731" failed`)
	if _, err := node.writer.Ran(t.Context(), engineCall(), failed); !errors.Is(err, accessdomain.ErrInvalid) {
		t.Fatalf("a free-text failure answers %v, want ErrInvalid", err)
	}
	failed.Failure = ""
	if _, err := node.writer.Ran(t.Context(), engineCall(), failed); !errors.Is(err, accessdomain.ErrInvalid) {
		t.Fatalf("a command that did not run and says no failure answers %v, want ErrInvalid", err)
	}
	failed.Failure = accessdomain.FailureUnreachable
	if _, err := node.writer.Ran(t.Context(), engineCall(), failed); err != nil {
		t.Fatalf("a command that could not reach its server: %v", err)
	}
	page, err := node.reader().Commands(t.Context(), accessdomain.AuditQuery{Handle: "sre"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Failure != accessdomain.FailureUnreachable ||
		page.Rows[0].Ran {
		t.Fatalf("the audit answers %+v, %v", page.Rows, err)
	}
}

// A REKEY RESEALS EVERY COMMAND UNDER THE ACTIVE KEY: a node whose keyring made
// a new key active moves each command still sealed under the old one by a
// record on the command's own subject, which every node applies, so the rows
// stay identical across nodes and open under the new key; a second pass finds
// nothing to move.
//
// Mutation: rewrite the row in place instead of publishing a record, and the
// node that did not reseal still holds the old key.
func TestACommandIsResealedUnderTheActiveKey(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	old := newWriteNode(t, log, "data-1")
	const text = "sudo systemctl restart api"
	ran := command("sre", uuid.NewString(), text)
	if _, err := old.writer.Ran(t.Context(), engineCall(), ran); err != nil {
		t.Fatal(err)
	}
	rotated := newWriteNodeWith(t, log, "data-2", rotatedCipher(t))
	rotated.drain()
	report, err := rotated.writer.Reseal(t.Context(), engineCall().By, rotated.reader())
	if err != nil || report.Commands != 1 || report.Unknown != 0 {
		t.Fatalf("the reseal moved %+v, %v; want one command", report, err)
	}
	rotated.drain()
	old.drain()
	for _, n := range []*writeNode{old, rotated} {
		if got := n.count(`SELECT COUNT(*) FROM access_commands WHERE content_key = 'k2'`); got != 1 {
			t.Fatalf("a node holds %d commands under the new key, want 1", got)
		}
	}
	if !maps.EqualFunc(old.dump(), rotated.dump(), slices.Equal) {
		t.Fatal("the two nodes' rows differ after the reseal")
	}
	page, err := rotated.reader().Commands(t.Context(), accessdomain.AuditQuery{Handle: "sre"},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("the audit answers %+v, %v", page, err)
	}
	if opened, err := page.Rows[0].Open(rotated.sealer); err != nil || opened != text {
		t.Fatalf("the resealed row opens to %q, %v", opened, err)
	}
	again, err := rotated.writer.Reseal(t.Context(), engineCall().By, rotated.reader())
	if err != nil || again.Commands != 0 {
		t.Fatalf("a second pass moved %+v, %v; want nothing", again, err)
	}
}

// rotatedCipher is the fleet keyring after a rotation: the old key kept, the
// new one active.
func rotatedCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	old := make([]byte, 32)
	for i := range old {
		old[i] = byte(i + 1)
	}
	next := make([]byte, 32)
	for i := range next {
		next[i] = byte(64 + i)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k2", Keys: map[string][]byte{"k1": old, "k2": next}})
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// A TOLD RECORD'S READ POSITION THAT NAMES NOTHING IS JUDGED ON NO STREAM. A
// position at generation and sequence zero names nothing whatever stream it
// says, so a told record carrying one is taken as one carrying none — as a node
// without data's told record is, routed to a data node by the estate router,
// whose wire leaves a position its type calls zero off. Judged by its stream,
// the same record was refused where it ran in process and taken where it was
// routed. A position that names something on another log is refused either way.
//
// Mutation: judge the read position's stream without asking whether it names
// anything, as the check did, and the record naming nothing on the tracker's
// log is refused.
func TestAToldRecordsReadPositionThatNamesNothingIsJudgedOnNoStream(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	named := told(t, "sre", uuid.NewString(), "operate", true)
	named.ReadThrough = statelog.Position{Stream: "CREWLET_TRACKER_LOG"}
	if _, err := node.writer.Tell(t.Context(), engineCall(), named); err != nil {
		t.Errorf("a told record whose read position names nothing on another log was refused: %v — "+
			"routed, the same record arrives with no position and is taken", err)
	}
	foreign := told(t, "sre", uuid.NewString(), "operate", true)
	foreign.ReadThrough = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 4}
	if _, err := node.writer.Tell(t.Context(), engineCall(), foreign); !errors.Is(err, accessdomain.ErrInvalid) {
		t.Errorf("a told record read at a position on another log was answered %v, want it refused", err)
	}
}

// A SERVER'S USE IS COUNTED IN THE STORE, per server and seat: the commands
// that ran in the week and in the month, and the last. A denied command used
// nothing, and a window that starts after every command counts none.
func TestAServersUseIsCountedPerServerAndSeat(t *testing.T) {
	t.Parallel()
	log := openAccessLog(t)
	node := newWriteNode(t, log, "data-1")
	ran := func(seat, host string) accessdomain.NewCommand {
		c := command(seat, uuid.NewString(), "uptime")
		c.Host = host
		return c
	}
	denied := ran("sre", "api-1")
	denied.Ran, denied.ExitCode, denied.Decision = false, 0, accessdomain.DecisionDenied
	for _, c := range []accessdomain.NewCommand{ran("sre", "api-1"), ran("sre", "api-1"), ran("ops", "api-1"),
		ran("sre", "db-1"), denied} {
		if _, err := node.writer.Ran(t.Context(), engineCall(), c); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	fresh := statelog.Freshness{Level: statelog.ReadStale}
	use, err := node.reader().HostUse(t.Context(), now.Add(-7*24*time.Hour), now.Add(-30*24*time.Hour), fresh)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int64{}
	for _, u := range use {
		if u.Last.IsZero() || u.Last.After(now.Add(time.Minute)) {
			t.Errorf("%s on %s was last used at %s", u.Handle, u.Host, u.Last)
		}
		got[u.Host+"/"+u.Handle] = [2]int64{u.Week, u.Month}
	}
	want := map[string][2]int64{"api-1/ops": {1, 1}, "api-1/sre": {2, 2}, "db-1/sre": {1, 1}}
	if !maps.Equal(got, want) {
		t.Errorf("each server's use is %v, want %v", got, want)
	}
	later, err := node.reader().HostUse(t.Context(), now.Add(time.Hour), now.Add(-30*24*time.Hour), fresh)
	if err != nil || len(later) != 3 || later[0].Week != 0 || later[0].Month != 1 {
		t.Errorf("a week that starts after every command: %+v, %v", later, err)
	}
	if none, err := node.reader().HostUse(t.Context(), now.Add(2*time.Hour), now.Add(time.Hour), fresh); err != nil ||
		len(none) != 0 {
		t.Errorf("a month that starts after every command: %+v, %v", none, err)
	}
	if _, err := node.reader().HostUse(t.Context(), now.Add(-31*24*time.Hour), now.Add(-30*24*time.Hour), fresh); err == nil {
		t.Error("a week that starts before its month was accepted")
	}
}
