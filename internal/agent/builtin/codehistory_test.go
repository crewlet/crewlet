package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// historyNode is a git node whose reads by object id its own query server
// answers — the page cutting the code tools send — over a copy and a git that
// stand in for a repository: a history of `commits` commits, newest first, and
// a file of `lines` lines, each line last changed by a commit of its own, every
// commit's subject `subject` and every line's text `text`. Its git honours
// what the reader asks — a history's skip, a blame's range — and stops
// writing when the reader stops reading, as git's pipe does.
type historyNode struct {
	commits int
	lines   int
	subject string
	text    string
}

// host is the code host over site whose reads by object id n's query server
// answers.
func (n historyNode) host(t *testing.T) builtin.CodeReader {
	t.Helper()
	s, err := query.New(query.Options{NodeID: "git-node", Cache: historyCopy{}, Git: n,
		Share: code.NewGitShare("git-node", 4, nil), Resyncs: code.NewResyncs(nil, nil),
		Roster:  func() code.Roster { return nil },
		Fetched: func(context.Context, types.CodeFetched) {}})
	if err != nil {
		t.Fatal(err)
	}
	return queryHost{codeHost: newCodeHost(), s: s}
}

// queryHost is the code host whose reads by object id a query server answers.
type queryHost struct {
	*codeHost
	s *query.Server
}

func (h queryHost) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	return h.s.Read(ctx, q)
}

// historyCopy is the copy a historyNode serves: site's, at one position, with
// no index of pushes and no memo, so every page is cut by a run of its own.
type historyCopy struct{}

func (historyCopy) Holds(string) bool { return true }
func (historyCopy) Sync(context.Context, string, statelog.Freshness) (cache.Synced, error) {
	return cache.Synced{Dir: "/history-node/site", Repo: siteDetail().RepoRef, Position: codeAt(12)}, nil
}
func (historyCopy) ServeConfig() []gitproc.KV { return nil }
func (historyCopy) Provenance(cache.Synced) (cache.Provenance, error) {
	return cache.Provenance{}, errors.New("the stand-in keeps no index of pushes")
}
func (historyCopy) WantHistory(string)                                  {}
func (historyCopy) MemoGet(cache.Synced, cache.MemoKey) ([]byte, bool)  { return nil, false }
func (historyCopy) Memo(cache.Synced) *cache.MemoBatch                  { return nil }
func (historyCopy) Spool() (*os.File, error)                            { return nil, os.ErrInvalid }
func (historyCopy) IndexTip(cache.Synced) (cache.IndexedTip, bool)      { return cache.IndexedTip{}, false }
func (historyCopy) CodeIndex(cache.Synced) (*cache.CodeIndexView, bool) { return nil, false }

// commitID is the stand-in's i-th commit's id.
func commitID(i int) string { return fmt.Sprintf("%040x", i+1) }

// Run answers the three processes a history's and a blame's read runs: the
// check of what it names (every id a commit, every path a file), the walk and
// the blame.
func (n historyNode) Run(_ context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	var out bytes.Buffer
	switch c.Args[0] {
	case "cat-file":
		in, err := io.ReadAll(c.Stdin)
		if err != nil {
			return gitproc.Result{}, err
		}
		for _, line := range strings.Split(strings.TrimSuffix(string(in), "\x00"), "\x00") {
			if id, _, path := strings.Cut(line, ":"); path {
				fmt.Fprintf(&out, "%s blob 100\x00", blobOID)
			} else {
				fmt.Fprintf(&out, "%s commit 100\x00", strings.TrimSuffix(id, "^{}"))
			}
		}
		_, err = c.Stdout.Write(out.Bytes())
		return gitproc.Result{}, err
	case "log":
		skip := 0
		for _, a := range c.Args {
			if n, ok := strings.CutPrefix(a, "--skip="); ok {
				skip, _ = strconv.Atoi(n)
			}
		}
		for i := skip; i < n.commits; i++ {
			record := strings.Join([]string{commitID(i), commitID(i + 1), "Ana", "ana@example.com", "1700000000",
				"1700000000", n.subject}, "\x00") + "\x00\x00"
			if _, err := c.Stdout.Write([]byte(record)); err != nil {
				return gitproc.Result{}, fmt.Errorf("%w: git log: %w", gitproc.ErrOutput, err)
			}
		}
	case "blame":
		from, to, _ := strings.Cut(c.Args[3], ",")
		first, _ := strconv.Atoi(from)
		last, _ := strconv.Atoi(to)
		for line := first; line <= min(last, n.lines); line++ {
			group := fmt.Sprintf("%s %d %d 1\nauthor Ana\nauthor-mail <ana@example.com>\nauthor-time 1700000000\n"+
				"author-tz +0000\nsummary %s\nfilename f.txt\n\t%s\n", commitID(line), line, line, n.subject, n.text)
			if _, err := c.Stdout.Write([]byte(group)); err != nil {
				return gitproc.Result{}, fmt.Errorf("%w: git blame: %w", gitproc.ErrOutput, err)
			}
		}
	default:
		return gitproc.Result{}, fmt.Errorf("the stand-in runs no git %s", c.Args[0])
	}
	return gitproc.Result{}, nil
}
