package code_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BOUNDS FIT THE BUDGETS THEY ARE SIZED FROM — each inequality a constant's
// own doc states, held here so moving one constant without the others fails.
func TestTheBoundsFitTheReadAndPurgeBudgets(t *testing.T) {
	t.Parallel()

	// A STORED PACK NEVER REACHES 2 GiB. Past it a version-2 index grows a
	// table of 64-bit offsets, and the bundle format's section sizes stop
	// being pure functions of the object count — which is what CheckBundle
	// compares exactly, for every bundle the store will ever hold. A push is
	// held here; a later compaction holds what it writes with pack-objects
	// --max-pack-size, and must keep to the same rule.
	if code.MaxPackBytes >= 1<<31 {
		t.Errorf("MaxPackBytes is %d, at or past the 2 GiB an index's 64-bit offset "+
			"table begins at", code.MaxPackBytes)
	}
	// A STORED BUNDLE HAS ROOM PAST THE CLIENT'S INPUT. What receive-pack
	// stores is the input COMPLETED — index-pack --fix-thin appends every base
	// a thin pack's deltas name — and the bundle adds the index and the
	// reverse index; so at equality a legal push at max_push_bytes would be
	// refused pack_too_large for bytes the client never sent.
	if code.MaxPackBytes <= config.CodeMaxPushBytesCeiling {
		t.Errorf("MaxPackBytes (%d) is not past the largest input a push may send "+
			"(CodeMaxPushBytesCeiling %d): a stored bundle must have room beyond the "+
			"client's input for --fix-thin's appended bases and its .idx and .rev, or a "+
			"push the limits admit is refused pack_too_large", code.MaxPackBytes,
			config.CodeMaxPushBytesCeiling)
	}
	// AND THE OBJECT STORE CAN RECORD IT: the largest bundle uploaded at the
	// pacing floor lands inside the write bound the collector deletes by.
	if pace := objstore.PaceFor(code.MaxPackBytes); pace >= objstore.RecordWithin {
		t.Errorf("a %d-byte bundle at the pacing floor takes %s, past RecordWithin (%s)",
			code.MaxPackBytes, pace, objstore.RecordWithin)
	}

	// A PURGE PART AND ITS BOOKKEEPING FIT ONE RECORD'S ROW BUDGET.
	if code.PurgePartRows+2 > code.MaxRecordRows {
		t.Errorf("a purge part deletes %d rows (PurgePartRows) and writes its counters, past "+
			"the %d one record's apply may touch (MaxRecordRows)", code.PurgePartRows,
			code.MaxRecordRows)
	}
	// AND SO DOES THE FINAL PURGE, whose statements are unbounded because the
	// decide forms it only once at most PurgePartRows of the repository's
	// pushes, refs and packs remain: those, every protection rule
	// (MaxProtectionsPerRepo), the name claim, the repository's row and the
	// deletion marker.
	if final := code.PurgePartRows + code.MaxProtectionsPerRepo + 3; final > code.MaxRecordRows {
		t.Errorf("the final purge removes up to %d rows (PurgePartRows %d + "+
			"MaxProtectionsPerRepo %d + 3), past the %d one record's apply may touch "+
			"(MaxRecordRows)", final, code.PurgePartRows, code.MaxProtectionsPerRepo,
			code.MaxRecordRows)
	}
	// THREE MAXIMAL PUSHES FIT ONE RECORD'S ROWS with the sweep still given
	// room: a push row, its refs and its pack each, the repository's
	// counters, the push history's share and the tombstone mark — and the
	// tombstone sweep takes what is left.
	rows := 3*(1+code.MaxCommandsPerPush+1) + 1 + code.HorizonDeletesPerRecord + 1
	if rows >= code.MaxRecordRows {
		t.Errorf("three maximal pushes write %d rows (3 × (1 + MaxCommandsPerPush %d + 1) + 1 + "+
			"HorizonDeletesPerRecord %d + 1), leaving the tombstone sweep nothing of the %d "+
			"budget (MaxRecordRows)", rows, code.MaxCommandsPerPush,
			code.HorizonDeletesPerRecord, code.MaxRecordRows)
	}
	if code.MaxRecordRows != statelog.ApplyTxRowBudget {
		t.Errorf("MaxRecordRows is %d and the framework's budget %d", code.MaxRecordRows,
			statelog.ApplyTxRowBudget)
	}
	// THE PUSH HISTORY'S REMOVAL KEEPS PACE: a record removes at least as many
	// expired push rows as it can add.
	if code.HorizonDeletesPerRecord < code.MaxPushesPerRecord {
		t.Errorf("a record removes %d expired push rows (HorizonDeletesPerRecord) and may add "+
			"%d (MaxPushesPerRecord), so an active repository's history outgrows its horizon",
			code.HorizonDeletesPerRecord, code.MaxPushesPerRecord)
	}
	// AND IN BYTES: a push row's width is its record's own commands and ids,
	// so a record adds at most MaxRecordBytes of history, and removes at
	// least that much wherever that much has expired.
	if code.HorizonBytesPerRecord < code.MaxRecordBytes {
		t.Errorf("a record removes %d bytes of expired push rows (HorizonBytesPerRecord) and "+
			"may add %d (MaxRecordBytes), so a wide history outgrows its horizon",
			code.HorizonBytesPerRecord, code.MaxRecordBytes)
	}

	// ONE MAXIMAL PUSH ROW FITS A PAGE: a page serves at least one row, so the
	// largest row a pushes page can hold — every command at the longest ref,
	// with the longest code a recorded command carries, and every introduced
	// id it lists — must fit the page's bytes, or a page that has to serve it
	// is past the reply ceiling.
	longest := code.Refusal("")
	for _, r := range code.Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	row := code.PushRow{
		PushID: strings.Repeat("p", 36), RepoID: strings.Repeat("r", 36),
		Position: statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1 << 30, Seq: 1 << 50},
		At:       time.Date(2031, 4, 2, 3, 14, 0, 999_999_000, time.UTC), Actor: strings.Repeat("a", 64),
		ActorKind: "operator", ActorSeat: strings.Repeat("s", 64), Node: strings.Repeat("n", 64),
		Atomic: true, PackID: strings.Repeat("k", 36), PackBytes: code.MaxPackBytes,
		IntroducedCount: 1 << 30, ReceivedAt: time.Date(2031, 4, 2, 3, 14, 0, 999_999_000, time.UTC),
	}
	for c := range code.MaxCommandsPerPush {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		row.Commands = append(row.Commands, code.Command{
			Ref: ref + strings.Repeat("x", code.MaxRefNameBytes-len(ref)),
			Old: oidOf(fmt.Sprint("old", c)), New: oidOf(fmt.Sprint("new", c)), Forced: true,
			Result: code.ResultNG, Reason: longest,
		})
	}
	for i := range code.MaxIntroduced {
		row.Introduced = append(row.Introduced, oidOf(fmt.Sprint("introduced", i)))
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > code.MaxPageBytes {
		t.Errorf("one maximal push row (MaxCommandsPerPush %d commands at MaxRefNameBytes %d, "+
			"MaxIntroduced %d ids) encodes to %d bytes, past MaxPageBytes (%d)",
			code.MaxCommandsPerPush, code.MaxRefNameBytes, code.MaxIntroduced, len(encoded),
			code.MaxPageBytes)
	}
	// AND TWO MAXIMAL PUSHES FIT ITS BYTES, refs at their longest.
	long := strings.Repeat("x", code.MaxRefNameBytes-len("refs/heads/")-8)
	var entries []code.PushEntry
	for p := range 2 {
		var cmds []code.Command
		for c := range code.MaxCommandsPerPush {
			cmds = append(cmds, code.Command{
				Ref: fmt.Sprintf("refs/heads/%s%08d", long, p*code.MaxCommandsPerPush+c),
				Old: code.ZeroOID, New: oidOf(fmt.Sprint(p, c)), Result: code.ResultOK,
			})
		}
		entries = append(entries, code.PushEntry{PushID: fmt.Sprint("push-", p), Commands: cmds,
			Pack: suitePack(fmt.Sprint(p))})
	}
	body, err := json.Marshal(code.PushBatch{RepoID: "repo", Pushes: entries})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > code.MaxRecordBytes {
		t.Errorf("two maximal pushes encode to %d bytes, past MaxRecordBytes (%d)", len(body),
			code.MaxRecordBytes)
	}

	// A PAGE LEAVES ITS REPLY ROOM: half the estate's reply ceiling, so the
	// envelope and the repository row always fit beside a full page.
	if code.MaxPageBytes > queue.MaxPayloadBytes/2 {
		t.Errorf("MaxPageBytes is %d, past half the %d-byte reply ceiling", code.MaxPageBytes,
			queue.MaxPayloadBytes)
	}

	// THE LIMITS A COMPANY DOES NOT SET ARE CONFIG'S, one value.
	if got := code.DefaultLimits(); got != (code.Limits{
		MaxPushBytes: config.DefaultCodeMaxPushBytes, MaxBlobBytes: config.DefaultCodeMaxBlobBytes,
		RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
	}) {
		t.Errorf("DefaultLimits is %+v, and config's defaults differ", got)
	}
}
