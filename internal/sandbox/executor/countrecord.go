package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUNNER WHOSE STREAM SHOWS ITS REQUESTS ONLY ONCE ITS HARNESS EXITS is
// counted live off the harness's own record of the session, which shows each
// request as it is made (measured on each pinned release against a stand-in:
// Codex's rollout, Gemini CLI's chat file and telemetry, Copilot's
// events.jsonl and Muse Code's session.jsonl, each within about a second of the
// request), and which its collection counts too, by the same rule
// ([recordCounted], [recordsCounted]); Hermes's off its session store
// ([storeCounted]). Where the script finds the record as the run goes, it
// names it in [Paths.CountRecord], a pointer the count holds to the record's
// place before it reads a byte ([recordPlace]).

// CountRecord is where a run's script names its harness's own record of the
// session as it finds it ([recordCounted]): the record's path, then how many of
// its lines are earlier runs', each on a line of its own, written whole.
func (p Paths) CountRecord() string { return p.WorkDir() + "/count-record" }

// recordCounted is a harness whose run's stream shows its model requests only
// once the harness exits, while its own record of the session shows each one as
// it is made: its live count reads that record ([Runner.Count]).
type recordCounted interface {
	// liveRecord is the run's record as the box now names it, and how many of
	// its lines are earlier runs'; ok is false while it names none, or names
	// one its place refuses ([recordPlace.named]).
	liveRecord(ctx context.Context, box sandbox.Sandbox, paths Paths) (path string, skip int, ok bool, err error)

	// recordTally is a fresh count of the record's lines, the run's own, in its
	// collection's unit; ctx and box read what the count needs beside them.
	recordTally(ctx context.Context, box sandbox.Sandbox, paths Paths) (recordTally, error)
}

// recordsCounted is a harness whose run's requests are counted off more than
// one record of its own, each showing some of them and none all: its count is
// their largest, live ([maxCount]) and at its collection alike, which never
// counts more than the run made.
type recordsCounted interface {
	countedRecords() []recordCounted
}

// maxCount is one live count of a job off several records ([recordsCounted]),
// its stream among them: the most requests any of them shows, and, where a
// record holds lines it cannot read, as many undecided as take the reading
// that may show the most to its most ([reachOf]). Each part never goes down,
// so neither does it.
type maxCount struct{ parts []sandbox.Counter }

// Read implements [sandbox.Counter]: every part is read, and a part that
// failed fails the read, the others' count still standing.
func (m *maxCount) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	counts := make([]sandbox.RequestCount, 0, len(m.parts))
	var errs []error
	for _, p := range m.parts {
		c, err := p.Read(ctx, box)
		counts = append(counts, c)
		if err != nil {
			errs = append(errs, err)
		}
	}
	return reachOf(counts...), errors.Join(errs...)
}

// reachOf is counts read as one: the most requests any shows, and undecided
// lines enough that requests and undecided together are the most any shows.
func reachOf(counts ...sandbox.RequestCount) sandbox.RequestCount {
	var out sandbox.RequestCount
	reach := 0
	for _, c := range counts {
		out.Requests = max(out.Requests, c.Requests)
		out.InFlight = max(out.InFlight, c.InFlight)
		reach = max(reach, c.Requests+c.Undecided)
	}
	out.Undecided = reach - out.Requests
	return out
}

// recordTally counts the requests the lines of a run's own record show, a line
// at a time, as [Decoder] counts its stream's.
type recordTally interface {
	Line(line []byte)
	Skipped(line LongLine)
	requests() int
	undecided() int
}

// recordPlace is where a harness keeps its record of a session: under root,
// depth directories down, in a file whose name matches name.
type recordPlace struct {
	root  string
	depth int
	name  *regexp.Regexp
}

// named reads the run's pointer ([Paths.CountRecord]) and holds it to the
// place: under the root at the depth, its parts clean, its name matching.
// ok is false for no pointer yet, and for one the place refuses: such a run is
// counted at its collection alone. The box confines every read to its home, so
// a link out of the home reads nothing either way.
func (place recordPlace) named(ctx context.Context, box sandbox.Sandbox, paths Paths) (string, int, bool, error) {
	raw, err := box.ReadFile(ctx, paths.CountRecord())
	if err != nil {
		return "", 0, false, fmt.Errorf("executor: reading the run's pointer to its record: %w", err)
	}
	fields := strings.SplitN(string(raw), "\n", 3)
	if len(fields) < 2 {
		return "", 0, false, nil
	}
	named, lines := fields[0], lineCount(fields[1])
	if lines < 0 || !place.holds(named) {
		return "", 0, false, nil
	}
	return named, lines, true, nil
}

// lineCount is s read as a count of lines, -1 for no count.
func lineCount(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// holds reports whether p is a file of the place: under the root at the
// place's depth, with no part that climbs, stays or names nothing (so a path
// that would clean to another), and named as the place's files are.
func (place recordPlace) holds(p string) bool {
	if !strings.HasPrefix(p, place.root+"/") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(p, place.root+"/"), "/")
	if len(parts) != place.depth+1 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return place.name.MatchString(parts[len(parts)-1])
}

// recordCount is one live count of a job off its harness's own record
// ([recordCounted]), read as [counter] reads a stream: only what grew, at most
// readBytes a read, never skipping, from the start again where the record
// shrank, the count never going down.
type recordCount struct {
	harness   recordCounted
	bound     int
	readBytes int64
	inFlight  int

	// path is the record once the run named one its place holds, and skip its
	// lines that are earlier runs'; the first named stands, so a pointer
	// written over later moves nothing.
	path  string
	skip  int
	lines streamLines
	seen  int
	rec   recordTally
	most  sandbox.RequestCount
}

// Read implements [sandbox.Counter].
func (c *recordCount) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	return c.read(ctx, box, false)
}

// read is [recordCount.Read]; with end, the job is over, and a last line with
// no break is a line.
func (c *recordCount) read(ctx context.Context, box sandbox.Sandbox, end bool) (sandbox.RequestCount, error) {
	paths := PathsFor(box)
	if c.path == "" {
		named, skip, ok, err := c.harness.liveRecord(ctx, box, paths)
		if err != nil || !ok {
			return c.most, err
		}
		c.path, c.skip = named, skip
	}
	if c.rec == nil {
		if err := c.restart(ctx, box, paths); err != nil {
			return c.most, err
		}
	}
	body, size, err := box.OpenRange(ctx, c.path, c.lines.taken, c.readBytes)
	if err == nil && size < c.lines.taken {
		// SHRUNK: the record was written anew, and is read again from its start.
		_ = body.Close()
		if err = c.restart(ctx, box, paths); err == nil {
			body, _, err = box.OpenRange(ctx, c.path, 0, c.readBytes)
		}
	}
	if err != nil {
		return c.most, fmt.Errorf("executor: opening the run's record to count its requests: %w", err)
	}
	defer func() { _ = body.Close() }()
	err = c.lines.read(bufio.NewReaderSize(body, lineChunkBytes), end,
		func(line []byte) error {
			if c.seen++; c.seen > c.skip {
				c.rec.Line(line)
			}
			return nil
		},
		func(n int64, head, tail []byte) error {
			if c.seen++; c.seen > c.skip {
				c.rec.Skipped(LongLine{Size: n, Bound: c.bound, Head: head, Tail: tail})
			}
			return nil
		})
	c.most = sandbox.RequestCount{
		Requests:  max(c.most.Requests, c.rec.requests()),
		InFlight:  c.inFlight,
		Undecided: max(c.most.Undecided, c.rec.undecided()),
	}
	if err != nil {
		return c.most, fmt.Errorf("executor: reading the run's record to count its requests: %w", err)
	}
	return c.most, nil
}

// recordCollected is a [recordCounted] harness whose collection counts its
// requests off the same record, read whole by the same rule, rather than off
// lines its script appends to its stream.
type recordCollected interface {
	recordCounted
	collectsFromRecord()
}

// collectedRecord is the requests the run's own record shows once its job is
// over ([recordCollected]), read whole by the rule its live count reads it with:
// none where the record is not there.
func (r *Runner) collectedRecord(ctx context.Context, box sandbox.Sandbox, rc recordCollected) (
	sandbox.RequestCount, error) {

	c := &recordCount{harness: rc, bound: r.lineBound, readBytes: math.MaxInt64, inFlight: r.harness.Events().InFlight()}
	return c.read(ctx, box, true)
}

// collectedRecords is the requests a harness counted off several records shows
// once its job is over ([recordsCounted]): the largest of streamed, its
// stream's count of the records whose lines its script appends there, and
// each record its collection reads itself ([recordCollected]), as [maxCount]
// reads them live.
func (r *Runner) collectedRecords(ctx context.Context, box sandbox.Sandbox, rs recordsCounted,
	streamed sandbox.RequestCount) (sandbox.RequestCount, error) {

	counts := []sandbox.RequestCount{streamed}
	for _, rc := range rs.countedRecords() {
		if collected, ok := rc.(recordCollected); ok {
			c, err := r.collectedRecord(ctx, box, collected)
			if err != nil {
				return sandbox.RequestCount{}, err
			}
			counts = append(counts, c)
		}
	}
	return reachOf(counts...), nil
}

// restart begins the reading at the record's first byte, with a fresh count.
func (c *recordCount) restart(ctx context.Context, box sandbox.Sandbox, paths Paths) error {
	rec, err := c.harness.recordTally(ctx, box, paths)
	if err != nil {
		return err
	}
	c.rec, c.seen = rec, 0
	c.lines = streamLines{bound: c.bound, ends: true}
	return nil
}

// wrapped is a record line's ends as the line stands once a script wraps it
// into its stream, prefix before and suffix after it, held
// to the windows a stream line past its bound keeps ([skipHeadBytes],
// [skipTailBytes]): so a line the count skipped is judged on what the
// collection's decoder would see of the same line.
func wrapped(line LongLine, prefix, suffix string) LongLine {
	head := append([]byte(prefix), line.Head...)
	head = head[:min(len(head), skipHeadBytes)]
	tail := append(append([]byte{}, line.Tail...), suffix...)
	tail = tail[max(len(tail)-skipTailBytes, 0):]
	return LongLine{Size: line.Size + int64(len(prefix)+len(suffix)), Bound: line.Bound, Head: head, Tail: tail}
}
