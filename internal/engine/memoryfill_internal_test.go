package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// seatsHeld is a seat host as the fill reads it: which seats are held, and
// which of those are established.
type seatsHeld struct {
	held        []string
	established map[string]bool
}

func (s seatsHeld) Held() []string { return append([]string(nil), s.held...) }

func (s seatsHeld) MayStart(handle string) (int64, bool) {
	return 1, s.established[handle]
}

// allEstablished holds and establishes every handle named.
func allEstablished(handles ...string) seatsHeld {
	s := seatsHeld{held: handles, established: map[string]bool{}}
	for _, h := range handles {
		s.established[h] = true
	}
	return s
}

// diaryFixture is a node store at width 64 holding the vectorless notes named
// for each agent.
func diaryFixture(t *testing.T, notes map[string][]string) (*store.DB, *learning.Diary) {
	t.Helper()
	db := storetest.OpenNode(t, t.TempDir()+"/n.db", store.Options{EmbeddingDim: 64})
	t.Cleanup(func() { _ = db.Close() })
	diary := learning.NewDiary(db)
	at := time.Now().UTC().Add(-time.Hour)
	for agent, contents := range notes {
		for i, content := range contents {
			if err := diary.Write(t.Context(), learning.DiaryEntry{
				ID: fmt.Sprintf("%s-%d", agent, i), AgentID: agent, Kind: learning.DiaryLong,
				Content: content, CreatedAt: at.Add(time.Duration(i) * time.Second),
			}); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	}
	return db, diary
}

func unfilled(t *testing.T, diary *learning.Diary, agent, model string) int {
	t.Helper()
	left, err := diary.Unfilled(t.Context(), agent, model, time.Now().UTC(),
		learning.FillCursor{}, 1000)
	if err != nil {
		t.Fatalf("Unfilled: %v", err)
	}
	return len(left)
}

// facts is n notes of one length, so a budget in bytes is a budget in notes.
func facts(n int, marker string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s durable fact number %03d", marker, i)
	}
	return out
}

// tick runs one tick of the fill over seats as fillMemory would, under the
// given memory and pass allowance.
func tick(t *testing.T, db *store.DB, seats heldSeats, ids func(string) string,
	provider embeddings.BatchEmbedder, memory *embeddings.Refusals, at time.Time,
	requests, bytes int, resume string,
) fillReport {
	t.Helper()
	pass := embeddings.NewPass(memory, at, requests, bytes)
	return fillHeldMemory(t.Context(), seats, memorySources(db, ids), provider, pass, resume)
}

var fillAt = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

// ONLY AN ESTABLISHED SEAT THIS NODE HOLDS IS FILLED. A seat still
// establishing has not hydrated its memory — filling it would embed whatever
// stale copy this node happens to have — and a seat this node does not hold
// is its holder's to fill.
func TestTheFillTouchesOnlyEstablishedHeldSeats(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
		"id-ready":   {"the release train is thursdays", "pat wants digests"},
		"id-joining": {"a note from a seat still hydrating"},
		"id-elsew":   {"a seat another node holds"},
	})
	fake := embeddings.NewFake(64)
	seats := seatsHeld{
		held:        []string{"joining", "ready"},
		established: map[string]bool{"ready": true},
	}
	ids := map[string]string{"ready": "id-ready", "joining": "id-joining", "elsewhere": "id-elsew"}
	report := tick(t, db, seats, func(h string) string { return ids[h] }, fake,
		embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick, memoryFillBytesPerTick, "")
	if report.filled["diary"] != 2 {
		t.Fatalf("filled %d notes, want the established seat's two", report.filled["diary"])
	}
	if n := unfilled(t, diary, "id-ready", fake.Model()); n != 0 {
		t.Errorf("the established seat has %d notes left", n)
	}
	if n := unfilled(t, diary, "id-joining", fake.Model()); n != 1 {
		t.Errorf("a seat still establishing was filled: %d of 1 left", n)
	}
	if n := unfilled(t, diary, "id-elsew", fake.Model()); n != 1 {
		t.Errorf("a seat this node does not hold was filled: %d of 1 left", n)
	}
}

// A TICK IS BOUNDED BY WHAT IT SENDS, ACROSS SEATS: a model change leaves every
// seat's diary in the old space at once, and the budget is what keeps the first
// tick from sending all of it. The next tick starts at the seat this one ran
// out at, so the seats behind it are not for ever behind the ones before.
func TestAFillTickStopsAtItsBudgetAndTheNextResumesWhereItStopped(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
		"id-a": facts(5, "a"), "id-b": facts(5, "b"), "id-c": facts(5, "c"),
	})
	fake := embeddings.NewFake(64)
	seats := allEstablished("a", "b", "c")
	ids := func(h string) string { return "id-" + h }
	one := len(embeddings.Prepare(facts(1, "a")[0]))
	memory := embeddings.NewRefusals()

	first := tick(t, db, seats, ids, fake, memory, fillAt, memoryFillRequestsPerTick, 7*one, "")
	if first.filled["diary"] != 7 || first.pass.Bytes > 7*one {
		t.Fatalf("filled %d notes in %d bytes with a budget of 7 notes", first.filled["diary"], first.pass.Bytes)
	}
	if first.resume != "b" {
		t.Fatalf("the tick ran out at seat %q, want b", first.resume)
	}
	second := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Minute),
		memoryFillRequestsPerTick, 5*one, first.resume)
	if second.filled["diary"] != 5 {
		t.Fatalf("the next tick filled %d notes", second.filled["diary"])
	}
	if n := unfilled(t, diary, "id-b", fake.Model()); n != 0 {
		t.Errorf("the seat the first tick ran out at still has %d notes: the next tick did not resume there", n)
	}
	if n := unfilled(t, diary, "id-c", fake.Model()); n != 3 {
		t.Errorf("seat c has %d notes left, want the 3 past the second budget", n)
	}
}

// ONE NOTE THE PROVIDER REFUSES DOES NOT HOLD BACK ITS NEIGHBOURS, AND IS NOT
// SENT AGAIN ON THE NEXT TICK: a refused call is split until the note is
// alone, every note the provider takes is filled, and the poison is held back
// for the hour rather than isolated again every minute.
func TestARefusedNoteIsIsolatedOnceAndNotSentAgainOnTheNextTick(t *testing.T) {
	t.Parallel()
	db, diary := diaryFixture(t, map[string][]string{
		"id-a": {"the release train is thursdays", "a poison note", "pat wants digests"},
	})
	fake := embeddings.NewFake(64)
	fake.Refuse("poison")
	seats := allEstablished("a")
	ids := func(string) string { return "id-a" }
	memory := embeddings.NewRefusals()
	first := tick(t, db, seats, ids, fake, memory, fillAt, memoryFillRequestsPerTick,
		memoryFillBytesPerTick, "")
	if first.filled["diary"] != 2 {
		t.Fatalf("filled %d notes, want the two the provider takes", first.filled["diary"])
	}
	if isolating := first.pass.Requests - first.pass.Canaries; isolating > 5 || first.pass.Canaries != 1 {
		t.Errorf("isolating one note among three took %d requests and %d canaries, want at "+
			"most 1 + 2·⌈log₂3⌉ = 5 and the one canary its first refusal is judged by",
			isolating, first.pass.Canaries)
	}
	left, err := diary.Unfilled(context.Background(), "id-a", fake.Model(), time.Now().UTC(),
		learning.FillCursor{}, 10)
	if err != nil || len(left) != 1 || left[0].Text != "a poison note" {
		t.Fatalf("left = %v, %v; want only the refused note", left, err)
	}

	before := len(fake.Requests())
	next := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Minute),
		memoryFillRequestsPerTick, memoryFillBytesPerTick, first.resume)
	if sent := len(fake.Requests()) - before; sent != 0 || next.pass.Held != 1 {
		t.Fatalf("the next tick sent %d requests for the held note (held %d)", sent, next.pass.Held)
	}
}

// A FAILURE THAT IS NOT ABOUT A NOTE ENDS THE TICK FOR EVERY SEAT: a throttled
// account or a revoked key answers every seat's batch the same way, and each
// held seat sending its own into it every minute was the burst a throttled
// account least needs.
func TestAProviderFailureEndsTheTickForEverySeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fail  func(*embeddings.Fake)
		class error
	}{
		{"429", func(f *embeddings.Fake) { f.FailTransiently("durable", 1000) }, embeddings.ErrTransient},
		{"401", func(f *embeddings.Fake) { f.FailConfiguration("durable", 1000) }, embeddings.ErrConfiguration},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db, diary := diaryFixture(t, map[string][]string{
				"id-a": facts(3, "a"), "id-b": facts(3, "b"), "id-c": facts(3, "c"),
			})
			fake := embeddings.NewFake(64)
			tc.fail(fake)
			report := tick(t, db, allEstablished("a", "b", "c"), func(h string) string { return "id-" + h },
				fake, embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick,
				memoryFillBytesPerTick, "")
			if !errors.Is(report.pass.Err(), tc.class) {
				t.Fatalf("the tick ended with %v, want %v", report.pass.Err(), tc.class)
			}
			if sent := len(fake.Requests()); sent != 1 {
				t.Fatalf("the tick sent %d requests across three seats, want the one "+
					"that met the failure", sent)
			}
			if report.resume != "a" {
				t.Errorf("the next tick would start at %q, want the seat the failure met", report.resume)
			}
			for _, agent := range []string{"id-a", "id-b", "id-c"} {
				if n := unfilled(t, diary, agent, fake.Model()); n != 3 {
					t.Errorf("%s has %d of 3 notes left", agent, n)
				}
			}
		})
	}
}

// A PROVIDER THAT REFUSES ITS CONFIGURATION — every request, whatever it carries
// — is concluded refused by the tick that meets it, in the refused call and the
// canary that judges it, and then sent nothing for the pause: where retrying
// each note alone was one request and one warning per note per seat per minute,
// and isolating every note before concluding was a hundred and twenty-seven
// silent refused requests a seat.
func TestAProviderRefusingEveryNoteIsBoundedThenLeftAlone(t *testing.T) {
	t.Parallel()
	db, _ := diaryFixture(t, map[string][]string{
		"id-a": facts(embeddings.PassBatch, "a"), "id-b": facts(embeddings.PassBatch, "b"),
		"id-c": facts(embeddings.PassBatch, "c"),
	})
	fake := embeddings.NewFake(64)
	fake.Refuse("") // every request carries the empty marker: a refused setting
	seats := allEstablished("a", "b", "c")
	ids := func(h string) string { return "id-" + h }
	memory := embeddings.NewRefusals()
	resume := ""
	concluded := -1
	for minute := range 10 {
		before := len(fake.Requests())
		report := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Duration(minute)*time.Minute),
			memoryFillRequestsPerTick, memoryFillBytesPerTick, resume)
		resume = report.resume
		if sent := len(fake.Requests()) - before; sent > memoryFillRequestsPerTick {
			t.Fatalf("tick %d sent %d requests, past the bound of %d", minute, sent,
				memoryFillRequestsPerTick)
		}
		if report.filled["diary"] != 0 {
			t.Fatalf("tick %d filled %d notes from a provider that refuses all of them",
				minute, report.filled["diary"])
		}
		if report.pass.Concluded {
			concluded = minute
			if report.pass.Requests != 2 {
				t.Errorf("concluding took %d requests, want the refused call and its canary",
					report.pass.Requests)
			}
			break
		}
	}
	if concluded != 0 {
		t.Fatalf("a refused configuration was concluded at tick %d, want the first tick that met it",
			concluded)
	}
	before := len(fake.Requests())
	paused := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Duration(concluded+1)*time.Minute),
		memoryFillRequestsPerTick, memoryFillBytesPerTick, resume)
	if sent := len(fake.Requests()) - before; sent != 0 || !paused.pass.Paused {
		t.Fatalf("the tick after the conclusion sent %d requests (paused %v)", sent, paused.pass.Paused)
	}
}

// A TICK WHOSE REQUESTS WERE REFUSED SAYS SO, even when it filled nothing and
// named no row: a page in which every row is refused for what it says takes
// more than one tick to narrow down, and each of those ticks — sixteen refused
// requests apiece — used to log nothing at all, which read as a fill with
// nothing to do.
func TestATickOfRefusedRequestsThatNamedNoRowSaysSo(t *testing.T) {
	t.Parallel()
	db, _ := diaryFixture(t, map[string][]string{"id-a": facts(embeddings.PassBatch, "a")})
	fake := embeddings.NewFake(64)
	fake.Refuse("durable") // every note, and not the canary: the configuration is fine
	report := tick(t, db, allEstablished("a"), func(string) string { return "id-a" }, fake,
		embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick, memoryFillBytesPerTick, "")
	if report.pass.Concluded || len(report.pass.RefusedAlone) != 0 || report.filled["diary"] != 0 {
		t.Fatalf("the fixture did not make a silent tick: concluded %v, %d refused alone, %d filled",
			report.pass.Concluded, len(report.pass.RefusedAlone), report.filled["diary"])
	}
	var isolating *fillLine
	lines := report.lines(fake)
	for i := range lines {
		if lines[i].msg == "memory_fill_refusals_isolating" {
			isolating = &lines[i]
		}
	}
	if isolating == nil {
		t.Fatalf("a tick of %d refused requests reported %v, want it named", report.pass.Refused, lines)
	}
	if isolating.level != slog.LevelInfo || !slices.Contains(isolating.args, any(report.pass.Refused)) {
		t.Errorf("the line is %v at %v, want INFO carrying the %d refused requests",
			isolating.args, isolating.level, report.pass.Refused)
	}

	// A tick that filled rows reports its refused requests on the fill's
	// own line, and needs no second one.
	db2, _ := diaryFixture(t, map[string][]string{"id-a": {"a poison durable fact", "pat wants digests"}})
	poison := embeddings.NewFake(64)
	poison.Refuse("poison")
	filled := tick(t, db2, allEstablished("a"), func(string) string { return "id-a" }, poison,
		embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick, memoryFillBytesPerTick, "")
	for _, line := range filled.lines(poison) {
		if line.msg == "memory_fill_refusals_isolating" {
			t.Errorf("a tick that filled %d rows also reported an isolation line", filled.filled["diary"])
		}
	}
}

// THE ACCOUNT'S BUDGET IS SHARED: every node's fill draws on one account, so
// each takes its share of the company's bytes a tick rather than all of them.
func TestEachNodeTakesItsShareOfTheAccountBudget(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ nodes, want int }{
		{1, memoryFillBytesPerTick}, {4, memoryFillBytesPerTick / 4}, {3, 333_334}, {0, memoryFillBytesPerTick},
	} {
		if got := shareOf(memoryFillBytesPerTick, tc.nodes); got != tc.want {
			t.Errorf("share of %d nodes = %d, want %d", tc.nodes, got, tc.want)
		}
	}
}

// A SEAT'S EPISODES ARE FILLED BESIDE ITS NOTES, from the text each was
// embedded from — its label, what it was asked and what it did — so after a
// model change a seat's history is reachable by meaning again, not only what
// it did since.
func TestTheFillReachesASeatsEpisodesFromWhatTheyStore(t *testing.T) {
	t.Parallel()
	db, _ := diaryFixture(t, map[string][]string{"id-a": {"the release train is thursdays"}})
	at := time.Now().UTC().Add(-time.Hour)
	episodes := learning.NewEpisodes(db)
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "e1", Handle: "a", Role: "Engineer", TurnID: "t1", StartedAt: at, EndedAt: at,
		TaskSummary: "Message from Ana: Slack message", Ask: "rotate the staging certs",
		PlanSummary: "rotated them and restarted the ingress", ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	fake := embeddings.NewFake(64)
	report := tick(t, db, allEstablished("a"), func(string) string { return "id-a" }, fake,
		embeddings.NewRefusals(), fillAt, memoryFillRequestsPerTick, memoryFillBytesPerTick, "")
	if report.filled["diary"] != 1 || report.filled["episodes"] != 1 {
		t.Fatalf("filled %v, want the note and the episode", report.filled)
	}
	want := embeddings.Prepare("Message from Ana: Slack message\n\nrotate the staging certs\n\n" +
		"rotated them and restarted the ingress")
	sent := false
	for _, request := range fake.Requests() {
		for _, input := range request {
			sent = sent || input == want
		}
	}
	if !sent {
		t.Fatalf("the episode was not embedded as its label, ask and account: requests %q", fake.Requests())
	}
	if n, err := episodes.Unsearchable(t.Context(), "a", fake.Model()); err != nil || n != 0 {
		t.Fatalf("after the fill %d episodes are unsearchable, %v", n, err)
	}
}

// slowProvider is the fake behind a provider that answers only so many
// requests in a tick before the tick's minute is gone: a call needing more than
// are left fails as a deadline does, with every request it was answered, and
// its requests count against the tick either way.
type slowProvider struct {
	*embeddings.Fake
	perTick, left int
}

func (s *slowProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	groups, err := s.Limits().Requests(texts)
	if err != nil {
		return nil, err
	}
	if len(groups) > s.left {
		s.left = 0
		return nil, &embeddings.Error{Model: s.Model(), Class: embeddings.ErrTransient,
			Err: context.DeadlineExceeded}
	}
	s.left -= len(groups)
	return s.Fake.EmbedBatch(ctx, texts)
}

// A ROW LONGER THAN A TICK DOES NOT STOP THE FILL. An episode's ask is stored
// whole, so one row can need more requests than a tick has time for; sent as
// one call it failed with the deadline every tick, threw away what it had been
// answered, and — newest first, so the first row again — stopped every row and
// every seat after it for good. Sent a request at a time with what each
// embedded kept, it is finished across ticks, as the vector of its whole text,
// and the seats behind it are filled.
func TestARowLongerThanATickDoesNotStopTheFill(t *testing.T) {
	t.Parallel()
	limits := embeddings.Limits{InputBytes: 64, BatchInputs: 4, BatchBytes: 4096}
	db, diary := diaryFixture(t, map[string][]string{"id-b": facts(3, "b")})
	words := make([]string, 600)
	for i := range words {
		words[i] = fmt.Sprintf("w%04dz", i)
	}
	ask := strings.Join(words, " ")
	at := time.Now().UTC().Add(-time.Hour)
	episodes := learning.NewEpisodes(db)
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "e1", Handle: "a", Role: "Engineer", TurnID: "t1", StartedAt: at, EndedAt: at,
		TaskSummary: "Message from Ana: Slack message", Ask: ask,
		PlanSummary: "read the log and filed the regression", ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	text := "Message from Ana: Slack message\n\n" + ask + "\n\nread the log and filed the regression"
	pieces := embeddings.Chunks(text, limits.InputBytes)
	requests := (len(pieces) + limits.BatchInputs - 1) / limits.BatchInputs
	slow := &slowProvider{Fake: embeddings.NewFake(64), perTick: 8}
	slow.SetLimits(limits)
	if requests <= slow.perTick || requests > 4*slow.perTick {
		t.Fatalf("the row needs %d requests, want more than a tick's %d and a few ticks' worth",
			requests, slow.perTick)
	}
	seats := allEstablished("a", "b")
	ids := func(h string) string { return "id-" + h }
	memory := embeddings.NewRefusals()
	resume := ""
	for minute := 0; ; minute++ {
		if minute > 2*requests {
			t.Fatalf("after %d ticks the row is still unfilled and seat b has %d notes left",
				minute, unfilled(t, diary, "id-b", slow.Model()))
		}
		slow.left = slow.perTick
		report := tick(t, db, seats, ids, slow, memory, fillAt.Add(time.Duration(minute)*time.Minute),
			memoryFillRequestsPerTick, memoryFillBytesPerTick, resume)
		resume = report.resume
		if minute == 0 {
			// A TICK THAT FILLED NOTHING BUT MOVED THE ROW ON SAYS SO.
			named := false
			for _, line := range report.lines(slow) {
				named = named || (line.msg == "memory_fill_input_continued" &&
					slices.Contains(line.args, any("e1")))
			}
			if !named {
				t.Fatalf("the first tick sent %d requests of the row and reported %v, "+
					"want the row named as continued", report.pass.Requests, report.lines(slow))
			}
		}
		if n, err := episodes.Unsearchable(t.Context(), "a", slow.Model()); err != nil {
			t.Fatal(err)
		} else if n == 0 && unfilled(t, diary, "id-b", slow.Model()) == 0 {
			break
		}
	}
	whole, err := embeddings.EmbedWhole(t.Context(), func() *embeddings.Fake {
		f := embeddings.NewFake(64)
		f.SetLimits(limits)
		return f
	}(), text)
	if err != nil {
		t.Fatalf("EmbedWhole: %v", err)
	}
	hits, err := episodes.Recall(t.Context(), learning.RecallQuery{Handle: "a", Embedding: whole,
		Model: slow.Model()})
	if err != nil || len(hits) != 1 || hits[0].Episode.ID != "e1" || hits[0].Similarity < 0.9999 {
		t.Fatalf("recall by the whole text's vector = %+v, %v; want the row, as that vector", hits, err)
	}
}

// A ROW ANSWERED WITH NO DIRECTION DOES NOT STOP THE FILL EITHER. The provider
// accepted the request and answered one piece of a long row with a vector of
// zeros, so the row can keep no vector — and forgotten, as it was, the next
// tick began the row again at its first piece, newest first, and spent the
// same requests reaching the same piece: a row whose prefix costs a tick took
// every tick, and the seat behind it was never filled. Held back for the hour,
// with what was embedded before the piece kept, it costs nothing until its
// retry, never begins again at its first piece, and the seat behind it is
// filled on the next tick.
func TestARowAnsweredWithNoDirectionDoesNotStopTheFill(t *testing.T) {
	t.Parallel()
	limits := embeddings.Limits{InputBytes: 64, BatchInputs: 4, BatchBytes: 4096}
	db, diary := diaryFixture(t, map[string][]string{"id-b": facts(20, "b")})
	words := make([]string, 600)
	for i := range words {
		words[i] = fmt.Sprintf("w%04dz", i)
	}
	ask := strings.Join(words, " ")
	at := time.Now().UTC().Add(-time.Hour)
	episodes := learning.NewEpisodes(db)
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "e1", Handle: "a", Role: "Engineer", TurnID: "t1", StartedAt: at, EndedAt: at,
		TaskSummary: "Message from Ana: Slack message", Ask: ask,
		PlanSummary: "read the log and filed the regression", ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	pieces := embeddings.Chunks("Message from Ana: Slack message\n\n"+ask+
		"\n\nread the log and filed the regression", limits.InputBytes)
	// THE PIECE THE FIFTH REQUEST BEGINS WITH answers zeros, and a tick sends
	// five: the row's prefix and the piece take a whole tick.
	const perTick = 5
	stop := (perTick - 1) * limits.BatchInputs
	fake := embeddings.NewFake(64)
	fake.SetLimits(limits)
	fake.AnswerZero(strings.Fields(pieces[stop])[0])
	whole := 0
	for _, piece := range pieces {
		whole += len(piece)
	}

	seats := allEstablished("a", "b")
	ids := func(h string) string { return "id-" + h }
	memory := embeddings.NewRefusals()
	resume := ""
	filledAt := -1
	for minute := 0; minute < 10; minute++ {
		before := len(fake.Requests())
		report := tick(t, db, seats, ids, fake, memory, fillAt.Add(time.Duration(minute)*time.Minute),
			perTick, memoryFillBytesPerTick, resume)
		resume = report.resume
		if minute == 0 {
			named := false
			for _, line := range report.lines(fake) {
				named = named || (line.msg == "memory_fill_vector_unusable" &&
					slices.Contains(line.args, any("e1")) && slices.Contains(line.args, any(whole)))
			}
			if !named {
				t.Fatalf("the tick that met the piece reported %v, want the row named as "+
					"unusable with its %d bytes", report.lines(fake), whole)
			}
			continue
		}
		for _, request := range fake.Requests()[before:] {
			if slices.Contains(request, pieces[0]) || slices.Contains(request, pieces[stop]) {
				t.Fatalf("minute %d sent the held row again (%q) inside its retry", minute, request[0])
			}
		}
		if filledAt < 0 && unfilled(t, diary, "id-b", fake.Model()) == 0 {
			filledAt = minute
		}
	}
	if filledAt < 0 || filledAt > 1 {
		t.Fatalf("seat b's notes were filled at minute %d, want the tick after the row "+
			"was held — %d of them are still unfilled", filledAt,
			unfilled(t, diary, "id-b", fake.Model()))
	}
	if n, err := episodes.Unsearchable(t.Context(), "a", fake.Model()); err != nil || n != 1 {
		t.Fatalf("%d of seat a's episodes are unsearchable (%v), want the held row", n, err)
	}
}

// THE FILL'S REFUSALS FOLLOW THE PROVIDER'S CONFIGURATION, AS THE CORPUS DUTY'S
// DO.
//
// An apply stores a provider built afresh, and one built the same — an apply
// that changed nothing about the embeddings, a re-activation that rotated the
// key — keeps what the fill holds back: a row the provider refused alone, and
// the pause on a configuration it concluded refused. Keyed on the provider's
// slot, every apply isolated each held row again and lifted the pause, while
// the corpus duty beside it on the same node kept both. One configured
// otherwise starts with nothing held, so a fix is tried at once.
func TestTheFillsRefusalsFollowTheProvidersConfiguration(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, embeddingDutyCompany)
	// The node's own loop would tick on a memory of its own.
	e.stopMemoryFill()
	loop := &memoryFill{}
	apply := func(provider *embeddings.Fake) *embeddings.Refusals {
		t.Helper()
		var held embeddings.Embedder = provider
		e.embeddings.Store(&held)
		e.fillMemory(t.Context(), loop)
		if loop.refusals == nil {
			t.Fatal("a tick with a provider held no refusal memory")
		}
		return loop.refusals
	}
	discard := func([]embeddings.PassInput, [][]float32) error { return nil }
	poison := []embeddings.PassInput{{Scope: "diary/id-a", ID: "n1", Text: "a poison note"}}
	send := func(memory *embeddings.Refusals, provider *embeddings.Fake) *embeddings.Pass {
		t.Helper()
		pass := embeddings.NewPass(memory, time.Now().UTC(), memoryFillRequestsPerTick,
			memoryFillBytesPerTick)
		if err := pass.Embed(t.Context(), provider, poison, discard); err != nil {
			t.Fatalf("embed: %v", err)
		}
		return pass
	}
	narrowed := embeddings.Limits{InputBytes: 8192, BatchInputs: 64, BatchBytes: 300_000}

	// A ROW REFUSED ALONE stays held across an apply that builds the
	// provider again the same.
	refusing := embeddings.NewFake(64)
	refusing.Refuse("poison")
	first := apply(refusing)
	if pass := send(first, refusing); len(pass.RefusedAlone) != 1 {
		t.Fatalf("the fixture refused %d inputs alone, want the poison note", len(pass.RefusedAlone))
	}
	same := embeddings.NewFake(64)
	same.Refuse("poison")
	if kept := apply(same); kept != first {
		t.Fatal("a provider built again with the same configuration started a new " +
			"refusal memory, so every held row is isolated again on every apply")
	}
	if pass := send(first, same); pass.Held != 1 || len(same.Requests()) != 0 {
		t.Fatalf("after an apply that changed nothing the poison note was held %d time(s) "+
			"and %d request(s) were sent, want it held and nothing sent", pass.Held,
			len(same.Requests()))
	}

	// CONFIGURED OTHERWISE, the memory starts again and the row is tried.
	narrower := embeddings.NewFake(64)
	narrower.SetLimits(narrowed)
	fresh := apply(narrower)
	if fresh == first || fresh.Len() != 0 {
		t.Fatal("a provider configured otherwise inherited the refusals of the one it replaced")
	}
	if pass := send(fresh, narrower); pass.Held != 0 || pass.Accepted != 1 {
		t.Fatalf("the row the old configuration refused was held %d time(s) and accepted %d, "+
			"want it sent and embedded at once", pass.Held, pass.Accepted)
	}

	// A CONFIGURATION CONCLUDED REFUSED stays paused across an apply that
	// builds it again the same, and is judged again once it changes.
	whole := embeddings.NewFake(64)
	whole.Refuse("") // every request, the canary's included
	concluded := apply(whole)
	if pass := send(concluded, whole); !pass.Concluded {
		t.Fatalf("the fixture did not conclude the configuration refused: %v", pass.Err())
	}
	again := embeddings.NewFake(64)
	again.Refuse("")
	if kept := apply(again); kept != concluded {
		t.Fatal("an apply that changed nothing about the embeddings lifted the pause on " +
			"a configuration the fill had concluded refused")
	}
	if pass := send(concluded, again); !pass.Paused || len(again.Requests()) != 0 {
		t.Fatalf("after an apply that changed nothing the pass was paused %v and sent %d "+
			"request(s), want it paused and nothing sent", pass.Paused, len(again.Requests()))
	}
	fixed := embeddings.NewFake(64)
	fixed.SetLimits(narrowed)
	if lifted := apply(fixed); lifted == concluded || send(lifted, fixed).Paused {
		t.Fatal("a provider configured otherwise stayed under the pause of the one it replaced")
	}
}
