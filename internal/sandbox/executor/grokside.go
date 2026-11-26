package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// sideCounted is a harness whose run makes requests its stream never shows,
// some of which a record of its own keeps: its count is the stream's and that
// record's together, the two disjoint, live ([sumCount]) and at its collection
// alike. Each side request counted is one the run made, so the sum never
// counts more than it did.
type sideCounted interface {
	// sideCount is a fresh reading of the run's side requests.
	sideCount() sideCounter
}

// sideCounter reads the side requests a run's record shows so far.
type sideCounter interface {
	read(ctx context.Context, box sandbox.Sandbox, paths Paths) (int, error)
}

// sumCount is one live count of a job off its stream and its side record
// ([sideCounted]): the stream's count with the side requests added, neither
// going down.
type sumCount struct {
	stream *counter
	side   sideCounter
	most   int
}

// Read implements [sandbox.Counter].
func (c *sumCount) Read(ctx context.Context, box sandbox.Sandbox) (sandbox.RequestCount, error) {
	streamed, streamErr := c.stream.Read(ctx, box)
	n, sideErr := c.side.read(ctx, box, PathsFor(box))
	c.most = max(c.most, n)
	streamed.Requests += c.most
	return streamed, errors.Join(streamErr, sideErr)
}

// sideCount implements [sideCounted]: what a Grok Build session's directory
// keeps of the requests its stream's usage lines never show (measured on
// 1.0.92 against a stand-in). Each compaction whose summary Grok refused
// leaves a file in compaction_requests/ listing its attempts, each a request,
// the last one Grok kept where the file records no error; each compaction it
// kept leaves a checkpoint, one request at least; and the session's title,
// written once a session to summary.json, is one request. NOT COUNTED: a
// refused compaction's retries past its listed attempts (2 of 5 measured), a
// kept compaction's second request, and a resumed session's title.
func (Grok) sideCount() sideCounter {
	return &grokSideCount{files: map[string]int{}, checkpoints: map[string]bool{}}
}

// grokEntryName is a compaction file's or checkpoint's name: a UUID's JSON.
var grokEntryName = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.json$`)

// grokSideCount is one reading of a Grok Build run's side requests: each
// compaction file read once, by name, and the names its session held as the
// run began ([Paths.GrokRunStart]) left out.
type grokSideCount struct {
	session         string
	titled, started bool
	before          map[string]bool
	files           map[string]int
	checkpoints     map[string]bool
	title           int
}

func (g *grokSideCount) read(ctx context.Context, box sandbox.Sandbox, paths Paths) (int, error) {
	if !g.started {
		if err := g.start(ctx, box, paths); err != nil || !g.started {
			return 0, err
		}
	}
	dir := paths.GrokSessionsKey() + "/" + g.session
	q := shellword.Quote
	res, err := box.Exec(ctx, `cd `+q(dir)+` 2> /dev/null || exit 0; for k in compaction_requests compaction_checkpoints; do `+
		`[ -d "$k" ] && [ ! -L "$k" ] && ls -1 "$k" | sed "s|^|$k |"; done; exit 0`, sandbox.ExecOptions{})
	if err != nil {
		return g.total(), fmt.Errorf("executor: listing a Grok Build session's compactions: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(res.Stdout))
	for sc.Scan() {
		kind, name, _ := strings.Cut(sc.Text(), " ")
		if !grokEntryName.MatchString(name) || g.before[kind+" "+name] {
			continue
		}
		switch kind {
		case "compaction_requests":
			if _, read := g.files[name]; !read {
				n, err := grokAttempts(ctx, box, dir+"/"+kind+"/"+name)
				if err != nil {
					return g.total(), err
				}
				g.files[name] = n
			}
		case "compaction_checkpoints":
			g.checkpoints[name] = true
		}
	}
	if g.title == 0 && !g.titled {
		if raw, err := box.ReadFile(ctx, dir+"/summary.json"); err == nil && grokTitled(raw) {
			g.title = 1
		}
	}
	return g.total(), nil
}

func (g *grokSideCount) total() int {
	n := len(g.checkpoints) + g.title
	for _, attempts := range g.files {
		n += attempts
	}
	return n
}

// start reads where the run began in its session ([Paths.GrokRunStart]): its
// session, whether it had a title, and the compaction entries it held.
func (g *grokSideCount) start(ctx context.Context, box sandbox.Sandbox, paths Paths) error {
	raw, err := box.ReadFile(ctx, paths.GrokRunStart())
	if err != nil {
		return fmt.Errorf("executor: reading where a Grok Build run began in its session: %w", err)
	}
	g.before = map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "session":
			g.session = value
		case "titled":
			g.titled = true
		case "compaction_requests", "compaction_checkpoints":
			g.before[line] = true
		}
	}
	g.started = sandbox.ValidSessionID(g.session)
	return nil
}

// grokAttempts is the requests a compaction file records: its attempts, each
// a request, less the one Grok kept where it records no error, which the
// compaction's checkpoint counts. A file too large to read counts none.
func grokAttempts(ctx context.Context, box sandbox.Sandbox, path string) (int, error) {
	raw, err := box.ReadFile(ctx, path)
	if errors.Is(err, sandbox.ErrFileTooLarge) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("executor: reading a Grok Build compaction's record: %w", err)
	}
	var file struct {
		Error    string            `json:"error"`
		Attempts []json.RawMessage `json:"attempt_details"`
	}
	if json.Unmarshal(raw, &file) != nil {
		return 0, nil
	}
	if file.Error == "" {
		return max(len(file.Attempts)-1, 0), nil
	}
	return len(file.Attempts), nil
}

// grokTitled reports whether a session's summary.json names its generated title.
func grokTitled(raw []byte) bool {
	var summary struct {
		Title string `json:"generated_title"`
	}
	return json.Unmarshal(raw, &summary) == nil && summary.Title != ""
}

// grokRunStart is the part of a run's script that records where it begins in
// its session ([Paths.GrokRunStart]) before its harness starts: the session,
// whether it already holds a title, and the compaction entries it already holds.
func grokRunStart(paths Paths) string {
	q := shellword.Quote
	return `{ printf 'session %s\n' "$s"; x=` + q(paths.GrokSessionsKey()) + `/"$s"; if [ -d "$x" ] && [ ! -L "$x" ]; then ` +
		`grep -q '"generated_title": *"[^"]' "$x/summary.json" 2> /dev/null && echo titled; for k in compaction_requests compaction_checkpoints; do ` +
		`[ -d "$x/$k" ] && ls -1 "$x/$k" | sed "s|^|$k |"; done; fi; } > ` + q(paths.GrokRunStart()+".next") +
		` && mv -f ` + q(paths.GrokRunStart()+".next") + " " + q(paths.GrokRunStart()) + "\n"
}
