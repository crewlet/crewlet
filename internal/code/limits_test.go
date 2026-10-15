package code_test

import (
	"encoding/json"
	"fmt"
	"math"
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
	// is past the reply ceiling. The longest ref in ENCODED bytes: every byte
	// after its number a `<`, which a ref may hold and JSON writes as six.
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
			Ref: ref + strings.Repeat("<", code.MaxRefNameBytes-len(ref)),
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

	// A RETENTION PASS LEAVES THE LANE ITS SHARE AND ENDS BEFORE ITS TICK:
	// it waits twice a step's duration after each (a third of the lane), and
	// its budget is StallGrace — what a node six times slower than the holder
	// ends a pass behind by is (6/3 − 1) budgets, StallGrace itself — and
	// leaves most of the tick for that node to catch up before the next.
	if code.RetentionIdlePerStep != 2 {
		t.Errorf("a pass waits %v times a step's duration after it, want 2: a third of the "+
			"lane (RetentionLaneShare %v)", code.RetentionIdlePerStep, code.RetentionLaneShare)
	}
	if code.RetentionPassBudget != statelog.StallGrace ||
		(6*code.RetentionLaneShare-1)*float64(code.RetentionPassBudget) > float64(statelog.StallGrace) {
		t.Errorf("RetentionPassBudget is %v: a node six times slower than the holder must end a "+
			"pass no more than StallGrace (%v) behind", code.RetentionPassBudget, statelog.StallGrace)
	}
	if 2*code.RetentionPassBudget > code.RetentionInterval {
		t.Errorf("RetentionPassBudget (%v) is past half of RetentionInterval (%v): a node behind "+
			"the pass has less of the tick to catch up in than the pass ran",
			code.RetentionPassBudget, code.RetentionInterval)
	}

	// THE LIMITS A COMPANY DOES NOT SET ARE CONFIG'S, one value.
	if got := code.DefaultLimits(); got != (code.Limits{
		MaxPushBytes: config.DefaultCodeMaxPushBytes, MaxBlobBytes: config.DefaultCodeMaxBlobBytes,
		RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
	}) {
		t.Errorf("DefaultLimits is %+v, and config's defaults differ", got)
	}
}

// THE COMMITTER'S BOUNDS FIT A RECORD'S BUDGETS: what a batch is formed by is
// what the record it becomes is held to, so the committer never forms a record
// its own decide refuses, and a queue's bounds say what they claim.
func TestTheBoundsFitTheRecordBudgets(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-00000000b0b0"
	nodeID := strings.Repeat("n", 64) // the longest node id config admits
	principal := code.Principal{Kind: code.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
		Seat: strings.Repeat("s", 64)}
	at := time.Date(2031, 4, 2, 3, 14, 0, 999_999_999, time.FixedZone("x", -(11*3600+59*60)))
	minimal := code.Proposal{
		PushID: "0190f3c4-0000-7000-8000-000000000001", RepoID: repo, Principal: principal,
		Node: nodeID, ReceivedAt: at,
		Commands: []code.Command{{Ref: "refs/heads/a", Old: code.ZeroOID, New: oidOf("a")}},
	}
	maximal := minimal
	maximal.PushID = "0190f3c4-0000-7000-8000-000000000002"
	maximal.Commands = nil
	for c := range code.MaxCommandsPerPush {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		maximal.Commands = append(maximal.Commands, code.Command{
			Ref: ref + strings.Repeat("x", code.MaxRefNameBytes-len(ref)),
			Old: oidOf(fmt.Sprint("old", c)), New: oidOf(fmt.Sprint("new", c)), Forced: true,
		})
	}
	for i := range code.MaxIntroduced {
		maximal.Introduced = append(maximal.Introduced, oidOf(fmt.Sprint("introduced", i)))
	}
	maximal.IntroducedCount = 1 << 30
	maximal.Pack = suitePack("maximal")
	maximal.Pack.PackID = maximal.PushID
	if err := maximal.Validate(); err != nil {
		t.Fatalf("the maximal proposal is not one: %v", err)
	}
	big, err := code.EntryBound(maximal)
	if err != nil {
		t.Fatal(err)
	}
	small, err := code.EntryBound(minimal)
	if err != nil {
		t.Fatal(err)
	}

	// TWO MAXIMAL PUSHES SHARE A RECORD'S BYTES, AND THREE DO NOT.
	if got := code.RecordOverheadBytes + 2*big; got > code.MaxRecordBytes {
		t.Errorf("two maximal entries (EntryBound %d each) and RecordOverheadBytes (%d) are %d bytes, "+
			"past MaxRecordBytes (%d)", big, code.RecordOverheadBytes, got, code.MaxRecordBytes)
	}
	if got := code.RecordOverheadBytes + 3*big; got <= code.MaxRecordBytes {
		t.Errorf("three maximal entries fit MaxRecordBytes (%d of %d): the bound states two",
			got, code.MaxRecordBytes)
	}
	// THREE MAXIMAL PUSHES SHARE A RECORD'S ROWS, AND FOUR DO NOT.
	if rows := code.RowBound(maximal); rows != code.MaxCommandsPerPush+2 {
		t.Errorf("a maximal push bounds %d rows, want MaxCommandsPerPush (%d) + its row + its pack",
			rows, code.MaxCommandsPerPush)
	}
	if got := code.RecordRows(3 * code.RowBound(maximal)); got > code.MaxRecordRows {
		t.Errorf("three maximal pushes bound %d rows with the record's own (RecordRows), past "+
			"MaxRecordRows (%d)", got, code.MaxRecordRows)
	}
	if got := code.RecordRows(4 * code.RowBound(maximal)); got <= code.MaxRecordRows {
		t.Errorf("four maximal pushes bound %d rows, within MaxRecordRows (%d)", got, code.MaxRecordRows)
	}
	// A FULL BATCH OF THE SMALLEST PUSHES FITS BOTH.
	if got := code.RecordOverheadBytes + code.MaxPushesPerRecord*small; got > code.MaxRecordBytes {
		t.Errorf("MaxPushesPerRecord (%d) minimal entries are %d bytes, past MaxRecordBytes (%d)",
			code.MaxPushesPerRecord, got, code.MaxRecordBytes)
	}
	if got := code.RecordRows(code.MaxPushesPerRecord * code.RowBound(minimal)); got > code.MaxRecordRows {
		t.Errorf("MaxPushesPerRecord (%d) minimal pushes bound %d rows, past MaxRecordRows (%d)",
			code.MaxPushesPerRecord, got, code.MaxRecordRows)
	}

	// DECIDING GROWS A COMMAND BY AT MOST CommandResultBound, at the longest
	// code a record may carry.
	longest := code.Refusal("")
	for _, r := range code.Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	undecided := code.Command{Ref: "refs/heads/a", Old: code.ZeroOID, New: oidOf("a")}
	base, _ := json.Marshal(undecided)
	for _, decided := range []code.Command{
		{Ref: undecided.Ref, Old: undecided.Old, New: undecided.New, Result: code.ResultNoop},
		{Ref: undecided.Ref, Old: undecided.Old, New: undecided.New, Result: code.ResultNG, Reason: longest},
	} {
		grown, _ := json.Marshal(decided)
		if growth := len(grown) - len(base); growth > code.CommandResultBound {
			t.Errorf("deciding a command %s %s grows it %d bytes, past CommandResultBound (%d)",
				decided.Result, decided.Reason, growth, code.CommandResultBound)
		}
	}

	// A RECORD'S OWN BYTES FIT RecordOverheadBytes: a real encoded record of
	// one entry, with the longest node id, the widest generation and an
	// authored instant in a zone, less its entry.
	entry, err := json.Marshal(code.EntryOf(minimal))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(code.PushBatch{RepoID: repo, Pushes: []code.PushEntry{code.EntryOf(minimal)}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := code.Encode(code.MutationRecord{
		RecordEnvelope: code.RecordEnvelope{
			OpID: statelog.NewOpID(at, "code.push"), Subject: code.RepoSubject(repo), Op: code.OpPush,
			CreatedAt: at, Gen: 1<<32 - 1, Writer: nodeID, Scope: code.Scope{code.RepoPath(repo)},
		},
		Expect: 1 << 63, Mutation: payload, Actor: nodeID, ActorKind: code.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if overhead := len(record) - len(entry); overhead > code.RecordOverheadBytes {
		t.Errorf("a one-entry record carries %d bytes beside its entry, past RecordOverheadBytes (%d)",
			overhead, code.RecordOverheadBytes)
	}

	// A QUEUE'S BYTES HOLD A FULL RECORD BEHIND ONE IN FLIGHT, and a queue
	// of the smallest pushes meets its count before its bytes.
	if code.CommitterInboxBytes < 2*code.MaxRecordBytes {
		t.Errorf("CommitterInboxBytes (%d) is under two records (MaxRecordBytes %d)",
			code.CommitterInboxBytes, code.MaxRecordBytes)
	}
	if got := code.CommitterInbox * small; got > code.CommitterInboxBytes {
		t.Errorf("CommitterInbox (%d) minimal pushes weigh %d bytes, past CommitterInboxBytes (%d), "+
			"so small pushes meet the byte bound rather than the count", code.CommitterInbox, got,
			code.CommitterInboxBytes)
	}
}

// THE LIMITS A PUSH IS HELD TO ARE CONFIG'S OWN READING OF code.limits — one
// resolution of the defaults, so `crewlet validate`'s order rules and the
// committer's decide can never disagree about what an unset field means.
func TestLimitsOfIsConfigsReading(t *testing.T) {
	t.Parallel()
	if got := code.LimitsOf(config.Code{}); got != code.DefaultLimits() {
		t.Errorf("an unset code.limits is held to %+v, and the domain's defaults are %+v",
			got, code.DefaultLimits())
	}
	resolved := config.CodeLimits{}.Resolved()
	if got := code.LimitsOf(config.Code{}); got.MaxPushBytes != resolved.MaxPushBytes ||
		got.MaxBlobBytes != resolved.MaxBlobBytes || got.RepoMaxBytes != resolved.RepoMaxBytes {
		t.Errorf("an unset code.limits is held to %+v, and config resolves it to %+v",
			got, resolved)
	}
	// EACH FIELD ON ITS OWN, so a default taken from the wrong field — the
	// file limit defaulting to the push limit — is seen.
	for name, tc := range map[string]struct {
		set  config.CodeLimits
		want code.Limits
	}{
		"a push limit alone": {
			set: config.CodeLimits{MaxPushBytes: 512 << 20},
			want: code.Limits{MaxPushBytes: 512 << 20,
				MaxBlobBytes: config.DefaultCodeMaxBlobBytes, RepoMaxBytes: config.DefaultCodeRepoMaxBytes},
		},
		"a file limit alone": {
			set: config.CodeLimits{MaxBlobBytes: 10 << 20},
			want: code.Limits{MaxPushBytes: config.DefaultCodeMaxPushBytes,
				MaxBlobBytes: 10 << 20, RepoMaxBytes: config.DefaultCodeRepoMaxBytes},
		},
		"a repository limit alone": {
			set: config.CodeLimits{RepoMaxBytes: 4 << 30},
			want: code.Limits{MaxPushBytes: config.DefaultCodeMaxPushBytes,
				MaxBlobBytes: config.DefaultCodeMaxBlobBytes, RepoMaxBytes: 4 << 30},
		},
	} {
		if got := code.LimitsOf(config.Code{Native: true, Limits: tc.set}); got != tc.want {
			t.Errorf("%s: code.limits %+v is held to %+v, want %+v", name, tc.set, got, tc.want)
		}
	}
}

// THE CODE LOG'S GIT CENSUS FOLLOWS ITS CEILING: the barriers that fill a
// tenth of it over the trim's min_age, so a resize of the log moves the census
// with it — and a min_age under a day counts as one.
//
// OVER THE WHOLE min_age, a fraction of a day included: a barrier stays on the
// log for all of it, so a min_age of 36 hours divided as one day would let the
// git clients' barriers fill half again the tenth they were sized for, and one
// of 47 hours nearly twice it — which census_drift's factor of two would then
// not catch until four tenths. And EXACT at every ceiling, the largest a
// stream can declare included, which a day's nanoseconds times the ceiling
// overflows in 64 bits.
func TestGitReadsFollowTheCodeLogsCeiling(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	for _, tc := range []struct {
		ceiling int64
		minAge  time.Duration
		want    int
	}{
		{1 << 30, week, 112_788},
		{16 << 30, week, 1_804_608},
		{1 << 30, time.Hour, 789_516},
		{1 << 30, 24 * time.Hour, 789_516},
		{1 << 30, 36 * time.Hour, 526_344},
		{1 << 30, 47 * time.Hour, 403_157},
		{math.MaxInt64, 24 * time.Hour, 6_781_891_203_569_688},
		{math.MaxInt64, week, 968_841_600_509_955},
		{0, week, 0},
	} {
		if got := code.GitReadsPerDay(tc.ceiling, tc.minAge); got != tc.want {
			t.Errorf("GitReadsPerDay(%d, %v) = %d, want %d", tc.ceiling, tc.minAge, got, tc.want)
		}
	}
}
