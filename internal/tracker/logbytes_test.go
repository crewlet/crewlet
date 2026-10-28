package tracker_test

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The reference company's day, the corpus the storage forecast in
// docs/guides/retention.md is stated for: 100 000 tasks a year, 300 000
// comments, 1 000 edits a day and 3 000 turns a day — and the read barriers
// beside them, its 100 agent seats' linearizable reads at
// statelog.LinearizableReadsPerSeatDay and the object collector's 25.
const (
	referenceTasksADay    = 100_000.0 / 365
	referenceCommentsADay = 300_000.0 / 365
	referenceEditsADay    = 1_000.0
	referenceTurnsADay    = 3_000.0
	referenceSeatReads    = 100 * statelog.LinearizableReadsPerSeatDay
	referenceBarriersADay = referenceSeatReads + 25

	// referenceKeyGrowth is how many bytes longer a key is at a five-year-old
	// project of the reference company's than at the new one the corpus
	// files into — two digits to five — and referenceKeysADay how many times
	// a day one is written: three on a create (the counter's value, the
	// task's key, its snapshot's), once on a comment or an edit.
	referenceKeyGrowth = 3
	referenceKeysADay  = 3*referenceTasksADay + referenceCommentsADay + referenceEditsADay
)

// THE CODE LOG'S DAY, which config sizes the code log's ceiling against this
// log's by (DerivedCodeLogNumerator's reason, and code's CodeLogMaxBytes): at
// the ratio of the two days a blocked trim fills both logs in the same time,
// so that ratio is a figure derived from this log's published day. A push is
// a SEAT'S, as every one of the reference company's is — a hundred seats
// pushing thirty times a day — alone in its record: internal/code's measured
// figure for a person's push (BenchmarkPushLogBytes, its package doc's "What
// a push costs, measured") and what a seat's principal, its way and the
// company day its quota counted it on take of the log beyond it (its
// push=agent arm). The barriers are its git clients'
// linearizable reads: a seat's push asks the log three times (its
// advertisement, its receive's sync and the fetch before it), thirty people
// fetch every three minutes of an eight-hour day, and a CI polls ten
// repositories a minute where one runs.
const (
	referencePushesADay  = 3_000
	codePushRecordBytes  = 1_481
	referencePushReads   = 3 * referencePushesADay
	referencePeopleReads = 30 * 8 * 60 / 3
	referenceCIReads     = 10 * 60 * 24
)

// codeLogTerm is one term of the code log's day: the bytes a day one op's
// records take at the reference company, at their least (no CI) and their
// most.
type codeLogTerm struct{ least, most float64 }

// codeLogDay is the code log's day, by the op whose records each term is: its
// pushes, and its git clients' barriers at code.BarrierRecordBytes. What the
// day is summed from, so a term priced here is a term of every figure
// TestTheDocsPrintTheMeasuredDay holds.
func codeLogDay() map[code.OpKind]codeLogTerm {
	pushes := float64(referencePushesADay * codePushRecordBytes)
	git := float64((referencePushReads + referencePeopleReads) * code.BarrierRecordBytes)
	return map[code.OpKind]codeLogTerm{
		code.OpPush:    {least: pushes, most: pushes},
		code.OpBarrier: {least: git, most: git + float64(referenceCIReads*code.BarrierRecordBytes)},
	}
}

// codeLogNegligible is every op of the code domain whose records the code
// log's day leaves out, and why: each is appended at the rate a person edits
// the company's document or runs a command, or the trim sweeps what the log
// no longer holds — a few records a day beside 3 000 pushes and the tens of
// thousands of reads around them.
var codeLogNegligible = map[code.OpKind]string{
	code.OpCreate: "a repository is created when an activation of the company's document " +
		"declares it",
	code.OpSettings: "a repository's settings change when an activation of the company's " +
		"document changes them",
	code.OpDelete: "a repository is deleted when an activation of the company's document " +
		"drops it",
	code.OpRestore: "a deleted repository returns when an activation of the company's " +
		"document declares it again",
	code.OpPurgePart: "a deleted repository's rows go in bounded parts once it is past its " +
		"retention: once a repository",
	code.OpPurge:    "a deleted repository's last part: once a repository",
	code.OpActivate: "the activation fence: one record an activation, fleet-wide",
	code.OpEviction: "an operator's eviction of a node from the log, or its readmission",
	code.OpGeneration: "a reanchor's record, once an operator recreates or restores the " +
		"log",
	code.OpTrimmed: "the trim's sweep of the anchors under its floor, one record per " +
		"statelog.AnchorsPerTrimmedRecord, and the log keeps an anchor a repository and a " +
		"name, never one a push or a read",
	code.OpExpungeRequest: "a person's purge of a repository's unreachable objects: a command " +
		"a person runs, one record each, a later one widening the purge already open",
}

// codeLogUnwritten is every op of the code domain that no node of this build
// appends, with the package — or the file in one — whose code would append it
// and how: the day leaves the op out while that writer does not exist, and
// TestTheCodeLogsDayPricesEveryRecordItTakes fails the day it does.
var codeLogUnwritten = map[code.OpKind]struct{ writer, how string }{
	code.OpCompact: {"internal/code/compact", "a compaction is proposed by a " +
		"repository's compactor alone, through the estate's code.compact"},
	code.OpRetireSweep: {"internal/code/compact", "a sweep removes the pack rows a " +
		"compaction retired, and nothing is retired while nothing compacts"},
	// THE EXPUNGER'S OWN FILE, not its package: the compactor's package
	// arrives first, and an expunge's records are appended only by the
	// expunger beside it, whose begin can list a thousand packs — what the
	// day must price once they exist, which its writer is the one to
	// measure.
	code.OpExpungeBegin: {"internal/code/compact/expunge.go", "an expunge is begun by a " +
		"repository's maintainer alone, through the estate's code.expunge"},
	code.OpExpungeCommit: {"internal/code/compact/expunge.go", "an expunge is committed by " +
		"the maintainer that began it, through the estate's code.expunge"},
}

// codeLogKinds is every kind of object the code log's records are on, and how
// its day counts the records on it.
var codeLogKinds = map[code.ObjectKind]string{
	code.KindRepo: "a push is on its repository (codeLogDay); so are its settings, deletion, " +
		"restore, purge and a person's purge of its unreachable objects (codeLogNegligible), and " +
		"a compaction, its sweep and an expunge's begin and commit (codeLogUnwritten)",
	code.KindBarrier:    "a git client's read (codeLogDay)",
	code.KindName:       "a repository's create, on the name it claims (codeLogNegligible)",
	code.KindEviction:   "an operator's eviction or readmission (codeLogNegligible)",
	code.KindGeneration: "a reanchor (codeLogNegligible)",
	code.KindCompany:    "the activation fence (codeLogNegligible)",
	code.KindTrimmed:    "the trim's sweep of the anchors (codeLogNegligible)",
}

// The reference company's chart, as a seat's tools hold it. Its work is filed
// into ENG, which the chart gives to the Engineering unit — a unit with no
// `id`, as the shipped example's are, so its key is its name — and the unit's
// lead is the project's lead, because a project a unit declares is owned by
// that unit's lead ([org.Organization.LeadsBy]). Both are on every routing
// snapshot a seat's write takes, and the unit on every task it files.
const (
	referenceUnit = "Engineering"
	referenceLead = "eng-lead"
)

// referenceChart is [tracker.Leads] for that chart, the seam a seat's tools
// and the engine's writer each hold (engine's liveLeads).
type referenceChart struct{}

func (referenceChart) ProjectLead(string) string { return referenceLead }
func (referenceChart) UnitLead(string) string    { return referenceLead }

// THE FIGURES THE DOCS PRINT: what each gesture of the reference company's
// day costs the mutation log, in bytes, as the tracker's package doc ("What a
// day costs the log, measured") and docs/guides/retention.md's storage
// forecast print them, and as config's and code's arithmetic for the code
// log's share of the broker reads the day they make.
//
// The text the records carry is a STATED ASSUMPTION, not a measurement: how
// long a description or a comment is belongs to the company, and the
// measurement is of everything else a record carries — its ids, its envelope,
// the routing snapshot its notification takes, and what the broker stores
// beside it. Agents write at length, so the corpus takes a description of two
// kibibytes (about three hundred words), a comment of one, and a turn's
// summary at its MaxTurnSummary cap, which the engine rewrites a longer
// account to fit; and half that text is printed beside it, because the text
// is the term a company moves.
//
// THE KEYS ARE A NEW PROJECT'S, one or two digits, which is the one
// identifier here shorter than a company's: a project of the reference
// company's (100 000 tasks a year over 50 projects) reaches four digits in its
// first year and five by its fifth, three bytes more by then wherever a key is
// written — the counter's value, the task's key and its snapshot's on a
// create, the snapshot's on a comment or an edit — about 8 kB of the day, under
// a tenth of one percent, which the docs state rather than this measuring a
// counter ten thousand creates deep.
//
// Each is HELD to what the broker counts within publishedTolerance
// (TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay), and every page that
// prints one, or a figure derived from them, to these
// (TestTheDocsPrintTheMeasuredDay): a change to what a record carries fails
// naming the figure to restate rather than leaving the forecast behind its
// code.
const (
	publishedCreateBytes  = 5_266
	publishedCommentBytes = 3_037
	publishedEditBytes    = 1_157
	publishedTurnBytes    = 1_608

	// AT HALF THE TEXT, a 1 KiB description and a 512-byte comment. An edit
	// and a turn carry neither, so they cost what they cost at the whole.
	publishedHalfTextCreateBytes  = 4_242
	publishedHalfTextCommentBytes = 2_436

	// publishedTolerance is how far a measured gesture may sit from the
	// figure printed for it: a hundredth. The measurement is of
	// fixed-length ids and fixed text, so it repeats to the byte, and a
	// hundredth is the most a printed figure may be wrong by — a dozen
	// bytes added to every record already move an edit past it, which is a
	// change the figures are to be restated for.
	publishedTolerance = 0.01
)

// THE BARRIER FIGURES THE DOCS PRINT: what one linearizable read's barrier
// costs the mutation log at the log's first generation, whose envelope carries
// none; from its second generation to its tenth, which is more reanchors than
// a log meets in its life and the figure the forecast prices every barrier at
// (code.BarrierRecordBytes, which the pages say it costs "too"); and at the
// last a generation can be, ten digits a log never reaches. Each is a signed
// frame (ADR-0031) under this suite's two-byte key id, which every record
// carries (code.BarrierRecordBytes says what a longer one adds). Each is HELD to
// what the broker counts within publishedTolerance
// (TestABarrierCostsTheMutationLogWhatTheForecastPricesItAt), and the pages to
// these (TestTheDocsPrintTheMeasuredDay).
const (
	publishedFirstBarrierBytes = 168
	publishedBarrierBytes      = 176
	publishedLastBarrierBytes  = 185
)

// A corpus is the text the reference company's records carry, what each
// gesture costs the log at it, and the published figure each cost is.
type corpus struct {
	name                string
	description, remark int
	cost                gestures
	constant            [4]string
}

// The two corpora the docs print: the forecast's, and half its text.
var (
	referenceCorpus = corpus{
		name: "the reference text", description: 2 << 10, remark: 1 << 10,
		cost: gestures{
			create: publishedCreateBytes, comment: publishedCommentBytes,
			edit: publishedEditBytes, turn: publishedTurnBytes,
		},
		constant: [4]string{"publishedCreateBytes", "publishedCommentBytes",
			"publishedEditBytes", "publishedTurnBytes"},
	}
	halfTextCorpus = corpus{
		name: "half the text", description: 1 << 10, remark: 512,
		cost: gestures{
			create: publishedHalfTextCreateBytes, comment: publishedHalfTextCommentBytes,
			edit: publishedEditBytes, turn: publishedTurnBytes,
		},
		constant: [4]string{"publishedHalfTextCreateBytes", "publishedHalfTextCommentBytes",
			"publishedEditBytes", "publishedTurnBytes"},
	}
)

// gestures is what each gesture of the corpus's day costs the log, in bytes.
type gestures struct{ create, comment, edit, turn float64 }

// day is the bytes of records the reference company's day writes at these
// costs.
func (g gestures) day() float64 {
	return referenceTasksADay*g.create + referenceCommentsADay*g.comment +
		referenceEditsADay*g.edit + referenceTurnsADay*g.turn
}

// THE MUTATION LOG'S CEILING IS FIVE YEARS OF THE REFERENCE COMPANY'S RECORDS,
// MEASURED.
//
// tracker.TrackerLogMaxBytes says a completely blocked trim reaches it in years
// rather than months, and the storage forecast derives the log's healthy size
// from the same day: each claim is a rate of bytes a day times a number of
// days, and the rate was the ceiling's own five years divided back out until
// something measured it. So each gesture of the corpus's day is written here
// the way the engine writes it — a create through the project's counter into
// the unit the chart gave the project, a comment carrying the commenter's
// watch, a status change, a turn charged to its task — each with the
// notification a seat's tools attach, routed through the company's chart (a
// project's lead and its unit's on every snapshot), and the identifiers
// production mints, through the real writer onto a real broker, and the
// stream's own growth is what each costs.
//
// Each gesture must cost what the docs print for it within publishedTolerance,
// at both corpora. And the day the reference text makes must lie within a
// tenth of the rate the ceiling is sized at, either way: past it the ceiling
// fills sooner than its reason says and the forecast understates the log;
// under it the forecast overstates the log, and the code log's share of the
// broker — a ratio to this day (config.DerivedCodeLogNumerator) — is set too
// small.
func TestTheMutationLogsCeilingIsFiveYearsOfAMeasuredDay(t *testing.T) {
	t.Parallel()
	for _, c := range []corpus{referenceCorpus, halfTextCorpus} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			measured := measureADay(t, c)
			for _, g := range []struct {
				measured, printed  float64
				printedAs, printer string
			}{
				{measured.create, c.cost.create, "a task filed", c.constant[0]},
				{measured.comment, c.cost.comment, "a comment", c.constant[1]},
				{measured.edit, c.cost.edit, "an edit", c.constant[2]},
				{measured.turn, c.cost.turn, "a turn charged to its task", c.constant[3]},
			} {
				if math.Abs(g.measured-g.printed) > publishedTolerance*g.printed {
					t.Errorf("at %s, %s costs the mutation log %.0f bytes and the docs print %.0f "+
						"(%s): restate it, and every figure derived from it, where "+
						"TestTheDocsPrintTheMeasuredDay names them", c.name, g.printedAs,
						g.measured, g.printed, g.printer)
				}
			}
			day := measured.day()
			t.Logf("at %s the reference company's day takes %.2f MB of records", c.name, day/1e6)
			if c.name != referenceCorpus.name {
				return
			}
			sized := float64(tracker.TrackerLogMaxBytes) / (5 * 365)
			t.Logf("the ceiling is sized at %.2f MB a day", sized/1e6)
			if day < 0.9*sized || day > 1.1*sized {
				t.Errorf("the reference company's day takes %.2f MB of the mutation log (%.0f bytes a "+
					"create, %.0f a comment, %.0f an edit, %.0f a turn), and TrackerLogMaxBytes is "+
					"sized at five years of %.2f MB: the ceiling's reason, the storage forecast in "+
					"docs/guides/retention.md and the package doc's measured section all derive "+
					"from that day, so re-derive them from this one", day/1e6, measured.create,
					measured.comment, measured.edit, measured.turn, sized/1e6)
			}
		})
	}
}

// measureADay writes each gesture of the corpus's day onto a log of its own
// and answers what each cost, as the stream's own growth — what the broker
// counts against the ceiling — over forty of it.
func measureADay(t *testing.T, c corpus) gestures {
	t.Helper()
	r := newRoundTripWithoutProject(t)
	// THE CHART THE ENGINE APPLIES, so the project's row names the unit a
	// create is filed into, and THE CHART THE WRITER RESOLVES LEADS
	// THROUGH, as the engine's does.
	r.ledBy(referenceChart{})
	if _, err := r.writer.ApplyChart(t.Context(), activatedAt(100), []tracker.ChartProject{
		{Key: "ENG", Name: referenceUnit, Unit: referenceUnit},
	}); err != nil {
		t.Fatalf("apply the chart: %v", err)
	}
	r.drain()
	cost := func(what string, n int, gesture func(i int)) float64 {
		t.Helper()
		before, err := r.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the log: %v", err)
		}
		for i := range n {
			gesture(i)
		}
		after, err := r.log.Stats(t.Context())
		if err != nil {
			t.Fatalf("read the log: %v", err)
		}
		each := float64(after.Bytes-before.Bytes) / float64(n)
		t.Logf("%s: %-8s %.0f bytes a gesture, in %d records", c.name, what, each,
			(after.Messages-before.Messages)/uint64(n))
		return each
	}

	// A SEAT'S WRITER, as the seat's tools hold it: the actor is the seat,
	// and the record carries the turn and the delegation chain behind it.
	at := time.Date(2031, 4, 16, 14, 30, 0, 0, time.UTC)
	seat := r.writer.As("backend-dev", tracker.AuthorAgent, tracker.Provenance{
		TurnID: uuid.NewString(), Chain: []string{"cto", referenceLead}})
	chart := referenceChart{}
	const repeats = 40
	tasks := make([]string, repeats)
	read := func(id string) tracker.Task {
		t.Helper()
		detail, err := r.reader.Task(t.Context(), id, tracker.DetailWants{},
			statelog.Freshness{Level: statelog.ReadSession})
		if err != nil {
			t.Fatalf("read task %s: %v", id, err)
		}
		return detail.Task
	}

	description := text("A description an agent wrote of the work to do. ", c.description)
	var out gestures
	out.create = cost("create", repeats, func(i int) {
		// The task's id is a function of the operation, as the seat's
		// create derives it, and it names no unit: the ordinary create,
		// which the writer files into the project's own.
		op := statelog.DeriveOpID(at, "create-work_item", fmt.Sprint(i))
		task := newTask(uuid.NewSHA1(uuid.NameSpaceOID, []byte(op)).String())
		task.Key = ""
		task.Title = "Make the importer resume after a refused push to the node"
		task.Body = description
		task.Reporter, task.Assignee = referenceLead, "backend-dev"
		task.Watchers = []string{referenceLead, "backend-dev"}
		tasks[i] = task.ID
		if _, err := seat.CreateTask(t.Context(), op, task,
			tracker.Wake{Kind: tracker.ChangeCreated, After: task}.Notify(chart)); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		r.drain()
	})
	if filed := read(tasks[0]); filed.FiledUnit != referenceUnit || filed.RoutingUnit != referenceUnit {
		t.Fatalf("the premise: a create into ENG was filed into %q and routed to %q, want %s for "+
			"both, as the chart gives it", filed.FiledUnit, filed.RoutingUnit, referenceUnit)
	}

	body := text("A comment an agent left on the work item, saying what it found. ", c.remark)
	out.comment = cost("comment", repeats, func(i int) {
		before := read(tasks[i])
		c := &tracker.Comment{ID: uuid.NewString(), Task: tasks[i], Author: "backend-dev",
			AuthorKind: tracker.AuthorAgent, Body: body, CreatedAt: at}
		patch := tracker.TaskPatch{Comment: c,
			Watch: &tracker.WatchIntent{Handle: "backend-dev", Watch: true, Auto: true}}
		if _, err := seat.UpdateTask(t.Context(), statelog.DeriveOpID(at, "comment-"+c.ID),
			tasks[i], "ENG", tracker.NoIfMatch, patch, tracker.ChangeComment,
			tracker.Wake{Kind: tracker.ChangeComment, Before: before, After: before,
				Comment: c}.Notify(chart)); err != nil {
			t.Fatalf("comment %d: %v", i, err)
		}
		r.drain()
	})

	out.edit = cost("edit", repeats, func(i int) {
		before := read(tasks[i])
		status := tracker.StatusInProgress
		after := before
		after.Status = status
		if _, err := seat.UpdateTask(t.Context(), statelog.DeriveOpID(at, "update-"+tasks[i]),
			tasks[i], "ENG", tracker.NoIfMatch, tracker.TaskPatch{Status: &status},
			tracker.ChangeStatus, tracker.Wake{Kind: tracker.ChangeStatus, Before: before,
				After: after}.Notify(chart)); err != nil {
			t.Fatalf("edit %d: %v", i, err)
		}
		r.drain()
	})

	summary := text("Added the resume path to the importer, with a test of a refused step. ",
		tracker.MaxTurnSummary)
	out.turn = cost("turn", repeats, func(i int) {
		// THE ENGINE'S CHARGE, written as the seat with the turn it charges
		// and no chain, as the engine's own writer for it is built.
		run := uuid.NewString()
		charge := r.writer.As("backend-dev", tracker.AuthorAgent, tracker.Provenance{TurnID: run})
		if _, err := charge.RecordTurn(t.Context(), "turn/"+run+"/dispatch", tracker.TurnRecord{
			Task: tasks[i], Seat: "backend-dev", TurnID: run, Trigger: "work_item",
			Outcome: "done", Phases: []string{"execute", "review"},
			Spend: tracker.TurnSpend{Turns: 1, Rounds: 9, Input: 48_000, Output: 3_200,
				CacheRead: 210_000, CacheWrite: 12_000, WallMs: 95_000},
			Summary: summary,
			Tools: []tracker.TurnTool{{Name: "get_work_item", Calls: 2},
				{Name: "run_sandbox", Calls: 1}, {Name: "search_knowledge", Calls: 2},
				{Name: "comment_on_work_item", Calls: 1}},
		}); err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		r.drain()
	})
	return out
}

// A BARRIER COSTS THE MUTATION LOG WHAT THE FORECAST PRICES IT AT.
//
// The storage forecast and config's arithmetic for the code log's share price
// a linearizable read's barrier on this log at code.BarrierRecordBytes, the
// code log's measured figure — a barrier here names a longer subject and
// carries another scope, so it is measured on its own stream at each
// generation the docs print a figure for: the first, whose envelope carries
// none; the second and the tenth, the two ends of the run of one-digit
// generations, which is more reanchors than a log meets in its life; and the
// last a generation can be, whose ten digits grow it by a byte each.
//
// Each must cost what the docs print for it within publishedTolerance, and
// every generation a log meets no more than the forecast prices it at: past
// that, the forecast understates the barriers and every figure derived from
// them, and the census sized from code.BarrierRecordBytes allows more of them
// than the log has room for.
func TestABarrierCostsTheMutationLogWhatTheForecastPricesItAt(t *testing.T) {
	t.Parallel()
	for _, arm := range []struct {
		name       string
		generation uint32
		printed    float64
		printer    string
		met        bool
	}{
		{"the first generation", 0, publishedFirstBarrierBytes, "publishedFirstBarrierBytes", true},
		{"the second generation", 1, publishedBarrierBytes, "publishedBarrierBytes", true},
		{"the tenth generation", 9, publishedBarrierBytes, "publishedBarrierBytes", true},
		{"the last generation", math.MaxUint32, publishedLastBarrierBytes, "publishedLastBarrierBytes", false},
	} {
		t.Run(arm.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			// SIGNED AS EVERY BARRIER A NODE APPENDS IS, since the frame is
			// part of what the log stores and so of what a barrier costs it.
			index, err := statelog.NewReadIndex(tracker.Domain{}, tracker.Domain{}.Stream(),
				r.log, testSigner(t, tracker.Domain{}), r.reserve, tracker.EncodeBarrier,
				func() uint32 { return arm.generation }, nil)
			if err != nil {
				t.Fatalf("build the read index: %v", err)
			}
			before, err := r.log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			const barriers = 500
			for range barriers {
				if _, err := index.Read(t.Context()); err != nil {
					t.Fatalf("a linearizable read's barrier: %v", err)
				}
			}
			after, err := r.log.Stats(t.Context())
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			if got := after.Messages - before.Messages; got != barriers {
				t.Fatalf("the premise: %d sequential reads appended %d barriers, want one each",
					barriers, got)
			}
			each := float64(after.Bytes-before.Bytes) / barriers
			t.Logf("at %s a barrier grew the mutation log by %.1f bytes", arm.name, each)
			if math.Abs(each-arm.printed) > publishedTolerance*arm.printed {
				t.Errorf("at %s a barrier costs the mutation log %.1f bytes and the docs print "+
					"%.0f (%s): restate it, and every figure derived from it, where "+
					"TestTheDocsPrintTheMeasuredDay names them", arm.name, each, arm.printed,
					arm.printer)
			}
			if arm.met && each > code.BarrierRecordBytes {
				t.Errorf("at %s a barrier costs the mutation log %.1f bytes, and the forecast "+
					"prices every barrier a log meets at %d (code.BarrierRecordBytes): the "+
					"figures derived from it understate this log", arm.name, each,
					code.BarrierRecordBytes)
			}
		})
	}
}

// THE DOCS PRINT THE MEASURED DAY: every page that states what a gesture costs
// the mutation log, or a figure derived from the day those costs make, states
// it from the published figures — so a figure restated for a change in what a
// record carries fails here until every page says it, rather than one page
// moving and four keeping the old day.
//
// The pages are the storage forecast in docs/guides/retention.md, this
// package's doc ("What a day costs the log, measured") and TrackerLogMaxBytes's
// reason, and the reasons that size the code log's share of the broker against
// this day (config's DerivedCodeLogNumerator and its prose in
// docs/guides/replication.md, code's CodeLogMaxBytes and GitReadsShare), the
// count of barriers a day that is the reference company's line for a code log
// at half this log's ceiling among them. Each figure is computed here from the
// published costs, the corpus's rates, the published barriers, the code log's
// day (codeLogDay, every term of it), code.BarrierRecordBytes and
// config.DefaultMinAge, and rounded as the pages round it; the few the pages
// put in words are held to the range the words are true over.
//
// Mutation: change any published figure, a corpus's rate or text,
// MaxTurnSummary, a term of the code log's day, code.BarrierRecordBytes or
// config.DefaultMinAge, and a page no longer says what this package measures.
func TestTheDocsPrintTheMeasuredDay(t *testing.T) {
	t.Parallel()
	whole, half := referenceCorpus.cost, halfTextCorpus.cost
	records, halfRecords := whole.day(), half.day()
	barriers := float64(referenceBarriersADay * code.BarrierRecordBytes)
	seatBarriersAYear := float64(referenceSeatReads*code.BarrierRecordBytes) * 365
	day := records + barriers
	window := config.DefaultMinAge.Hours() / 24
	ceiling := float64(tracker.TrackerLogMaxBytes)
	years := func(capacity, rate float64) float64 { return capacity / rate / 365 }
	keyBytes := referenceKeyGrowth * referenceKeysADay

	// THE CODE LOG'S DAY BESIDE THIS ONE, at its least (no CI) and its most:
	// every term it prices, summed.
	terms := codeLogDay()
	pushes := terms[code.OpPush].most
	gitLeast, gitMost := terms[code.OpBarrier].least, terms[code.OpBarrier].most
	var codeLeastBytes, codeMostBytes, besideBarriers float64
	for op, term := range terms {
		codeLeastBytes += term.least
		codeMostBytes += term.most
		if op != code.OpBarrier {
			besideBarriers += term.most
		}
	}
	codeLeast, codeMost := codeLeastBytes/day, codeMostBytes/day
	share := float64(config.DerivedCodeLogNumerator) / config.DerivedCodeLogDenominator
	// THE BARRIERS UNDER WHICH HALF SUFFICES, for a code log that takes them
	// beside its other terms at their most: the count a day that makes the
	// code log's day half this one's, so a code log at half the mutation log's
	// ceiling fills no sooner under a blocked trim. The line itself is the two
	// logs' bytes a day; this is the reference company's count for it, which
	// the pages print to the nearest hundred, beside the reads the reference
	// company's git clients make with no CI at all.
	halfBarriers := day/2 - besideBarriers
	halfCount := halfBarriers / code.BarrierRecordBytes
	leastReads := int64(referencePushReads + referencePeopleReads)
	// A TENTH OF THE CODE LOG'S CEILING is what its census lets git's barriers
	// hold over min_age ([code.GitReadsShare]).
	tenth := func(ceiling float64) float64 { return ceiling / code.GitReadsShare }

	// THE BARRIER THE DAY IS PRICED AT IS THE ONE THIS LOG MEASURES: the pages
	// print one figure for both, saying a mutation-log barrier costs
	// code.BarrierRecordBytes "too".
	if publishedBarrierBytes != code.BarrierRecordBytes {
		t.Errorf("the docs price this log's barriers at code.BarrierRecordBytes, %d bytes, and "+
			"say a barrier here costs that too, but one costs %d (publishedBarrierBytes): "+
			"price the day at what this log measures, or reword", code.BarrierRecordBytes,
			publishedBarrierBytes)
	}

	// THE WORDS, each held to the range it is true over.
	for _, w := range []struct {
		words    string
		value    float64
		low, top float64
	}{
		{"nearly four years of the whole day", years(ceiling, day), 3.5, 4},
		{"nearly five years of the records", years(ceiling, records), 4.5, 5},
		{"a log created at 4 GiB fills in about a year", years(4<<30, day), 0.9, 1.2},
		{"at a three-year-old company the ceiling is under a year away", years(ceiling, day) - 3, 0.75, 1},
		{"half of the records are the turns", referenceTurnsADay * whole.turn / records, 0.45, 0.55},
		{"the keys a five-year-old project writes are under a tenth of one percent of the day",
			keyBytes / records, 0, 0.001},
		{"the code log takes a little over half of this log's day at its least", codeLeast, 0.5, 0.6},
		{"and about three quarters at its most", codeMost, 0.72, 0.8},
		{"two thirds is the middle of that range, never past its top",
			(share - codeLeast) / (codeMost - codeLeast), 0.45, 0.55},
		{"half suffices under fewer barriers than the reference company's git clients " +
			"append with no CI, and under some", halfCount / float64(leastReads), 0, 1},
		{"git's barriers over min_age are under a thirtieth of the tenth of the derived ceiling",
			window * gitMost / tenth(code.CodeLogMaxBytes), 0, 1.0 / 30},
		{"and about a third of the tenth of the 1 GiB floor",
			window * gitMost / tenth(float64(config.CodeLogMaxBytesFloor)), 0.3, 0.36},
	} {
		if w.value < w.low || w.value > w.top {
			t.Errorf("the docs say %s, and the published day makes it %.2f: reword it where "+
				"this test holds it", w.words, w.value)
		}
	}

	mb := func(bytes float64, places int) string { return fmt.Sprintf("%.*f", places, bytes/1e6) }
	count := func(rate float64) string { return spaced(int64(math.Round(rate))) }
	hundreds := func(rate float64) string { return spaced(int64(math.Round(rate/100) * 100)) }
	kib := func(n int) string { return fmt.Sprintf("%d KiB", n>>10) }
	tasks, comments := referenceTasksADay*whole.create, referenceCommentsADay*whole.comment
	edits, turns := referenceEditsADay*whole.edit, referenceTurnsADay*whole.turn
	collector := referenceBarriersADay - referenceSeatReads
	// THE BARRIERS' YEAR to the nearest ten megabytes, as the guide rounds a
	// figure it calls "about".
	barrierYear := math.Round(seatBarriersAYear/1e7) * 10
	// THE CODE LOG'S GIT CENSUS at its floor and the default min_age, to the
	// nearest thousand, as the guide rounds it: the term it prices at
	// code.BarrierRecordBytes, so a barrier restated moves the census the
	// guide prints.
	gitCensus := spaced(int64(math.Round(float64(code.GitReadsPerDay(config.CodeLogMaxBytesFloor,
		config.DefaultMinAge))/1e3) * 1e3))

	for file, wants := range map[string][]string{
		"docs/guides/retention.md": {
			fmt.Sprintf("holds **about %s MB**", mb(window*day, 0)),
			fmt.Sprintf("seven days, `min_age`'s default, of the %s MB a day the reference company "+
				"writes to it", mb(day, 1)),
			fmt.Sprintf("The day is %s MB of records and %s MB of read barriers", mb(records, 1),
				mb(barriers, 1)),
			fmt.Sprintf("| %s tasks filed, at a %s description | %s bytes, in two records | %s MB |",
				count(referenceTasksADay), kib(referenceCorpus.description),
				spaced(int64(whole.create)), mb(tasks, 2)),
			fmt.Sprintf("| %s comments, at %s | %s bytes | %s MB |", count(referenceCommentsADay),
				kib(referenceCorpus.remark), spaced(int64(whole.comment)), mb(comments, 2)),
			fmt.Sprintf("| %s edits, each a status change | %s bytes | %s MB |",
				count(referenceEditsADay), spaced(int64(whole.edit)), mb(edits, 2)),
			fmt.Sprintf("| %s turns charged to a task, a summary at its %d-byte cap | %s bytes | %s MB |",
				count(referenceTurnsADay), tracker.MaxTurnSummary, spaced(int64(whole.turn)),
				mb(turns, 2)),
			fmt.Sprintf("| %s read barriers: the seats' %s and the object collector's %d | %d bytes | %s MB |",
				spaced(referenceBarriersADay), spaced(referenceSeatReads), collector,
				code.BarrierRecordBytes, mb(barriers, 2)),
			fmt.Sprintf("with half of it — a %s description, a %d-byte comment — the records come to %s MB",
				kib(halfTextCorpus.description), halfTextCorpus.remark, mb(halfRecords, 1)),
			fmt.Sprintf("about %.0f kB of the day, under a tenth of one percent", keyBytes/1e3),
			fmt.Sprintf("The read barriers add about **%.0f MB a year** to the log's throughput — %s a "+
				"day at %d bytes, %.0f %% of what the log takes", barrierYear,
				spaced(referenceSeatReads), code.BarrierRecordBytes, 100*barriers/day),
			fmt.Sprintf("at %d bytes a barrier — the code log's `code.BarrierRecordBytes`, which a "+
				"mutation-log barrier costs too from the log's second generation to its tenth (%d "+
				"bytes at its first)", publishedBarrierBytes, publishedFirstBarrierBytes),
			fmt.Sprintf("On a fleet that trims they are about %s MB of the %s.", mb(window*barriers, 0),
				mb(window*day, 0)),
			fmt.Sprintf("the log keeps the whole day, about %.1f GB a year, so a log created at 16 GiB "+
				"fills in nearly four years — nearly five on its records alone — and one "+
				"created at 4 GiB in about a year", day*365/1e9),
			fmt.Sprintf("at %d bytes a barrier in its signed frame (about %s a day at the 1 GiB floor "+
				"and the default seven days)", code.BarrierRecordBytes, gitCensus),
		},
		"internal/tracker/subject.go": {
			fmt.Sprintf("a task filed: %s bytes, in two records", spaced(int64(whole.create))),
			fmt.Sprintf("at a %s description", kib(referenceCorpus.description)),
			fmt.Sprintf("a comment: %s bytes at %s", spaced(int64(whole.comment)),
				kib(referenceCorpus.remark)),
			fmt.Sprintf("an edit: %s bytes for a status change", spaced(int64(whole.edit))),
			fmt.Sprintf("a turn charged to its task: %s bytes with its summary at [MaxTurnSummary]",
				spaced(int64(whole.turn))),
			fmt.Sprintf("So the day is %s MB of records — %s MB of tasks, %s of comments, %s of edits "+
				"and %s of turns: half of it is the turns", mb(records, 1), mb(tasks, 2),
				mb(comments, 2), mb(edits, 2), mb(turns, 2)),
			fmt.Sprintf("(a %s description and a %d-byte comment make it %s MB)",
				kib(halfTextCorpus.description), halfTextCorpus.remark, mb(halfRecords, 1)),
			fmt.Sprintf("written, about %.0f kB of the day.", keyBytes/1e3),
			fmt.Sprintf("the seats' %s linearizable reads and the object collector's %d append %s MB of "+
				"barriers, at %d bytes each from a log's second generation to its tenth (%d at its "+
				"first;", spaced(referenceSeatReads), collector, mb(barriers, 1),
				publishedBarrierBytes, publishedFirstBarrierBytes),
			fmt.Sprintf("%s MB a day in all, which the trim's seven-day min_age holds about %s MB of",
				mb(day, 1), mb(window*day, 0)),
			"[TrackerLogMaxBytes] is nearly five years of the records, and nearly four " +
				"years of the whole day, barriers included.",
		},
		"internal/tracker/domain.go": {
			fmt.Sprintf("its day takes %s MB of this log", mb(records, 1)),
			fmt.Sprintf("which fills it in %.1f years", years(ceiling, records)),
			fmt.Sprintf("Its seats' linearizable reads add %s MB a day of barriers, so a completely "+
				"blocked trim reaches it in nearly four years", mb(barriers, 1)),
			"at a three-year-old company whose trim never ran the ceiling is under a year away",
		},
		"internal/config/retention.go": {
			fmt.Sprintf("the mutation log about %s MB a day: %s MB of records", mb(day, 1),
				mb(records, 1)),
			fmt.Sprintf("and %s MB of barriers, its seats' %s reads and the object collector's %d",
				mb(barriers, 1), spaced(referenceSeatReads), collector),
			fmt.Sprintf("the code log %s–%s MB a day: %s pushes at %s bytes",
				mb(codeLeastBytes, 1), mb(codeMostBytes, 1), spaced(referencePushesADay),
				spaced(codePushRecordBytes)),
			fmt.Sprintf("%s MB; and the barriers of its git clients, %s–%s MB", mb(pushes, 1),
				mb(gitLeast, 1), mb(gitMost, 1)),
			fmt.Sprintf("so a seat's push three times, %s a day, beside thirty people fetching every "+
				"three minutes of a working day (%s) and, where one runs, a CI polling ten "+
				"repositories a minute (%s)", spaced(referencePushReads),
				spaced(referencePeopleReads), spaced(referenceCIReads)),
			fmt.Sprintf("So the code log takes %.2f–%.2f of the mutation log's day — %.2f on its push "+
				"records alone (%.2f of the tracker's records)", codeLeast, codeMost, pushes/day,
				pushes/records),
			fmt.Sprintf("beside the reference company's %s MB of pushes, ABOUT %s BARRIERS A DAY, %s MB, "+
				"make half the mutation log's %s MB day", mb(pushes, 1), hundreds(halfCount),
				mb(halfBarriers, 1), mb(day, 1)),
			fmt.Sprintf("with no CI at all (%s)", spaced(leastReads)),
		},
		"internal/code/limits.go": {
			fmt.Sprintf("against the tracker's measured %s MB of records and %s MB of barriers",
				mb(records, 1), mb(barriers, 1)),
			fmt.Sprintf("%s pushes at the MEASURED %s bytes a seat's push alone in its record "+
				"(BenchmarkPushLogBytes), %s MB, and its git clients' barriers at "+
				"BarrierRecordBytes, %s–%s MB", spaced(referencePushesADay),
				spaced(codePushRecordBytes), mb(pushes, 1), mb(gitLeast, 1), mb(gitMost, 1)),
			fmt.Sprintf("git barriers beside its pushes: %s–%s MB a day, %.0f–%.0f MB over the default "+
				"seven days — under a thirtieth of the tenth of a derived 10⅔ GiB ceiling, and "+
				"about a third of the tenth of even the 1 GiB floor", mb(gitLeast, 1), mb(gitMost, 1),
				window*gitLeast/1e6, window*gitMost/1e6),
		},
		"docs/guides/replication.md": {
			fmt.Sprintf("a company's push records (%s bytes a seat's push, measured) and the barriers "+
				"its git clients' reads append (%d bytes a read) take a little over half to about three "+
				"quarters of what its tracker writes and its seats' reads take a day, and the ratio is "+
				"taken at the middle of that range", spaced(codePushRecordBytes), code.BarrierRecordBytes),
			fmt.Sprintf("Beside the reference company's %s pushes a day, that is **about %s barriers a "+
				"day**: its pushes' %s MB — every one a seat's, %s bytes alone in its record — and "+
				"those barriers' %s MB are half the %s MB a day its tracker writes and its seats' reads "+
				"take", spaced(referencePushesADay), hundreds(halfCount), mb(pushes, 1),
				spaced(codePushRecordBytes), mb(halfBarriers, 1), mb(day, 1)),
			fmt.Sprintf("with no CI at all — %s, three reads a push and thirty people fetching every "+
				"three minutes of a working day", spaced(leastReads)),
		},
	} {
		page := prose(t, file)
		for _, want := range wants {
			if !strings.Contains(page, want) {
				t.Errorf("%s does not say %q", file, want)
			}
		}
	}
}

// THE CODE LOG'S DAY PRICES EVERY RECORD THE LOG TAKES, or says why one is not
// in it.
//
// The day sizes the code log's share (config's DerivedCodeLogNumerator), and
// the pages count, beside it, the barriers under which a code log at half the
// mutation log's ceiling suffices. A kind of record the log takes and the day
// leaves out makes both quietly short, and no figure
// TestTheDocsPrintTheMeasuredDay reads moves when it joins the log: a
// compaction's, a change's or a check's record would come on top of a day
// that has no room for it, and a company that lowered its code log to a half
// on the barrier count would fill it first.
//
// So every op the code domain declares is exactly one of three: a term of the
// day (codeLogDay), negligible beside it for the reason given
// (codeLogNegligible), or appended by no node of this build (codeLogUnwritten)
// — and that last only while the package that would append it does not exist.
// Every kind of object the log's records are on says how the day counts them
// (codeLogKinds). A new op, a new kind, or the package that writes an
// unwritten op fails here until its records are priced into the day — and the
// share and when a half suffices restated on the pages
// TestTheDocsPrintTheMeasuredDay holds — or argued negligible.
//
// Mutation: declare an op or a kind in the code domain, or make
// internal/code/compact, and this fails naming it.
func TestTheCodeLogsDayPricesEveryRecordItTakes(t *testing.T) {
	t.Parallel()
	const remedy = "price its records into codeLogDay — and restate the code log's share and " +
		"when a half suffices on the pages TestTheDocsPrintTheMeasuredDay holds — or say in " +
		"codeLogNegligible why they are no rate beside the day"
	day := codeLogDay()
	for _, op := range code.OpKinds {
		var in []string
		if _, found := day[op]; found {
			in = append(in, "codeLogDay")
		}
		if _, found := codeLogNegligible[op]; found {
			in = append(in, "codeLogNegligible")
		}
		if _, found := codeLogUnwritten[op]; found {
			in = append(in, "codeLogUnwritten")
		}
		switch len(in) {
		case 0:
			t.Errorf("the code log takes %q records, which its day neither prices nor leaves out "+
				"for a reason: %s", op, remedy)
		case 1:
		default:
			t.Errorf("%q is in %s: an op is priced, negligible or unwritten, one of the three",
				op, strings.Join(in, " and "))
		}
	}
	for _, listed := range []struct {
		name string
		ops  []code.OpKind
	}{
		{"codeLogDay", slices.Collect(maps.Keys(day))},
		{"codeLogNegligible", slices.Collect(maps.Keys(codeLogNegligible))},
		{"codeLogUnwritten", slices.Collect(maps.Keys(codeLogUnwritten))},
	} {
		for _, op := range listed.ops {
			if !op.Valid() {
				t.Errorf("%s names %q, which is no op the code domain declares", listed.name, op)
			}
		}
	}
	for op, why := range codeLogNegligible {
		if strings.TrimSpace(why) == "" {
			t.Errorf("codeLogNegligible leaves %q out of the day and says nothing of why", op)
		}
	}
	// AN UNWRITTEN OP IS UNWRITTEN WHILE ITS WRITER DOES NOT EXIST.
	for op, unwritten := range codeLogUnwritten {
		_, err := os.Stat(filepath.Join(sourcetree.Root(t), filepath.FromSlash(unwritten.writer)))
		switch {
		case err == nil:
			t.Errorf("%s exists, so a node appends %q records — %s: %s", unwritten.writer, op,
				unwritten.how, remedy)
		case !errors.Is(err, fs.ErrNotExist):
			t.Fatalf("whether %s exists, which would append %q records: %v", unwritten.writer, op, err)
		}
	}
	for _, kind := range code.ObjectKinds {
		if strings.TrimSpace(codeLogKinds[kind]) == "" {
			t.Errorf("the code log's records are on %q objects, and codeLogKinds does not say how "+
				"its day counts them: %s", kind, remedy)
		}
	}
	for kind := range codeLogKinds {
		if !kind.Valid() {
			t.Errorf("codeLogKinds names %q, which is no kind of object the code domain declares", kind)
		}
	}
}

// prose is a file of this tree as a reader reads its words: a comment's
// markers taken off each line and every run of white space one space, so a
// sentence is found however a page or a comment wraps it.
func prose(t *testing.T, file string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		lines[i] = strings.TrimPrefix(strings.TrimSpace(line), "//")
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}

// spaced is n as the docs write a number: its thousands set apart by a space
// (1 568, 12 525).
func spaced(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}

// text is n bytes of sentence repeated, cut at n.
func text(sentence string, n int) string {
	return strings.Repeat(sentence, n/len(sentence)+1)[:n]
}
