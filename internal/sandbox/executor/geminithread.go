package executor

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A Gemini CLI take's receipt: the take script records each take it handed
// Gemini CLI, its token and its rendered text's digest ([ItemDigest]), in
// [Paths.GeminiTakes], since the token rides the hook's answer in a field
// Gemini CLI drops ([geminiTakeField]); the run's main session then says which
// it delivered ([geminiConfirmed]).

var _ mainThreadHarness = GeminiCLI{}

// threadTraits is what a Gemini CLI run's main-thread read settles: its hooks'
// receipts, read off the session's own words, and its copy's count, a copy
// its harness was cut off in uncounted.
func (GeminiCLI) threadTraits() sandbox.ThreadTraits {
	return sandbox.ThreadTraits{Reads: true, Receipts: true, Summaries: true}
}

// geminiRecordedTake is one line of [Paths.GeminiTakes]: a take the script
// handed Gemini CLI, or the record's start line, From, the count of the
// session's lines as a run started ([geminiScript]), which every take after
// it is confirmed past.
type geminiRecordedTake struct {
	Take   string `json:"take"`
	Digest string `json:"digest"`
	From   *int   `json:"from"`
}

// readMainThread counts the copy of ask's session the box holds where ask
// asks ([chatCopyCount]), the one chat file naming it, reading its newest
// summary in the same walk ([geminiSummaries]), and answers the receipts of
// the takes it asks ([GeminiCLI.receipts]).
func (g GeminiCLI) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	var summaries *geminiSummaries
	if ask.Summary {
		if why := summaryReleaseWhy(ask.Release, geminiRelease); why != "" {
			reading.Summary = summaryUnread(why)
		} else {
			summaries = newGeminiSummaries()
		}
	}
	if ask.Count || summaries != nil {
		var file string
		if names, err := geminiSessionFiles(ctx, box, paths, ask.Session); err == nil && len(names) == 1 {
			file = paths.GeminiChats() + "/" + names[0]
		}
		var counter chatCounter
		if ask.Count {
			counter = newGeminiCounting()
		}
		var also func([]byte)
		var lines *geminiSummaryReading
		if summaries != nil {
			lines = newGeminiSummaryReading(summaries)
			also = func(raw []byte) { lines.take(ctx, raw) }
		}
		count, err := chatCopyCount(ctx, box, file, counter, ask.Release, geminiRelease, also)
		reading.Count = count
		if summaries != nil {
			if err = cmp.Or(err, lines.err); err != nil {
				summaries.read.fail("the session's chat file was not read to its end: " + err.Error())
			}
			reading.Summary = summaries.reading(ask)
		}
	}
	if len(ask.Takes) > 0 {
		reading.Receipts = g.receipts(ctx, box, paths, ask)
	}
	return reading
}

// geminiSummaries is one walk's replay of a Gemini CLI chat file for its newest
// summary, each record as the transcript reads it ([geminiSummaryReading]):
// each record by its id, in the order first written; a `$patch`'s updates as
// copies of their records and its removals gone; a `$rewindTo`'s record and
// every later one gone. The newest is the latest snapshot standing at the
// file's end, so one a patch took away is never it. Its words are held to
// [summaryReadMaxBytes] in all, the earliest let go first. A line the replay
// cannot follow (unknown) leaves every snapshot before it unsaid.
type geminiSummaries struct {
	place map[string]int
	next  int
	snaps []geminiSnap
	held  int
	// first is the place of the first snapshot read, unknown where the
	// replay could not follow the file and why, -1 for neither.
	first, unknown int
	why            string
	read           summaryRead
}

// geminiSnap is a standing record that is a snapshot: its id, place and
// words, dropped where the bound let them go.
type geminiSnap struct {
	id      string
	place   int
	text    string
	dropped bool
}

func newGeminiSummaries() *geminiSummaries {
	return &geminiSummaries{place: map[string]int{}, first: -1, unknown: -1}
}

// lost says the replay cannot follow the file from here.
func (g *geminiSummaries) lost(why string) { g.unknown, g.why = g.next, why }

// record reads one copy of a record ([geminiSummaryReading]): its latest copy
// is the record.
func (g *geminiSummaries) record(rec *geminiSnapRecord) {
	if rec.id == "" {
		return
	}
	at, seen := g.place[rec.id]
	if !seen {
		at = g.next
		g.place[rec.id] = at
		g.next++
	}
	g.drop(func(s geminiSnap) bool { return s.id == rec.id })
	if rec.typ != "user" || !rec.snap.set {
		return
	}
	if g.first < 0 {
		g.first = at
	}
	snap := geminiSnap{id: rec.id, place: at, dropped: rec.snap.past}
	if !snap.dropped {
		snap.text = string(rec.snap.buf)
	}
	g.snaps = append(g.snaps, snap)
	g.held += len(snap.text)
	for i := 0; g.held > summaryReadMaxBytes && i < len(g.snaps); i++ {
		if s := &g.snaps[i]; !s.dropped {
			g.held -= len(s.text)
			s.text, s.dropped = "", true
		}
	}
}

// remove is a patch's removal of the record id.
func (g *geminiSummaries) remove(id string) {
	g.drop(func(s geminiSnap) bool { return s.id == id })
	delete(g.place, id)
}

// rewindTo is a `$rewindTo`: its record and every later one gone.
func (g *geminiSummaries) rewindTo(id string) {
	at, ok := g.place[id]
	if !ok {
		g.lost("the chat file rewinds to a record its history does not hold")
		return
	}
	g.drop(func(s geminiSnap) bool { return s.place >= at })
	for id, p := range g.place {
		if p >= at {
			delete(g.place, id)
		}
	}
}

// drop lets each snapshot gone go.
func (g *geminiSummaries) drop(gone func(geminiSnap) bool) {
	g.snaps = slices.DeleteFunc(g.snaps, func(s geminiSnap) bool {
		if gone(s) {
			g.held -= len(s.text)
			return true
		}
		return false
	})
}

// reading is the newest snapshot standing, as ask keeps it.
func (g *geminiSummaries) reading(ask sandbox.MainThreadAsk) sandbox.ThreadSummary {
	newest := geminiSnap{place: -1}
	if len(g.snaps) > 0 {
		newest = slices.MaxFunc(g.snaps, func(a, b geminiSnap) int { return a.place - b.place })
	}
	switch {
	case g.unknown >= 0 && newest.place < g.unknown && g.first >= 0 && g.first < g.unknown:
		g.read.unshaped(g.why)
	case newest.place < 0:
	case newest.dropped:
		g.read.unshaped(errSummaryPast.Error())
	default:
		g.read.take(newest.text)
	}
	return g.read.reading(ask)
}

// receipts answers the receipts of the takes ask asks: held where the main
// session shows the take's text delivered in order; absent where the job is
// over and it does not, its whole thread read; undecided otherwise. A take
// the script never recorded was never handed over.
func (GeminiCLI) receipts(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.TakeReceipts {

	undecided := func(why string) sandbox.TakeReceipts { return sandbox.TakeReceipts{Why: why} }
	if !sandbox.ValidSessionID(ask.Session) {
		return undecided(fmt.Sprintf("the run's Gemini CLI session %q is no session id its record is named by", ask.Session))
	}
	raw, err := box.ReadFile(ctx, paths.GeminiTakes(ask.Session))
	if err != nil {
		return undecided("the run's record of its takes could not be read: " + err.Error())
	}
	var recorded []geminiRecordedTake
	var asked []geminiAskedTake
	from := 0
	started := false
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		var t geminiRecordedTake
		switch {
		case json.Unmarshal(line, &t) != nil:
		case t.From != nil && *t.From >= 0:
			// A LATER RUN'S START ENDS THE EARLIER RUNS' LINES: their takes
			// are confirmed only before it ([geminiAskedTake]).
			for i := range asked {
				if asked[i].until < 0 {
					asked[i].until = *t.From
				}
			}
			from = *t.From
			started = true
		case t.Take != "":
			recorded = append(recorded, t)
			asked = append(asked, geminiAskedTake{digest: t.Digest, from: from, until: -1})
		}
	}
	// A RECORD WITHOUT ITS START LINE was lost with its box (a box reads a
	// missing file as empty), so a take it may have held stays undecided.
	if !started {
		return undecided("the run's record of its takes holds no start line, so the box lost it")
	}
	held := map[string]bool{}
	if len(recorded) > 0 {
		session, err := geminiSessionFile(ctx, box, paths, ask.Session)
		if err != nil {
			return undecided(err.Error())
		}
		confirmed, err := geminiConfirmed(ctx, session, asked, maxLineBytes)
		_ = session.Close()
		if err != nil {
			return undecided("the run's session could not be read whole: " + err.Error())
		}
		for i, ok := range confirmed {
			if ok {
				held[recorded[i].Take] = true
			}
		}
	}
	var receipts sandbox.TakeReceipts
	for _, asked := range ask.Takes {
		switch {
		case held[asked.Take]:
			receipts.Held = append(receipts.Held, asked.Take)
		case ask.Ended:
			receipts.Absent = append(receipts.Absent, asked.Take)
		}
	}
	if len(receipts.Held) == 0 && len(receipts.Absent) == 0 {
		receipts.Why = "the run's session does not show the takes yet, and its job is not over"
	}
	return receipts
}

// geminiSessionFile opens the main session of session in box: the first chat
// file naming it ([geminiSessionFiles]).
func geminiSessionFile(ctx context.Context, box sandbox.Sandbox, paths Paths, session string) (io.ReadCloser, error) {
	names, err := geminiSessionFiles(ctx, box, paths, session)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("the run's Gemini CLI session %s has no chat file in the box", session)
	}
	return box.OpenFile(ctx, paths.GeminiChats()+"/"+names[0])
}

// geminiSessionFiles are the chat files under [Paths.GeminiChats] holding
// session in box, by name: each named after its first eight characters whose
// first line names it whole, as two sessions may share the eight.
func geminiSessionFiles(ctx context.Context, box sandbox.Sandbox, paths Paths, session string) ([]string, error) {
	if len(session) < 8 || !sandbox.ValidSessionID(session) {
		return nil, fmt.Errorf("the run's Gemini CLI session %q is no session id Gemini CLI names a file by", session)
	}
	res, err := box.Exec(ctx, "ls -1 "+shellword.Quote(paths.GeminiChats()), sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("the run's Gemini CLI session %s has no chat directory to read", session)
	}
	tail := "-" + session[:8] + ".jsonl"
	var names []string
	for name := range strings.Lines(res.Stdout) {
		name = strings.TrimSpace(name)
		if !strings.HasPrefix(name, "session-") || !strings.HasSuffix(name, tail) || strings.Contains(name, "/") {
			continue
		}
		file, err := box.OpenFile(ctx, paths.GeminiChats()+"/"+name)
		if err != nil {
			continue
		}
		first, _ := bufio.NewReader(io.LimitReader(file, int64(maxLineBytes))).ReadBytes('\n')
		_ = file.Close()
		var head struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(first, &head) != nil || head.SessionID != session {
			continue
		}
		names = append(names, name)
	}
	return names, nil
}
