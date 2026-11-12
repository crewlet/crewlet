package gitlet_test

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
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
	if gitlet.MaxPackBytes >= 1<<31 {
		t.Errorf("MaxPackBytes is %d, at or past the 2 GiB an index's 64-bit offset "+
			"table begins at", gitlet.MaxPackBytes)
	}
	// A STORED BUNDLE HAS ROOM PAST THE CLIENT'S INPUT. What receive-pack
	// stores is the input COMPLETED — index-pack --fix-thin appends every base
	// a thin pack's deltas name — and the bundle adds the index and the
	// reverse index; so at equality a legal push at max_push_bytes would be
	// refused pack_too_large for bytes the client never sent.
	if gitlet.MaxPackBytes <= config.CodeMaxPushBytesCeiling {
		t.Errorf("MaxPackBytes (%d) is not past the largest input a push may send "+
			"(CodeMaxPushBytesCeiling %d): a stored bundle must have room beyond the "+
			"client's input for --fix-thin's appended bases and its .idx and .rev, or a "+
			"push the limits admit is refused pack_too_large", gitlet.MaxPackBytes,
			config.CodeMaxPushBytesCeiling)
	}
	// AND THE OBJECT STORE CAN RECORD IT: the largest bundle uploaded at the
	// pacing floor, under the whole bound its put is given, lands inside the
	// write bound the collector deletes by — its key is minted as the upload
	// begins, and a row names it only once the upload ends.
	if bound := gitlet.UploadDeadline(gitlet.MaxPackBytes); bound >= objstore.RecordWithin {
		t.Errorf("a %d-byte bundle's put is given %s, past RecordWithin (%s)",
			gitlet.MaxPackBytes, bound, objstore.RecordWithin)
	}

	// A PURGE PART AND ITS BOOKKEEPING FIT ONE RECORD'S ROW BUDGET.
	if gitlet.PurgePartRows+2 > gitlet.MaxRecordRows {
		t.Errorf("a purge part deletes %d rows (PurgePartRows) and writes its counters, past "+
			"the %d one record's apply may touch (MaxRecordRows)", gitlet.PurgePartRows,
			gitlet.MaxRecordRows)
	}
	// AND SO DOES THE FINAL PURGE, whose statements are unbounded because the
	// decide forms it only once at most PurgePartRows of the repository's
	// pushes, refs, packs, retired packs and changes' revisions, authors,
	// bodies and rows remain: those, every protection rule
	// (MaxProtectionsPerRepo) and every path owner (MaxPathOwners, one row an
	// owner), the published chain — which no part removes: MaxPublishedLayers
	// layers of MaxLayerParts parts each — the name claim, the repository's
	// row, its kind's count and the deletion marker.
	chain := gitlet.MaxPublishedLayers * (1 + gitlet.MaxLayerParts)
	if final := gitlet.PurgePartRows + gitlet.MaxProtectionsPerRepo + gitlet.MaxPathOwners + chain + 4; final >
		gitlet.MaxRecordRows {
		t.Errorf("the final purge removes up to %d rows (PurgePartRows %d + MaxProtectionsPerRepo %d + "+
			"MaxPathOwners %d + a chain's %d + 4), past the %d one record's apply may touch (MaxRecordRows)",
			final, gitlet.PurgePartRows, gitlet.MaxProtectionsPerRepo, gitlet.MaxPathOwners, chain,
			gitlet.MaxRecordRows)
	}
	// AND A RECORD CARRYING A REPOSITORY'S SETTINGS WHOLE: a settings change
	// or a restore deletes its rules and path owners and writes the new ones,
	// and moves the repository's row — a create writes them with its claim.
	if settings := 2*(gitlet.MaxProtectionsPerRepo+gitlet.MaxPathOwners) + 1; settings > gitlet.MaxRecordRows {
		t.Errorf("a settings record touches up to %d rows (twice MaxProtectionsPerRepo %d and MaxPathOwners "+
			"%d, and the row), past the %d one record's apply may touch (MaxRecordRows)", settings,
			gitlet.MaxProtectionsPerRepo, gitlet.MaxPathOwners, gitlet.MaxRecordRows)
	}
	// A MAXIMAL PUSH OF DELETIONS FITS ONE RECORD'S ROWS with its sweep's
	// room: a push row, its pack and its refs, the object each deletion left,
	// an old tombstone swept for each deletion's new one, the repository's
	// counters, the push history's share and the tombstone mark — or no push
	// could delete a thousand refs at a repository holding its bound of
	// tombstones without its record writing them as removals.
	rows := 1 + 3*gitlet.MaxCommandsPerPush + 1 + 1 + gitlet.HorizonDeletesPerRecord + 1
	if rows > gitlet.MaxRecordRows {
		t.Errorf("a maximal push of deletions touches %d rows (1 + 3 × MaxCommandsPerPush %d + 1 + "+
			"1 + HorizonDeletesPerRecord %d + 1), past the %d budget (MaxRecordRows)", rows,
			gitlet.MaxCommandsPerPush, gitlet.HorizonDeletesPerRecord, gitlet.MaxRecordRows)
	}
	if gitlet.MaxRecordRows != statelog.ApplyTxRowBudget {
		t.Errorf("MaxRecordRows is %d and the framework's budget %d", gitlet.MaxRecordRows,
			statelog.ApplyTxRowBudget)
	}
	// THE PUSH HISTORY'S REMOVAL KEEPS PACE: a record removes at least as many
	// expired push rows as it can add.
	if gitlet.HorizonDeletesPerRecord < gitlet.MaxPushesPerRecord {
		t.Errorf("a record removes %d expired push rows (HorizonDeletesPerRecord) and may add "+
			"%d (MaxPushesPerRecord), so an active repository's history outgrows its horizon",
			gitlet.HorizonDeletesPerRecord, gitlet.MaxPushesPerRecord)
	}
	// AND IN BYTES: a push row's width is its record's own commands and ids,
	// so a record adds at most MaxRecordBytes of history, and removes at
	// least that much wherever that much has expired.
	if gitlet.HorizonBytesPerRecord < gitlet.MaxRecordBytes {
		t.Errorf("a record removes %d bytes of expired push rows (HorizonBytesPerRecord) and "+
			"may add %d (MaxRecordBytes), so a wide history outgrows its horizon",
			gitlet.HorizonBytesPerRecord, gitlet.MaxRecordBytes)
	}

	// ONE MAXIMAL PUSH ROW FITS A PAGE: a page serves at least one row, so the
	// largest row a pushes page can hold — every command at the longest ref,
	// with the longest code a recorded command carries, every introduced id
	// it lists, the longest way and every field of a seat's run at its bound
	// — a coding box's run id a UUID, the one spelling a run id has — must
	// fit the page's bytes, or a page that has to serve it is past the reply
	// ceiling. The longest ref and run field in ENCODED bytes: every byte
	// after a ref's number, and every byte of a run's field, a `<`, which both
	// may hold and JSON writes as six.
	longest := gitlet.Refusal("")
	for _, r := range gitlet.Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	widestVia := gitlet.PushVia("")
	for _, v := range gitlet.PushVias() {
		if len(v) > len(widestVia) {
			widestVia = v
		}
	}
	runField := strings.Repeat("<", gitlet.MaxProvenanceBytes)
	row := gitlet.PushRow{
		PushID: strings.Repeat("p", 36), RepoID: strings.Repeat("r", 36),
		Position: statelog.Position{Stream: "CREWLET_CODE_LOG", Generation: 1 << 30, Seq: 1 << 50},
		At:       time.Date(2031, 4, 2, 3, 14, 0, 999_999_000, time.UTC), Actor: strings.Repeat("a", 64),
		ActorKind: "operator", ActorSeat: strings.Repeat("s", 64), Via: widestVia,
		Run: gitlet.RunProvenance{TurnID: runField, WorkBackend: runField, WorkKey: runField, Model: runField,
			RunID: "3f1c2b4a-5d6e-4f70-8a9b-0c1d2e3f4a5b", Harness: runField},
		Node: strings.Repeat("n", 64), Atomic: true, PackID: strings.Repeat("k", 36), PackBytes: gitlet.MaxPackBytes,
		IntroducedCount: 1 << 30, ReceivedAt: time.Date(2031, 4, 2, 3, 14, 0, 999_999_000, time.UTC),
	}
	if gitlet.FitProvenance(runField) != runField {
		t.Fatalf("a run field of %d `<` is not one a stamp carries as it is: build the widest run from "+
			"what FitProvenance admits", len(runField))
	}
	for c := range gitlet.MaxCommandsPerPush {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		row.Commands = append(row.Commands, gitlet.Command{
			Ref: ref + strings.Repeat("<", gitlet.MaxRefNameBytes-len(ref)),
			Old: oidOf(fmt.Sprint("old", c)), New: oidOf(fmt.Sprint("new", c)), Forced: true,
			Result: gitlet.ResultNG, Reason: longest,
		})
	}
	for i := range gitlet.MaxIntroduced {
		row.Introduced = append(row.Introduced, oidOf(fmt.Sprint("introduced", i)))
	}
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > gitlet.MaxPageBytes {
		t.Errorf("one maximal push row (MaxCommandsPerPush %d commands at MaxRefNameBytes %d, "+
			"MaxIntroduced %d ids) encodes to %d bytes, past MaxPageBytes (%d)",
			gitlet.MaxCommandsPerPush, gitlet.MaxRefNameBytes, gitlet.MaxIntroduced, len(encoded),
			gitlet.MaxPageBytes)
	}
	// AND TWO MAXIMAL PUSHES FIT ITS BYTES, refs at their longest.
	long := strings.Repeat("x", gitlet.MaxRefNameBytes-len("refs/heads/")-8)
	var entries []gitlet.PushEntry
	for p := range 2 {
		var cmds []gitlet.Command
		for c := range gitlet.MaxCommandsPerPush {
			cmds = append(cmds, gitlet.Command{
				Ref: fmt.Sprintf("refs/heads/%s%08d", long, p*gitlet.MaxCommandsPerPush+c),
				Old: gitlet.ZeroOID, New: oidOf(fmt.Sprint(p, c)), Result: gitlet.ResultOK,
			})
		}
		entries = append(entries, gitlet.PushEntry{PushID: fmt.Sprint("push-", p), Via: gitlet.ViaGit,
			Commands: cmds, Pack: suitePack(fmt.Sprint(p))})
	}
	body, err := json.Marshal(gitlet.PushBatch{RepoID: "repo", Pushes: entries})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > gitlet.MaxRecordBytes {
		t.Errorf("two maximal pushes encode to %d bytes, past MaxRecordBytes (%d)", len(body),
			gitlet.MaxRecordBytes)
	}
	// AND A HOLD OF A SEAT'S BRANCH AT ITS LONGEST: a turn's id at the
	// provenance bound and a ref at MaxRefNameBytes of a byte JSON escapes
	// six-fold — the widest a hold record carries, which the code suite's one
	// push payload a subject kind does not (its decode and apply are
	// TestARefHoldMovesNoClock's).
	branch := gitlet.SeatNamespace("dev")
	hold, err := json.Marshal(gitlet.Hold{RepoID: strings.Repeat("r", 36),
		TurnID: strings.Repeat("<", gitlet.MaxProvenanceBytes),
		Ref:    branch + strings.Repeat("<", gitlet.MaxRefNameBytes-len(branch))})
	if err != nil {
		t.Fatal(err)
	}
	if len(hold) > gitlet.MaxRecordBytes {
		t.Errorf("the widest hold of a seat's branch encodes to %d bytes, past MaxRecordBytes (%d)", len(hold),
			gitlet.MaxRecordBytes)
	}

	// A PAGE LEAVES ITS REPLY ROOM: half the estate's reply ceiling, so the
	// envelope and the repository row always fit beside a full page.
	if gitlet.MaxPageBytes > queue.MaxPayloadBytes/2 {
		t.Errorf("MaxPageBytes is %d, past half the %d-byte reply ceiling", gitlet.MaxPageBytes,
			queue.MaxPayloadBytes)
	}

	// A RETENTION PASS LEAVES THE LANE ITS SHARE AND ENDS BEFORE ITS TICK:
	// it waits twice a step's duration after each (a third of the lane), and
	// its budget is StallGrace — what a node six times slower than the holder
	// ends a pass behind by is (6/3 − 1) budgets, StallGrace itself — and
	// leaves most of the tick for that node to catch up before the next.
	if gitlet.RetentionIdlePerStep != 2 {
		t.Errorf("a pass waits %v times a step's duration after it, want 2: a third of the "+
			"lane (RetentionLaneShare %v)", gitlet.RetentionIdlePerStep, gitlet.RetentionLaneShare)
	}
	if gitlet.RetentionPassBudget != statelog.StallGrace ||
		(6*gitlet.RetentionLaneShare-1)*float64(gitlet.RetentionPassBudget) > float64(statelog.StallGrace) {
		t.Errorf("RetentionPassBudget is %v: a node six times slower than the holder must end a "+
			"pass no more than StallGrace (%v) behind", gitlet.RetentionPassBudget, statelog.StallGrace)
	}
	if 2*gitlet.RetentionPassBudget > gitlet.RetentionInterval {
		t.Errorf("RetentionPassBudget (%v) is past half of RetentionInterval (%v): a node behind "+
			"the pass has less of the tick to catch up in than the pass ran",
			gitlet.RetentionPassBudget, gitlet.RetentionInterval)
	}

	// THE LIMITS A COMPANY DOES NOT SET ARE CONFIG'S, one value.
	if got := gitlet.DefaultLimits(); got != (gitlet.Limits{
		MaxPushBytes: config.DefaultCodeMaxPushBytes, MaxBlobBytes: config.DefaultCodeMaxBlobBytes,
		RepoMaxBytes: config.DefaultCodeRepoMaxBytes, PushesPerSeatDay: config.DefaultCodePushesPerSeatDay,
		MergeByFloor: gitlet.MergeBy(config.CodeMergeBy("").Resolved()),
	}) {
		t.Errorf("DefaultLimits is %+v, and config's defaults differ", got)
	}
}

// EVERY PUSH IS COUNTED BELOW THE TOP BOUNDARY of the histograms that measure
// it: its bundle in crewlet.code.push.bytes, whose byte boundaries must reach
// the largest bundle one push may store (MaxPackBytes), and its duration in
// the millisecond histograms, whose boundaries must reach the bound on the
// longest transfer the engine paces (objstore.RecordWithin, which
// TestTheBoundsFitTheReadAndPurgeBudgets holds the largest bundle at the
// pacing floor under). Past its top boundary a
// value lands in the overflow, where every push past 64 KiB was counted while
// one set of boundaries ending at 65 536 served every unit.
//
// Mutation: end the byte boundaries below MaxPackBytes, or the millisecond
// boundaries below twelve hours, and this fails.
func TestEveryPushIsCountedBelowTheTopBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		unit, what string
		largest    float64
	}{
		{metrics.UnitBytes, "the largest bundle one push may store (MaxPackBytes)", float64(gitlet.MaxPackBytes)},
		{metrics.UnitMilliseconds, "the longest transfer the engine paces (objstore.RecordWithin)",
			float64(objstore.RecordWithin) / float64(time.Millisecond)},
	} {
		bounds, ok := metrics.Bounds(tc.unit)
		if !ok {
			t.Errorf("%q has no histogram boundaries", tc.unit)
			continue
		}
		if top := bounds[len(bounds)-1]; top < tc.largest {
			t.Errorf("the %s histograms end at %v, below %s, %v: it is counted in the overflow", tc.unit,
				top, tc.what, tc.largest)
		}
	}
	for _, in := range metrics.Catalogue() {
		if in.Name == metrics.CodePushBytes && in.Unit != metrics.UnitBytes {
			t.Errorf("%s is recorded in %q, not in the bytes the bound above is stated in", in.Name, in.Unit)
		}
	}
}

// A DELTA PASSES THE WIDEST FULL ANSWER ONLY BY TOMBSTONES IN PLACE OF PACKS.
// The reader answers a delta only while it carries no more ROWS than the full
// answer would — live changes and tombstones together, against the live refs
// and every pack (TestADeltaLargerThanTheFullAnswerIsAnsweredInFull holds that
// through the real reader) — so by rows the tombstones give it no new worst
// case. What is held here is what that rule leaves in BYTES, every row at its
// widest — every name at MaxRefNameBytes, every position the widest the packed
// form admits — against the full answer of a repository of as many live refs
// and packs: the widest delta of that many rows, filled widest row first from
// what a delta can carry, passes it by at most one tombstone less one pack row
// (the narrowest a repository holds) for each pack, up to MaxTombstonesPerRepo
// of them — by nothing for names JSON writes as themselves, and by the figures
// MaxTombstonesPerRepo states for names JSON spells longest. And the bound
// itself is the live refs' own, for the reasons MaxTombstonesPerRepo gives.
func TestADeltaPassesTheWidestFullAnswerOnlyByTombstonesInPlaceOfPacks(t *testing.T) {
	t.Parallel()
	if gitlet.MaxTombstonesPerRepo != gitlet.MaxRefsPerRepo {
		t.Errorf("MaxTombstonesPerRepo is %d and MaxRefsPerRepo %d: the reasons the tombstones' "+
			"bound gives — a clean-up of a full repository keeps its whole horizon, and what a "+
			"delta of them costs the reader — are the live refs' own bound",
			gitlet.MaxTombstonesPerRepo, gitlet.MaxRefsPerRepo)
	}
	width := func(row any) int {
		b, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	widest := statelog.Position{Stream: topics.CodeLogStream, Generation: statelog.MaxGeneration,
		Seq: statelog.GenerationStride - 1}
	narrowest := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 1}
	// A name at MaxRefNameBytes of the character JSON spells longest (`<`, six
	// bytes), and of one it writes as itself.
	name := func(c string) string {
		ref := "refs/heads/" + strings.Repeat(c, gitlet.MaxRefNameBytes-len("refs/heads/"))
		if !gitref.ValidRef(ref) {
			t.Fatalf("%.24s… is no ref a repository holds", ref)
		}
		return ref
	}
	long, plain := name("<"), name("x")
	tomb := width(gitlet.RefRow{Ref: long, Position: widest})
	live := width(gitlet.RefRow{Ref: long, OID: oidOf("x"), Position: widest})
	plainTomb := width(gitlet.RefRow{Ref: plain, Position: widest})
	plainLive := width(gitlet.RefRow{Ref: plain, OID: oidOf("x"), Position: widest})
	// THE NARROWEST PACK ROW a repository holds — the one a tombstone gains
	// most by standing in for: one object, in the least bundle of it
	// PackRef.Check admits, added at the narrowest position and its content
	// dated from there.
	pack := *suitePack("narrowest")
	pack.PackID = "0190f3c4-0000-7000-8000-000000000001"
	if err := pack.Check(); err != nil {
		t.Fatalf("the narrowest pack is no pack a repository holds: %v", err)
	}
	smaller := pack
	smaller.Size--
	if smaller.Check() == nil {
		t.Fatalf("a bundle of %d bytes is admitted: the pack row is not the narrowest", smaller.Size)
	}
	packRow := width(gitlet.PackRow{PackID: pack.PackID, Part: "pack", Object: pack.Object,
		Hash: pack.Hash, Size: pack.Size, IndexBytes: pack.IndexBytes, ReverseBytes: pack.ReverseBytes,
		GitChecksum: pack.GitChecksum, Objects: pack.Objects, Kind: "push", State: "live",
		Added: narrowest, Content: narrowest})

	// THE FIGURES MaxTombstonesPerRepo's doc states.
	if tomb != 1_567 || plainTomb != 347 || packRow != 459 {
		t.Errorf("at their widest a tombstone is %d bytes and one of a plain name %d, and the "+
			"narrowest pack row %d: MaxTombstonesPerRepo states 1 567, 347 and 459", tomb,
			plainTomb, packRow)
	}

	// widestDelta is the most bytes a delta of rows rows can carry: filled
	// widest row first from what a delta may hold — the repository's live
	// refs, its tombstones and its packs.
	type kind struct{ rows, width int }
	widestDelta := func(rows int, kinds ...kind) int {
		slices.SortFunc(kinds, func(a, b kind) int { return b.width - a.width })
		bytes := 0
		for _, k := range kinds {
			n := min(rows, k.rows)
			bytes, rows = bytes+n*k.width, rows-n
		}
		return bytes
	}
	for _, packs := range []int{0, 1, 20_000, gitlet.MaxTombstonesPerRepo, 2 * gitlet.MaxTombstonesPerRepo} {
		rows := gitlet.MaxRefsPerRepo + packs // the full answer's: the most a delta is let carry
		stones := min(packs, gitlet.MaxTombstonesPerRepo)

		// NAMES JSON SPELLS LONGEST: a tombstone less a pack row a pack.
		full := gitlet.MaxRefsPerRepo*live + packs*packRow
		delta := widestDelta(rows, kind{gitlet.MaxRefsPerRepo, live},
			kind{gitlet.MaxTombstonesPerRepo, tomb}, kind{packs, packRow})
		if over, stated := delta-full, stones*(tomb-packRow); over > stated {
			t.Errorf("with %d packs the widest delta passes the widest full answer (%d bytes) by %d "+
				"bytes, past the %d MaxTombstonesPerRepo states (%d bytes a pack, for %d of them)",
				packs, full, over, stated, tomb-packRow, stones)
		}
		if packs == 20_000 && (megabytes(delta-full) != 22 || megabytes(full) != 171) {
			t.Errorf("with 20 000 packs the widest delta passes the %d-byte widest full answer by %d "+
				"bytes: MaxTombstonesPerRepo states 22 MB beside 171 MB", full, delta-full)
		}

		// NAMES JSON WRITES AS THEMSELVES: not a byte.
		plainFull := gitlet.MaxRefsPerRepo*plainLive + packs*packRow
		if plainDelta := widestDelta(rows, kind{gitlet.MaxRefsPerRepo, plainLive},
			kind{gitlet.MaxTombstonesPerRepo, plainTomb}, kind{packs, packRow}); plainDelta > plainFull {
			t.Errorf("with %d packs and plain names the widest delta is %d bytes, past the widest "+
				"full answer's %d: a tombstone of a plain name (%d bytes) is wider than a pack row (%d)",
				packs, plainDelta, plainFull, plainTomb, packRow)
		}
	}
}

// megabytes is bytes in whole megabytes, to the nearest.
func megabytes(bytes int) int { return (bytes + 500_000) / 1_000_000 }

// THE COMMITTER'S BOUNDS FIT A RECORD'S BUDGETS: what a batch is formed by is
// what the record it becomes is held to, so the committer never forms a record
// its own decide refuses, and a queue's bounds say what they claim.
func TestTheBoundsFitTheRecordBudgets(t *testing.T) {
	t.Parallel()
	const repo = "0190f3c4-0000-7000-8000-00000000b0b0"
	nodeID := strings.Repeat("n", 64) // the longest node id config admits
	principal := gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: strings.Repeat("o", 64),
		Seat: strings.Repeat("s", 64)}
	at := time.Date(2031, 4, 2, 3, 14, 0, 999_999_999, time.FixedZone("x", -(11*3600+59*60)))
	minimal := gitlet.Proposal{
		PushID: "0190f3c4-0000-7000-8000-000000000001", RepoID: repo, Principal: principal,
		Via: gitlet.ViaGit, Node: nodeID, ReceivedAt: at,
		Commands: []gitlet.Command{{Ref: "refs/heads/a", Old: gitlet.ZeroOID, New: oidOf("a")}},
	}
	maximal := minimal
	maximal.PushID = "0190f3c4-0000-7000-8000-000000000002"
	maximal.Commands = nil
	for c := range gitlet.MaxCommandsPerPush {
		ref := fmt.Sprintf("refs/heads/%08d", c)
		maximal.Commands = append(maximal.Commands, gitlet.Command{
			Ref: ref + strings.Repeat("x", gitlet.MaxRefNameBytes-len(ref)),
			Old: oidOf(fmt.Sprint("old", c)), New: oidOf(fmt.Sprint("new", c)), Forced: true,
		})
	}
	for i := range gitlet.MaxIntroduced {
		maximal.Introduced = append(maximal.Introduced, oidOf(fmt.Sprint("introduced", i)))
	}
	maximal.IntroducedCount = 1 << 30
	maximal.Pack = suitePack("maximal")
	maximal.Pack.PackID = maximal.PushID
	if err := maximal.Validate(); err != nil {
		t.Fatalf("the maximal proposal is not one: %v", err)
	}
	big, err := gitlet.EntryBound(maximal)
	if err != nil {
		t.Fatal(err)
	}
	small, err := gitlet.EntryBound(minimal)
	if err != nil {
		t.Fatal(err)
	}

	// TWO MAXIMAL PUSHES SHARE A RECORD'S BYTES, AND THREE DO NOT.
	if got := gitlet.RecordOverheadBytes + 2*big; got > gitlet.MaxRecordBytes {
		t.Errorf("two maximal entries (EntryBound %d each) and RecordOverheadBytes (%d) are %d bytes, "+
			"past MaxRecordBytes (%d)", big, gitlet.RecordOverheadBytes, got, gitlet.MaxRecordBytes)
	}
	if got := gitlet.RecordOverheadBytes + 3*big; got <= gitlet.MaxRecordBytes {
		t.Errorf("three maximal entries fit MaxRecordBytes (%d of %d): the bound states two",
			got, gitlet.MaxRecordBytes)
	}
	// THE BYTE-MAXIMAL PUSH IS ONE OF FORCED UPDATES, counted two rows a
	// command — its ref, and the object it left — so ONE shares a record's
	// rows with nothing of its size.
	if rows := gitlet.RowBound(maximal); rows != 2*gitlet.MaxCommandsPerPush+2 {
		t.Errorf("a maximal push of forced updates bounds %d rows, want two a command (%d) + its row "+
			"+ its pack", rows, 2*gitlet.MaxCommandsPerPush)
	}
	if got := gitlet.RecordRows(gitlet.RowBound(maximal)); got > gitlet.MaxRecordRows {
		t.Errorf("a maximal push of forced updates bounds %d rows with the record's own (RecordRows), "+
			"past MaxRecordRows (%d)", got, gitlet.MaxRecordRows)
	}
	if got := gitlet.RecordRows(2 * gitlet.RowBound(maximal)); got <= gitlet.MaxRecordRows {
		t.Errorf("two maximal pushes of forced updates bound %d rows, within MaxRecordRows (%d)", got,
			gitlet.MaxRecordRows)
	}
	// THREE MAXIMAL PUSHES OF FAST-FORWARD UPDATES SHARE A RECORD'S ROWS, AND
	// FOUR DO NOT: one row a command, its ref, since a fast-forward leaves no
	// object behind.
	forward := maximal
	forward.Commands = nil
	for _, c := range maximal.Commands {
		c.Forced = false
		forward.Commands = append(forward.Commands, c)
	}
	if rows := gitlet.RowBound(forward); rows != gitlet.MaxCommandsPerPush+2 {
		t.Errorf("a maximal push of fast-forward updates bounds %d rows, want MaxCommandsPerPush (%d) "+
			"+ its row + its pack", rows, gitlet.MaxCommandsPerPush)
	}
	if got := gitlet.RecordRows(3 * gitlet.RowBound(forward)); got > gitlet.MaxRecordRows {
		t.Errorf("three maximal pushes of fast-forward updates bound %d rows with the record's own "+
			"(RecordRows), past MaxRecordRows (%d)", got, gitlet.MaxRecordRows)
	}
	if got := gitlet.RecordRows(4 * gitlet.RowBound(forward)); got <= gitlet.MaxRecordRows {
		t.Errorf("four maximal pushes of fast-forward updates bound %d rows, within MaxRecordRows (%d)",
			got, gitlet.MaxRecordRows)
	}
	// A DELETION IS COUNTED THREE TIMES — its ref, the old tombstone the sweep
	// may take for the new one, and the object it left — so ONE maximal push
	// of deletions shares a record's rows with nothing of its size, and a
	// record's sweep always has a row for every tombstone its deletions make.
	deleting := maximal
	deleting.Commands = nil
	for _, c := range maximal.Commands {
		deleting.Commands = append(deleting.Commands, gitlet.Command{Ref: c.Ref, Old: c.Old, New: gitlet.ZeroOID})
	}
	if rows := gitlet.RowBound(deleting); rows != 3*gitlet.MaxCommandsPerPush+2 {
		t.Errorf("a maximal push of deletions bounds %d rows, want three a deletion (%d) + its row + "+
			"its pack", rows, 3*gitlet.MaxCommandsPerPush)
	}
	if got := gitlet.RecordRows(gitlet.RowBound(deleting)); got > gitlet.MaxRecordRows {
		t.Errorf("a maximal push of deletions bounds %d rows with the record's own, past "+
			"MaxRecordRows (%d)", got, gitlet.MaxRecordRows)
	}
	if got := gitlet.RecordRows(2 * gitlet.RowBound(deleting)); got <= gitlet.MaxRecordRows {
		t.Errorf("two maximal pushes of deletions bound %d rows, within MaxRecordRows (%d)", got,
			gitlet.MaxRecordRows)
	}
	// A FULL BATCH OF THE SMALLEST PUSHES FITS BOTH.
	if got := gitlet.RecordOverheadBytes + gitlet.MaxPushesPerRecord*small; got > gitlet.MaxRecordBytes {
		t.Errorf("MaxPushesPerRecord (%d) minimal entries are %d bytes, past MaxRecordBytes (%d)",
			gitlet.MaxPushesPerRecord, got, gitlet.MaxRecordBytes)
	}
	if got := gitlet.RecordRows(gitlet.MaxPushesPerRecord * gitlet.RowBound(minimal)); got > gitlet.MaxRecordRows {
		t.Errorf("MaxPushesPerRecord (%d) minimal pushes bound %d rows, past MaxRecordRows (%d)",
			gitlet.MaxPushesPerRecord, got, gitlet.MaxRecordRows)
	}
	// A SEAT'S PUSH IS COUNTED TWO ROWS MORE — its day's row and the older
	// day it forgets (code_seat_days) — and a full batch of the smallest
	// still fits; the bytes of the day its committer stamps are in its bound.
	// The smallest a seat makes is one the engine built, naming its turn
	// alone: a push git received from a seat is its coding box's, which names
	// its executor as well.
	seat := minimal
	seat.Principal = gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: strings.Repeat("s", 64),
		Run: gitlet.RunProvenance{TurnID: "0192f00d-0000-7000-8000-000000000001"}}
	seat.Via = gitlet.ViaEngine
	if got, want := gitlet.RowBound(seat), gitlet.RowBound(minimal)+2; got != want {
		t.Errorf("a seat's minimal push bounds %d rows, want a person's %d and its day's two", got, want)
	}
	if got := gitlet.RecordRows(gitlet.MaxPushesPerRecord * gitlet.RowBound(seat)); got > gitlet.MaxRecordRows {
		t.Errorf("MaxPushesPerRecord (%d) minimal seats' pushes bound %d rows, past MaxRecordRows (%d)",
			gitlet.MaxPushesPerRecord, got, gitlet.MaxRecordRows)
	}
	stamped, err := json.Marshal(gitlet.EntryOf(seat, gitlet.LongestDay))
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := gitlet.EntryBound(seat); err != nil || len(stamped)+gitlet.CommandResultBound+1 > bound {
		t.Errorf("a seat's entry stamped with its day takes %d bytes beside its decision, past its bound %d (%v)",
			len(stamped)+gitlet.CommandResultBound+1, bound, err)
	}

	// DECIDING GROWS A COMMAND BY AT MOST CommandResultBound, at the longest
	// code a record may carry.
	longest := gitlet.Refusal("")
	for _, r := range gitlet.Refusals() {
		if r.Recorded() && len(r) > len(longest) {
			longest = r
		}
	}
	undecided := gitlet.Command{Ref: "refs/heads/a", Old: gitlet.ZeroOID, New: oidOf("a")}
	base, _ := json.Marshal(undecided)
	for _, decided := range []gitlet.Command{
		{Ref: undecided.Ref, Old: undecided.Old, New: undecided.New, Result: gitlet.ResultNoop},
		{Ref: undecided.Ref, Old: undecided.Old, New: undecided.New, Result: gitlet.ResultNG, Reason: longest},
	} {
		grown, _ := json.Marshal(decided)
		if growth := len(grown) - len(base); growth > gitlet.CommandResultBound {
			t.Errorf("deciding a command %s %s grows it %d bytes, past CommandResultBound (%d)",
				decided.Result, decided.Reason, growth, gitlet.CommandResultBound)
		}
	}

	// A RECORD'S OWN BYTES FIT RecordOverheadBytes: a real encoded record of
	// one entry, with the longest node id, the widest generation and an
	// authored instant in a zone, less its entry.
	entry, err := json.Marshal(gitlet.EntryOf(minimal, ""))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(gitlet.PushBatch{RepoID: repo, Pushes: []gitlet.PushEntry{gitlet.EntryOf(minimal, "")}})
	if err != nil {
		t.Fatal(err)
	}
	record, err := gitlet.Encode(gitlet.MutationRecord{
		RecordEnvelope: gitlet.RecordEnvelope{
			OpID: statelog.NewOpID(at, "code_push"), Subject: gitlet.RepoSubject(repo), Op: gitlet.OpPush,
			CreatedAt: at, Gen: 1<<32 - 1, Writer: nodeID, Scope: gitlet.Scope{gitlet.RepoPath(repo)},
		},
		Expect: 1 << 63, Mutation: payload, Actor: nodeID, ActorKind: gitlet.ActorSystem,
	})
	if err != nil {
		t.Fatal(err)
	}
	if overhead := len(record) - len(entry); overhead > gitlet.RecordOverheadBytes {
		t.Errorf("a one-entry record carries %d bytes beside its entry, past RecordOverheadBytes (%d)",
			overhead, gitlet.RecordOverheadBytes)
	}

	// A QUEUE'S BYTES HOLD A FULL RECORD BEHIND ONE IN FLIGHT, and a queue
	// of the smallest pushes meets its count before its bytes.
	if gitlet.CommitterInboxBytes < 2*gitlet.MaxRecordBytes {
		t.Errorf("CommitterInboxBytes (%d) is under two records (MaxRecordBytes %d)",
			gitlet.CommitterInboxBytes, gitlet.MaxRecordBytes)
	}
	if got := gitlet.CommitterInbox * small; got > gitlet.CommitterInboxBytes {
		t.Errorf("CommitterInbox (%d) minimal pushes weigh %d bytes, past CommitterInboxBytes (%d), "+
			"so small pushes meet the byte bound rather than the count", gitlet.CommitterInbox, got,
			gitlet.CommitterInboxBytes)
	}
}

// THE LIMITS A PUSH IS HELD TO ARE CONFIG'S OWN READING OF code.limits — one
// resolution of the defaults, so `crewlet validate`'s order rules and the
// committer's decide can never disagree about what an unset field means — and
// so is the floor a merge is held to, code.merge_by_floor.
func TestLimitsOfIsConfigsReading(t *testing.T) {
	t.Parallel()
	if got := gitlet.LimitsOf(config.Code{}); got != gitlet.DefaultLimits() {
		t.Errorf("an unset code.limits is held to %+v, and the domain's defaults are %+v",
			got, gitlet.DefaultLimits())
	}
	resolved := config.CodeLimits{}.Resolved()
	if got := gitlet.LimitsOf(config.Code{}); got.MaxPushBytes != resolved.MaxPushBytes ||
		got.MaxBlobBytes != resolved.MaxBlobBytes || got.RepoMaxBytes != resolved.RepoMaxBytes ||
		got.PushesPerSeatDay != resolved.PushesPerSeatDay {
		t.Errorf("an unset code.limits is held to %+v, and config resolves it to %+v",
			got, resolved)
	}
	// EACH FIELD ON ITS OWN, so a default taken from the wrong field — the
	// file limit defaulting to the push limit — is seen.
	for name, tc := range map[string]struct {
		set  config.CodeLimits
		want gitlet.Limits
	}{
		"a push limit alone": {
			set: config.CodeLimits{MaxPushBytes: 512 << 20},
			want: gitlet.Limits{MaxPushBytes: 512 << 20,
				MaxBlobBytes: config.DefaultCodeMaxBlobBytes, RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
				PushesPerSeatDay: config.DefaultCodePushesPerSeatDay, MergeByFloor: gitlet.MergeByWriters},
		},
		"a file limit alone": {
			set: config.CodeLimits{MaxBlobBytes: 10 << 20},
			want: gitlet.Limits{MaxPushBytes: config.DefaultCodeMaxPushBytes,
				MaxBlobBytes: 10 << 20, RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
				PushesPerSeatDay: config.DefaultCodePushesPerSeatDay, MergeByFloor: gitlet.MergeByWriters},
		},
		"a repository limit alone": {
			set: config.CodeLimits{RepoMaxBytes: 4 << 30},
			want: gitlet.Limits{MaxPushBytes: config.DefaultCodeMaxPushBytes,
				MaxBlobBytes: config.DefaultCodeMaxBlobBytes, RepoMaxBytes: 4 << 30,
				PushesPerSeatDay: config.DefaultCodePushesPerSeatDay, MergeByFloor: gitlet.MergeByWriters},
		},
		"a seat's daily quota alone": {
			set: config.CodeLimits{PushesPerSeatDay: 40},
			want: gitlet.Limits{MaxPushBytes: config.DefaultCodeMaxPushBytes,
				MaxBlobBytes: config.DefaultCodeMaxBlobBytes, RepoMaxBytes: config.DefaultCodeRepoMaxBytes,
				PushesPerSeatDay: 40, MergeByFloor: gitlet.MergeByWriters},
		},
	} {
		if got := gitlet.LimitsOf(config.Code{Backend: config.CodeGitlet, Limits: tc.set}); got != tc.want {
			t.Errorf("%s: code.limits %+v is held to %+v, want %+v", name, tc.set, got, tc.want)
		}
	}
	// THE MERGE FLOOR, unset and each value set, beside limits nobody set:
	// config's own resolution, as a value the domain records.
	for _, floor := range append([]config.CodeMergeBy{""}, config.CodeMergeBys...) {
		got := gitlet.LimitsOf(config.Code{Backend: config.CodeGitlet, MergeByFloor: floor}).MergeByFloor
		if want := gitlet.MergeBy(floor.Resolved()); got != want || !got.Valid() {
			t.Errorf("code.merge_by_floor %q is held as %q, want %q, a value the domain records", floor, got,
				want)
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
		{1 << 30, week, 87_154},
		{16 << 30, week, 1_394_469},
		{1 << 30, time.Hour, 610_080},
		{1 << 30, 24 * time.Hour, 610_080},
		{1 << 30, 36 * time.Hour, 406_720},
		{1 << 30, 47 * time.Hour, 311_530},
		{math.MaxInt64, 24 * time.Hour, 5_240_552_293_667_486},
		{math.MaxInt64, week, 748_650_327_666_783},
		{0, week, 0},
	} {
		if got := gitlet.GitReadsPerDay(tc.ceiling, tc.minAge); got != tc.want {
			t.Errorf("GitReadsPerDay(%d, %v) = %d, want %d", tc.ceiling, tc.minAge, got, tc.want)
		}
	}
}
