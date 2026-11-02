package doclet_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// A SEAT'S REMARK MADE AGAIN AFTER A DIFFERENT CALL IS A COMMENT OF ITS OWN,
// however it named the page — end to end, from the model's tool call to the
// rows.
//
// comment_on_page takes its page as an id or as "CONTAINER/Title", so one
// page can be named two ways. A turn's comment id is derived from what the
// call RESOLVED to (the page id, the body) and its repeat count, and the
// count compares the arguments AS TYPED, skipping every earlier call that
// was this same call. Without the arguments in the identity the two could
// disagree: "ok" by id, the same again, "ok" by title, "other" by id and
// then "ok" by id once more gave the last call a count of two — the title
// call and "other", the two "ok"s by id being itself — which is exactly the
// count the title call had. So the last call derived the title comment's id
// and was answered with that comment's success while nothing was written,
// though it was the same words said again after a different remark, which
// the rule says is a remark of its own.
//
// Nothing between the call and the rows is mocked: the store derives the id
// from the key the tool hands it, the tool reads the key's count from the log,
// and the surface writes the log — so the property lives in three packages and
// is asserted where all three meet.
func TestASeatsRemarkAfterAnotherCallIsAnotherCommentHoweverItNamedThePage(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), doclet.NewPage{
		Container: "ENG", Title: "Runbook", Body: "prose"}).Page.ID
	seat := newSeatPages(t, r)
	byID := func(body string) map[string]any {
		return map[string]any{"page": page, "body": body}
	}
	byTitle := func(body string) map[string]any {
		return map[string]any{"page": "ENG/Runbook", "body": body}
	}

	run := func(runID string) []string {
		call := seat.run(runID)
		return []string{
			call.comment(byID("ok")),
			// ASKED AGAIN WITH NOTHING BETWEEN, as an executor that
			// repeats itself does: the same comment.
			call.comment(byID("ok")),
			// OTHER ARGUMENTS for the same page and words: a call of its
			// own, and a comment of its own.
			call.comment(byTitle("ok")),
			call.comment(byID("other")),
			// THE FIRST CALL AGAIN, after different ones.
			call.comment(byID("ok")),
		}
	}
	bodies := func() []string {
		t.Helper()
		thread, err := r.store.Thread(t.Context(), page)
		if err != nil {
			t.Fatalf("read the thread: %v", err)
		}
		out := make([]string, 0, len(thread))
		for _, c := range thread {
			out = append(out, c.Body)
		}
		return out
	}

	first := run("run-1")
	if first[1] != first[0] {
		t.Errorf("the same call asked twice with nothing between answered %s "+
			"and %s — one remark is two comments", first[0], first[1])
	}
	if first[4] == first[2] || first[4] == first[0] {
		t.Errorf("\"ok\" by id after other calls answered %s, the comment an "+
			"earlier call made (%v) — it was answered with that comment's "+
			"success and nothing was written", first[4], first)
	}
	want := []string{"ok", "ok", "other", "ok"}
	if got := bodies(); !slices.Equal(got, want) {
		t.Fatalf("the thread reads %q, want %q: the repeat collapsed, and the "+
			"title call and the last \"ok\" each a comment of its own", got, want)
	}

	// THE RE-RUN: a redelivered trigger runs the turn again, under a new run
	// id with an empty call log, and makes the same calls.
	if again := run("run-2"); !slices.Equal(again, first) {
		t.Errorf("the re-run answered %v, the first run %v — a re-run's calls "+
			"must reach the comments the first run made", again, first)
	}
	if got := bodies(); !slices.Equal(got, want) {
		t.Fatalf("after a re-run the thread reads %q, want the first run's %q",
			got, want)
	}
}

// seatPages is the seat `eng`'s own page tools over a round's store, as a real
// turn reaches them: through the tool surface a turn's calls go through, which
// keeps the run's call log.
type seatPages struct {
	t   *testing.T
	r   *roundTrip
	reg *tools.Registry
	org *org.Organization
}

func newSeatPages(t *testing.T, r *roundTrip) seatPages {
	t.Helper()
	company := &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "eng"}},
	}
	company.Normalize()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Pages:     builtin.PageDeps{Reader: sessionPages{r.reader}, Writer: r.store},
		Authorize: builtin.Decide(authz.NoChart{}),
	}); err != nil {
		t.Fatalf("register the page tools: %v", err)
	}
	return seatPages{t: t, r: r, reg: reg, org: company}
}

// sessionPages serves the seat's reads at `session`, whatever level the tool
// asks for.
//
// A seat reads LINEARIZABLY, which appends a barrier to the log and waits for
// this node's applier to reach it — and this harness applies by hand, after a
// call returns, and its read authority has no read index at all. Every call
// here is drained before the next one is made, so the rows a read finds are
// already the log's; the level is not what these cases are about.
type sessionPages struct{ *doclet.Reader }

func (r sessionPages) List(ctx context.Context, f doclet.Filter,
	_ statelog.Freshness) (doclet.Listing, error) {
	return r.Reader.List(ctx, f, statelog.Freshness{Level: statelog.ReadSession})
}

func (r sessionPages) Get(ctx context.Context, ref string,
	_ statelog.Freshness) (doclet.Detail, error) {
	return r.Reader.Get(ctx, ref, statelog.Freshness{Level: statelog.ReadSession})
}

// seatPageRun is one execution of a turn, making calls.
type seatPageRun struct {
	seat    seatPages
	surface *tools.Surface
}

// run is one execution of the turn — a fresh run id and an empty call log,
// over one unit of work begun at [keyedSince].
func (s seatPages) run(runID string) seatPageRun {
	turn := &turnctx.Turn{
		RunID: runID, WorkKey: "wk-runbook", WorkSince: keyedSince,
		Seat: s.org.Roles[0], Org: s.org, Calls: turnctx.NewCallLog(),
	}
	return seatPageRun{seat: s, surface: tools.NewSurface("execute",
		s.reg.Snapshot(), builtin.PageTools()).ForTurn(turn)}
}

// comment makes one comment_on_page call, which must not fail, applies what
// it wrote, and answers the comment it names.
func (c seatPageRun) comment(args map[string]any) string {
	t := c.seat.t
	t.Helper()
	res, err := c.surface.Execute(t.Context(),
		llm.ToolCall{Name: builtin.CommentOnPageTool, Arguments: args})
	if err != nil || res.Failed {
		t.Fatalf("comment_on_page %v: %v %s", args, err, res.Output)
	}
	c.seat.r.drain()
	var receipt struct {
		CommentID string `json:"comment_id"`
	}
	if err := json.Unmarshal([]byte(res.Output), &receipt); err != nil ||
		receipt.CommentID == "" {
		t.Fatalf("comment_on_page answered %q, which names no comment", res.Output)
	}
	return receipt.CommentID
}
