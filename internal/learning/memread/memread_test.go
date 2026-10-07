package memread_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

var pinned = time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)

// node is one engine's worth of memory: its own store, its own incarnation.
type node struct {
	owner  string
	stores *memread.Stores
	db     *store.DB
}

func newNode(t *testing.T, owner string) *node {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "m.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &node{owner: owner, db: db, stores: &memread.Stores{
		Diary:         learning.NewDiary(db),
		Episodes:      learning.NewEpisodes(db),
		Skills:        learning.NewSkills(db),
		Profiles:      learning.NewCounterparties(db),
		Onboarding:    learning.NewOnboarding(db),
		Conversations: ledgerstore.NewConversations(db),
		// The derivation is the org's; any stable function of the handle
		// is one for a test.
		AgentID: func(handle string) string { return "agent-" + handle },
		Now:     func() time.Time { return pinned.Add(time.Hour) },
	}}
}

// remember writes n diary entries and n episodes for handle, their content
// naming who wrote them so an answer says whose store it came from.
func (n *node) remember(t *testing.T, handle, whose string, count int) {
	t.Helper()
	for i := range count {
		at := pinned.Add(time.Duration(i) * time.Second)
		if err := n.stores.Diary.Write(t.Context(), learning.DiaryEntry{
			ID: whose + "-d-" + strconv.Itoa(i), AgentID: "agent-" + handle,
			Kind: learning.DiaryLong, Content: whose + " note " + strconv.Itoa(i),
			Source: learning.PersistSource, CreatedAt: at,
		}); err != nil {
			t.Fatalf("diary: %v", err)
		}
		if _, err := n.stores.Episodes.Append(t.Context(), learning.Episode{
			ID: whose + "-ep-" + strconv.Itoa(i), Handle: handle, TurnID: "turn-" + strconv.Itoa(i),
			TaskSummary: whose + " episode", StartedAt: at, EndedAt: at.Add(time.Minute),
		}); err != nil {
			t.Fatalf("episode: %v", err)
		}
	}
}

// fleet is a broker and a lease table two nodes share.
type fleet struct {
	broker *memory.Broker
	leases *coordmemory.Backend
}

func newFleet() *fleet {
	return &fleet{broker: memory.NewBroker(), leases: coordmemory.New()}
}

// hold gives handle's lease to owner.
func (f *fleet) hold(t *testing.T, handle, owner string) {
	t.Helper()
	lease, _, err := f.leases.TryAcquire(t.Context(), coord.SeatResource(handle),
		coord.AcquireOptions{Owner: owner, TTL: time.Minute})
	if err != nil || lease == nil {
		t.Fatalf("acquire %s for %s: %v", handle, owner, err)
	}
}

// present claims owner's node presence lease, its heartbeat advertising
// features — none at all is an older build's. A node id's presence belongs to
// its NEWEST incarnation, so one an earlier incarnation held is given up first,
// as a restart does.
func (f *fleet) present(t *testing.T, owner string, features ...coord.Feature) {
	t.Helper()
	node := memread.NodeOf(owner)
	if was, err := f.leases.Get(t.Context(), coord.NodeResource(node)); err != nil {
		t.Fatalf("read the presence of %s: %v", node, err)
	} else if was != nil && was.Owner != owner {
		if _, err := f.leases.Release(t.Context(), coord.NodeResource(node), was.Owner, was.Epoch); err != nil {
			t.Fatalf("release %s's presence: %v", was.Owner, err)
		}
	}
	lease, _, err := f.leases.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
		Owner: owner, TTL: time.Minute, Preferred: node, Ungated: true,
		Meta: map[string]any{coord.StatusKey: coord.NodeStatus{Features: features}.Meta()},
	})
	if err != nil || lease == nil {
		t.Fatalf("presence of %s: %v", owner, err)
	}
}

// reader makes n an answerer on the fleet's broker, attached to the given
// seats and present on this build, and returns its reader.
func (f *fleet) reader(t *testing.T, n *node, attached ...string) *memread.Reader {
	t.Helper()
	q := f.broker.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	seats := func() []string { return attached }
	stop, err := memread.Serve(t.Context(), q, n.owner, seats, n.stores)
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
	f.present(t, n.owner, coord.Features...)
	return &memread.Reader{
		Owner: n.owner, Local: n.stores, Queue: q, Leases: f.leases, Attached: seats,
		Features: coord.FeatureReader{Leases: f.leases}, Budget: 500 * time.Millisecond,
	}
}

// counting records every read put on the broker, so a case can say one was
// never asked.
type counting struct {
	memread.Asker
	asked int
}

func (c *counting) Ask(ctx context.Context, subject string, request []byte, want int) ([][]byte, error) {
	c.asked++
	return c.Asker.Ask(ctx, subject, request, want)
}

// A NON-HOLDER READS THE HOLDER'S MEMORY, NOT ITS OWN COPY.
//
// Two nodes each keep a copy of @swe's memory: node-b's is what it hydrated
// the last time it ran the seat, node-a's is current because node-a holds it
// now. A read served by node-b is answered by node-a — the rows, the totals and
// the name of the node that answered — and node-b's stale copy appears nowhere.
// Answered locally (the mutation), node-b's two old notes come back as the
// seat's memory with its own name on them.
func TestANonHolderReadsTheHoldersMemory(t *testing.T) {
	t.Parallel()
	f := newFleet()
	a, b := newNode(t, "node-a:1"), newNode(t, "node-b:1")
	a.remember(t, "swe", "current", 5)
	b.remember(t, "swe", "stale", 2)
	f.hold(t, "swe", a.owner)
	f.reader(t, a, "swe")
	readB := f.reader(t, b)

	got, err := readB.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if got.HeldBy != "node-a" {
		t.Errorf("held_by = %q, want node-a — the node that holds the seat", got.HeldBy)
	}
	if got.DiaryTotal != 5 || got.EpisodesTotal != 5 || len(got.Diary) != 5 {
		t.Errorf("a diary of %d (total %d) and %d episodes, want the holder's 5 of each",
			len(got.Diary), got.DiaryTotal, got.EpisodesTotal)
	}
	for _, row := range got.Diary {
		if !strings.HasPrefix(row.Content, "current") {
			t.Errorf("the answer carries %q from a copy the holder does not keep", row.Content)
		}
	}
}

// A SEAT NO NODE HOLDS HAS NO CURRENT COPY, and none is shown.
//
// This node's disk has @swe's rows from the last time it ran the seat. They are
// real and of unknown age, and drawing them as the seat's memory is the stale
// answer this package exists to stop: the answer is empty and says why.
func TestASeatNobodyHoldsIsAnsweredEmptyAndSaysSo(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	b.remember(t, "swe", "stale", 3)
	read := f.reader(t, b)

	got, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if got.HeldBy != memread.HolderNone {
		t.Errorf("held_by = %q, want %q", got.HeldBy, memread.HolderNone)
	}
	if len(got.Diary) != 0 || got.DiaryTotal != 0 || got.LatestReflection != nil {
		t.Errorf("an unheld seat answered %d diary rows (total %d) from a copy nobody "+
			"keeps current", len(got.Diary), got.DiaryTotal)
	}
	if got.Diary == nil || got.Episodes == nil || got.Skills == nil || got.Counterparties == nil {
		t.Error("an empty answer omits a list rather than sending it empty")
	}
	threads, err := read.Threads(t.Context(), "swe", "", 0)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if threads.HeldBy != memread.HolderNone || len(threads.Conversations) != 0 {
		t.Errorf("threads of an unheld seat = %+v, want none and held_by none", threads)
	}
}

// A HOLDER THAT DOES NOT ANSWER IS AN UNKNOWN, never an empty memory.
//
// The lease names an incarnation that serves nothing — gone, or on a build
// that cannot answer. The read fails as unavailable, which a screen retries;
// answering "this seat remembers nothing" would be a claim nobody made.
func TestASilentHolderIsUnavailableNotEmpty(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	f.hold(t, "swe", "node-c:9")
	// ITS BUILD SAYS IT ANSWERS, and it serves nothing: the silence is the
	// case, not the build.
	f.present(t, "node-c:9", coord.FeatureHeldRead)
	read := f.reader(t, b)

	_, err := read.Memory(t.Context(), "swe", 0)
	if !errors.Is(err, memread.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "node-c") {
		t.Errorf("err = %v — it should name the node that did not answer", err)
	}
}

// A HOLDER ON AN OLDER BUILD IS NAMED AS ONE, AT ONCE — and never asked.
//
// Mid rolling upgrade, @swe is held by a node whose build serves no held-read
// subject. Asking it anyway waits out the whole budget on every poll and then
// reports a holder that "did not answer", which reads as a node in trouble.
// The read is unavailable immediately, says the holder's build is older, and
// puts nothing on the broker. Asked regardless (the mutation), the budget —
// ten seconds here — is spent and the test's clock catches it.
func TestAHolderOnAnOlderBuildIsNamedAtOnceAndNeverAsked(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	f.hold(t, "swe", "node-c:9")
	f.present(t, "node-c:9") // an older build: advertises nothing
	read := f.reader(t, b)
	read.Budget = 10 * time.Second
	asks := &counting{Asker: read.Queue}
	read.Queue = asks

	start := time.Now()
	_, err := read.Memory(t.Context(), "swe", 0)
	if !errors.Is(err, memread.ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if !strings.Contains(err.Error(), "node-c") || !strings.Contains(err.Error(), "older build") {
		t.Errorf("err = %v — it should name the holder and say its build is older", err)
	}
	if asks.asked != 0 {
		t.Errorf("asked the broker %d times — a build that cannot answer is not asked", asks.asked)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %s — the answer is known without waiting on the holder", took)
	}
	if _, err := read.Threads(t.Context(), "swe", "", 0); !errors.Is(err, memread.ErrUnavailable) {
		t.Errorf("threads: err = %v, want ErrUnavailable", err)
	}
}

// A HOLDER WHOSE BUILD NOTHING DESCRIBES IS UNKNOWN, NOT OLDER.
//
// The lease names an incarnation with no presence lease — mid-drain, or its
// heartbeat lapsed. Nothing says what it can do, so the read is unavailable to
// be tried again, and it does not blame an upgrade that may not be happening.
func TestAHolderWithNoPresenceIsUnknownNotOlder(t *testing.T) {
	t.Parallel()
	f := newFleet()
	b := newNode(t, "node-b:1")
	f.hold(t, "swe", "node-c:9")
	read := f.reader(t, b)

	_, err := read.Memory(t.Context(), "swe", 0)
	if !errors.Is(err, memread.ErrUnavailable) || !errors.Is(err, coord.ErrFeatureUnknown) {
		t.Fatalf("err = %v, want ErrUnavailable wrapping coord.ErrFeatureUnknown", err)
	}
	if strings.Contains(err.Error(), "older build") {
		t.Errorf("err = %v — a holder nothing describes is not known to be older", err)
	}
}

// A HOLDER STILL TAKING THE SEAT IS NOT YET CURRENT, on this node or another.
//
// A seat is hydrated before its mailbox attaches, and between the two its
// memory is still arriving. Asked of itself or of a peer, that node refuses
// rather than answering a page that is short.
func TestAHolderStillTakingTheSeatRefuses(t *testing.T) {
	t.Parallel()
	f := newFleet()
	a, b := newNode(t, "node-a:1"), newNode(t, "node-b:1")
	a.remember(t, "swe", "arriving", 1)
	f.hold(t, "swe", a.owner)
	readA := f.reader(t, a) // holds the lease, has not attached
	readB := f.reader(t, b)

	for name, read := range map[string]*memread.Reader{"itself": readA, "a peer": readB} {
		if _, err := read.Memory(t.Context(), "swe", 0); !errors.Is(err, memread.ErrUnavailable) {
			t.Errorf("asked by %s: err = %v, want ErrUnavailable", name, err)
		}
	}
}

// ONLY THE INCARNATION THE LEASE NAMES ANSWERS.
//
// node-a restarted: its old incarnation still serves (a process mid-teardown)
// and still has the seat in its attached set. The lease names the new one, and
// the read is its answer — the old one's copy is from a tenure that ended.
func TestOnlyTheIncarnationTheLeaseNamesAnswers(t *testing.T) {
	t.Parallel()
	f := newFleet()
	old, fresh, asker := newNode(t, "node-a:old"), newNode(t, "node-a:new"), newNode(t, "node-b:1")
	old.remember(t, "swe", "old", 1)
	fresh.remember(t, "swe", "new", 2)
	f.hold(t, "swe", fresh.owner)
	f.reader(t, old, "swe")
	f.reader(t, fresh, "swe")
	read := f.reader(t, asker)

	got, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if got.DiaryTotal != 2 || len(got.Diary) == 0 || !strings.HasPrefix(got.Diary[0].Content, "new") {
		t.Errorf("answered %+v, want the new incarnation's two notes", got.Diary)
	}
}

// A NODE WITH NO BROKER IS THE FLEET: its store is the only copy there is.
func TestANodeWithNoBrokerAnswersItself(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	n.remember(t, "swe", "only", 2)
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if got.HeldBy != "solo" || got.DiaryTotal != 2 {
		t.Errorf("held_by %q with %d notes, want solo with 2", got.HeldBy, got.DiaryTotal)
	}
}

// THE TOTALS ARE THE SEAT'S WHOLE SET, NOT THE PAGE.
//
// A panel that counted the rows it was sent reported the page size for exactly
// the seats whose memory had outgrown it. Every collection carries its count
// beside a page that is cut, and the latest reflection is the newest entry
// whatever the page — a page of one included.
func TestEveryCollectionCarriesItsTotalBesideItsPage(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	held := memread.PageLimit + 7
	n.remember(t, "swe", "seat", held)
	for i := range held {
		name := "skill-" + strconv.Itoa(i)
		if err := n.stores.Skills.Insert(t.Context(), learning.Skill{
			ID: name, AgentHandle: "swe", Name: name, Description: "drafted",
			CreatedAt: pinned, UpdatedAt: pinned,
		}); err != nil {
			t.Fatalf("skill: %v", err)
		}
		if _, err := n.stores.Profiles.(*learning.Counterparties).Record(t.Context(), learning.Observation{
			Observer: "swe", Subject: learning.Subject{Handle: "peer-" + strconv.Itoa(i), Name: name},
			At: pinned.Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("profile: %v", err)
		}
	}
	read := &memread.Reader{Owner: n.owner, Local: n.stores}

	page, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	for name, got := range map[string][2]int{
		"diary":          {len(page.Diary), page.DiaryTotal},
		"episodes":       {len(page.Episodes), page.EpisodesTotal},
		"skills":         {len(page.Skills), page.SkillsTotal},
		"counterparties": {len(page.Counterparties), page.CounterpartiesTotal},
	} {
		if got[0] != memread.PageLimit || got[1] != held {
			t.Errorf("%s: a page of %d with a total of %d, want %d of %d",
				name, got[0], got[1], memread.PageLimit, held)
		}
	}

	one, err := read.Memory(t.Context(), "swe", 1)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if len(one.Diary) != 1 || one.DiaryTotal != held {
		t.Errorf("a page of one carries %d of %d", len(one.Diary), one.DiaryTotal)
	}
	newest := "seat note " + strconv.Itoa(held-1)
	if one.LatestReflection == nil || one.LatestReflection.Content != newest {
		t.Errorf("latest reflection = %+v, want the newest entry %q", one.LatestReflection, newest)
	}
}

// ONBOARDED MEANS FINISHED, and the instant is when the seat FIRST onboarded.
//
// A claimed pass writes the marker row with no chain, which is not an
// onboarding; `onboarded_at` stays empty until a pass marks it, and a later
// re-onboarding keeps the first instant.
func TestOnboardedAtIsSetOnlyOnceAPassFinished(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	if _, err := n.stores.Onboarding.Claim(t.Context(), "agent-swe", pinned, time.Minute); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	claimed, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if claimed.OnboardedAt != "" {
		t.Errorf("onboarded_at = %q for a pass claimed and never finished", claimed.OnboardedAt)
	}
	if err := n.stores.Onboarding.Mark(t.Context(), learning.Marker{
		AgentID: "agent-swe", ChainHash: "chain-1", Handle: "swe",
	}, pinned.Add(time.Minute)); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	marked, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if marked.OnboardedAt == "" {
		t.Fatal("onboarded_at is empty for a seat whose pass finished")
	}
	if got, _ := time.Parse(time.RFC3339Nano, marked.OnboardedAt); got.After(pinned.Add(time.Minute)) {
		t.Errorf("onboarded_at = %s, after the pass that marked it", marked.OnboardedAt)
	}
}

// A CONVERSATION LISTING SAYS WHAT IT WAS CUT FROM, and a named thread's
// entries come back beside it.
func TestThreadsCarryTheirTotalAndTheNamedThread(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	for i := range 4 {
		key := "slack:C" + strconv.Itoa(i)
		if err := n.stores.Conversations.Append(t.Context(), "swe", key,
			ledger.Session{Reply: "said in " + key}, key, pinned.Add(time.Duration(i)*time.Minute), 0); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := read.Threads(t.Context(), "swe", "slack:C2", 2)
	if err != nil {
		t.Fatalf("Threads: %v", err)
	}
	if len(got.Conversations) != 2 || got.ConversationsTotal != 4 {
		t.Errorf("%d threads of %d, want 2 of 4", len(got.Conversations), got.ConversationsTotal)
	}
	if len(got.Entries) != 1 || got.Entries[0].Reply != "said in slack:C2" {
		t.Errorf("entries = %+v, want the one recorded in slack:C2", got.Entries)
	}
	if got.HeldBy != "solo" {
		t.Errorf("held_by %q, want solo", got.HeldBy)
	}
}

// EVERY KEY ON EVERY ANSWER. A client cannot tell "this seat has learned
// nothing" from "this answer does not carry that half" if the key is simply
// not there, and both are ordinary states — so an answer about a seat with no
// memory at all sends every list empty and every total zero.
func TestAnEmptyMemoryCarriesEveryKey(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := read.Memory(t.Context(), "nobody", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"diary", "episodes", "skills", "counterparties"} {
		if list, ok := body[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s = %#v, want an empty list", key, body[key])
		}
	}
	for _, key := range []string{"diary_total", "episodes_total", "skills_total",
		"counterparties_total", "onboarded_at", "held_by"} {
		if _, ok := body[key]; !ok {
			t.Errorf("the answer omits %s", key)
		}
	}
	if v, ok := body["latest_reflection"]; !ok || v != nil {
		t.Errorf("latest_reflection = %#v, want present and null", v)
	}
}

// THE PROFILES ARE READ A PAGE AT A TIME, in the store. The half-page this
// read draws was cut from the store's whole listing — two hundred trait bags
// read to keep the Overview's one row, on a poll.
func TestTheProfilesAreAskedForThePageAndNoMore(t *testing.T) {
	t.Parallel()
	asked := &askedProfiles{}
	local := &memread.Stores{Profiles: asked}
	for _, c := range []struct{ page, want int }{{1, 1}, {0, memread.PageLimit}, {500, memread.PageLimit}} {
		if _, err := local.Memory(t.Context(), "swe", c.page); err != nil {
			t.Fatalf("Memory: %v", err)
		}
		if asked.limit != c.want {
			t.Errorf("a memory page of %d asked the profiles for %d, want %d", c.page, asked.limit, c.want)
		}
	}
}

// askedProfiles records the page it was asked for.
type askedProfiles struct{ limit int }

func (a *askedProfiles) List(_ context.Context, _ string, limit int) ([]learning.Profile, error) {
	a.limit = limit
	return nil, nil
}

func (a *askedProfiles) Count(context.Context, string) (int, error) { return 0, nil }

// stubProfiles answers fixtures, or fails.
type stubProfiles struct {
	profiles []learning.Profile
	err      error
}

func (s stubProfiles) List(_ context.Context, _ string, limit int) ([]learning.Profile, error) {
	return s.profiles[:min(limit, len(s.profiles))], s.err
}

func (s stubProfiles) Count(context.Context, string) (int, error) { return len(s.profiles), s.err }

// WHAT A SEAT LEARNED ABOUT A COLLEAGUE, as a screen reads it: both instants —
// they measure different cadences, and the gap between them is what says this
// seat has stopped learning about somebody it still works with — whether the
// subject is a seat of this company, and each identity only for its own kind.
func TestAProfileCarriesBothInstantsAndOneIdentity(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	seen := pinned.Add(-24 * time.Hour)
	n.stores.Profiles = stubProfiles{profiles: []learning.Profile{
		{
			Observer: "swe", Subject: learning.Subject{Handle: "cto", Name: "Cy"},
			Traits: map[string]any{"prefers": "async"}, InteractionCount: 7,
			FirstSeenAt: seen, LastUpdatedAt: pinned, LastCorroboratedAt: seen,
		},
		{
			Observer: "swe", InteractionCount: 1,
			Subject:     learning.Subject{ExternalID: "U0FOUNDER", Platform: "slack", Name: "Ada"},
			FirstSeenAt: seen, LastUpdatedAt: seen, LastCorroboratedAt: seen,
		},
	}}
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := read.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if len(got.Counterparties) != 2 || got.CounterpartiesTotal != 2 {
		t.Fatalf("%d profiles of %d, want 2 of 2", len(got.Counterparties), got.CounterpartiesTotal)
	}
	seat, person := got.Counterparties[0], got.Counterparties[1]
	if !seat.Resolved || seat.Subject.Handle != "cto" || seat.Subject.ExternalID != "" {
		t.Errorf("a seat subject = %+v, want resolved with only its handle", seat)
	}
	if person.Resolved || person.Subject.ExternalID != "U0FOUNDER" || person.Subject.Platform != "slack" {
		t.Errorf("a person on a surface = %+v, want unresolved with its platform identity", person)
	}
	if !seat.LastUpdatedAt.Equal(pinned) || !seat.LastCorroboratedAt.Equal(seen) ||
		seat.Interactions != 7 {
		t.Errorf("a profile's instants = %+v, want both carried as recorded", seat)
	}
	if person.Traits == nil {
		t.Error("a profile with no traits carries null rather than an empty map")
	}
}

// AN ERROR IS NOT AN EMPTY LIST. Everything in `learning` is best effort for a
// TURN, which must not die because a store was slow; a screen reporting "this
// seat has worked with nobody" when the store could not be reached is the
// collapse the three-valued answers exist to prevent.
func TestAFailedProfileReadIsNotAnEmptyList(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	sentinel := errors.New("the store could not be reached")
	n.stores.Profiles = stubProfiles{err: sentinel}
	read := &memread.Reader{Owner: n.owner, Local: n.stores}
	if _, err := read.Memory(t.Context(), "swe", 0); !errors.Is(err, sentinel) {
		t.Errorf("a failed profile read answered %v, want the failure", err)
	}
}

// THE PAGES ARE BOUNDED IN BOTH DIRECTIONS. The ledger applies a LIMIT only
// when one is positive, so an absent or negative page would read a seat's
// whole ledger — a thread per channel of a busy workspace — through the
// process; and a memory page past the limit would carry a store's worth of
// rows to a panel that draws fifty.
func TestThePagesAreBoundedInBothDirections(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name        string
		asked       int
		memory, led int
	}{
		{"absent or zero", 0, memread.PageLimit, memread.DefaultThreadPage},
		{"negative", -1, memread.PageLimit, memread.DefaultThreadPage},
		{"inside", 7, 7, 7},
		{"past the memory page", memread.PageLimit + 1, memread.PageLimit, memread.PageLimit + 1},
		{"past the ledger's ceiling", memread.MaxThreadPage + 1, memread.PageLimit, memread.MaxThreadPage},
	} {
		if got := memread.Page(c.asked); got != c.memory {
			t.Errorf("%s: a memory page of %d, want %d", c.name, got, c.memory)
		}
		if got := memread.ThreadPage(c.asked); got != c.led {
			t.Errorf("%s: a ledger page of %d, want %d", c.name, got, c.led)
		}
	}
}

// THE SKILL PAGE IS TAKEN IN THE STORE rather than out of a fully read library.
//
// A skill row carries its content and frontmatter, so reading the seat's whole
// catalogue to show a page of it is real I/O. The bound is
// [learning.ListOptions.Limit], which the SQL honours — and its zero value is
// the unbounded setting every non-paging caller (the prefetch's offer, the
// refiner) relies on, so a bound that leaked into the default would silently
// truncate all of them. Both are asserted.
func TestTheSkillPageIsBoundedInTheStore(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	held := memread.PageLimit + 7
	for i := range held {
		name := "skill-" + strconv.Itoa(i)
		if err := n.stores.Skills.Insert(t.Context(), learning.Skill{
			ID: name, AgentHandle: "swe", Name: name, Description: "drafted",
			Content:   strings.Repeat("a body a page never renders. ", 64),
			CreatedAt: pinned, UpdatedAt: pinned,
		}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	page, err := n.stores.Skills.List(t.Context(), "swe", learning.ListOptions{Limit: memread.PageLimit})
	if err != nil {
		t.Fatalf("bounded listing: %v", err)
	}
	if len(page) != memread.PageLimit {
		t.Errorf("a listing bounded to %d read %d", memread.PageLimit, len(page))
	}
	whole, err := n.stores.Skills.List(t.Context(), "swe", learning.ListOptions{})
	if err != nil {
		t.Fatalf("unbounded listing: %v", err)
	}
	if len(whole) != held {
		t.Errorf("the zero Limit read %d of %d: it is the unbounded setting", len(whole), held)
	}
}

// AN EPISODE ROW SAYS WHAT IT IS. A raw row carries what woke the turn, what it
// was asked and what it did under their own names; a compacted row carries its
// pattern, how many of its turns ended done and what varied — the three things
// query_episodes answers a seat with, so the screen is not left drawing a
// compacted row as a turn with no summary.
func TestAnEpisodeRowSaysWhatItIs(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	for _, ep := range []learning.Episode{{
		ID: "raw", Handle: "swe", TurnID: "turn-1", WorkKey: "wk-raw", Kind: learning.KindRaw,
		TaskSummary: "Message from Ana: Slack message", Ask: "The staging deploy keeps failing.",
		PlanSummary: "Rolled staging back to v41.", ReviewOutcome: "done",
		StartedAt: pinned, EndedAt: pinned.Add(time.Minute),
	}, {
		ID: "folded", Handle: "swe", WorkKey: "wk-folded", Kind: learning.KindCompacted, Count: 12,
		CommonTaskPattern: "Triaging a failed staging deploy", SuccessRate: 0.75,
		NotablePatterns: "Two went to the SRE lead.", ReviewOutcome: "done",
		StartedAt: pinned.Add(-time.Hour), EndedAt: pinned,
	}} {
		if _, err := n.stores.Episodes.Append(t.Context(), ep); err != nil {
			t.Fatalf("episode %s: %v", ep.ID, err)
		}
	}
	got, err := (&memread.Reader{Owner: n.owner, Local: n.stores}).Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	rows := map[string]memread.EpisodeRow{}
	for _, row := range got.Episodes {
		rows[row.ID] = row
	}
	raw, folded := rows["raw"], rows["folded"]
	if raw.TaskSummary != "Message from Ana: Slack message" || raw.Ask != "The staging deploy keeps failing." ||
		raw.PlanSummary != "Rolled staging back to v41." || raw.Compaction != nil {
		t.Errorf("the raw row = %+v, want its label, its ask and what it did, and no compaction", raw)
	}
	if c := folded.Compaction; !folded.Compacted || folded.Count != 12 || c == nil ||
		c.CommonTaskPattern != "Triaging a failed staging deploy" || c.Done != 9 ||
		c.NotablePatterns != "Two went to the SRE lead." {
		t.Errorf("the compacted row = %+v (%+v), want its pattern, 9 of 12 done and what varied",
			folded, folded.Compaction)
	}
}

// A COMPACTED ROW FROM A HOLDER THAT DOES NOT SAY WHAT IT FOLDED SAYS NOTHING
// ABOUT IT. A holder on a build from before the compaction was sent answers a
// compacted row with none of its pattern, its tally or what varied. Decoded into
// three plain fields they were "no pattern" and "0 of 12 done" — statements
// about the data, on a screen re-serving the row — where the truth was that the
// holder did not say; as one object they are absent, and re-served as null.
func TestACompactedRowFromAnOlderHolderCarriesNoCompaction(t *testing.T) {
	t.Parallel()
	// The row as such a build sends it: every key it knew, and no others.
	older := []byte(`{"id":"folded","turn_id":"","agent_handle":"swe",
		"task_summary":"","plan_summary":"","review_outcome":"done",
		"tool_sequence":["read","page"],"skills_used":null,"conversation_key":"",
		"work_key":"wk","created_at":"2026-09-01T07:00:00Z",
		"ended_at":"2026-09-01T08:00:00Z","duration_ms":0,"compacted":true,"count":12}`)
	var row memread.EpisodeRow
	if err := json.Unmarshal(older, &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !row.Compacted || row.Count != 12 || row.Compaction != nil {
		t.Fatalf("the older holder's row decoded as %+v with compaction %+v, want a "+
			"compacted row of 12 that says nothing about what it folded", row, row.Compaction)
	}
	served, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(served, &keys); err != nil {
		t.Fatalf("decode the re-served row: %v", err)
	}
	if got, ok := keys["compaction"]; !ok || string(got) != "null" {
		t.Fatalf("the re-served row carries compaction %s (present %v), want null", got, ok)
	}
	for _, key := range []string{"common_task_pattern", "done", "notable_patterns"} {
		if _, ok := keys[key]; ok {
			t.Errorf("the re-served row carries %q at its top level, which reads as the "+
				"compaction's own word", key)
		}
	}
}

// A LISTED EPISODE CARRIES THE OPENINGS OF ITS LONG TEXTS, AND ONE READ HAS THEM
// WHOLE. An episode's ask is bounded only by the event that delivered it and its
// account by nothing, so a listed row carries each as its opening — cut on a
// rune boundary, never inside a character — with the size of the whole beside
// it, and the `episode` read returns the row with both complete. An id the seat
// does not hold, or another seat's, is no episode rather than a failure.
func TestAListedEpisodeCarriesItsOpeningsAndOneReadHasItWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	ask := strings.Repeat("ünïcode in a pasted log line; ", 200)
	account := strings.Repeat("rolled back and filed the regression. ", 80)
	for _, ep := range []learning.Episode{{
		ID: "long", Handle: "swe", TurnID: "turn-1", WorkKey: "wk-long", Kind: learning.KindRaw,
		TaskSummary: "Message from Ana: Slack message", Ask: ask, PlanSummary: account,
		ReviewOutcome: "done", StartedAt: pinned, EndedAt: pinned.Add(time.Minute),
	}, {
		ID: "theirs", Handle: "cto", TurnID: "turn-2", WorkKey: "wk-theirs", Kind: learning.KindRaw,
		TaskSummary: "another seat's turn", StartedAt: pinned, EndedAt: pinned.Add(time.Minute),
	}} {
		if _, err := n.stores.Episodes.Append(t.Context(), ep); err != nil {
			t.Fatalf("episode %s: %v", ep.ID, err)
		}
	}
	reader := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := reader.Memory(t.Context(), "swe", 0)
	if err != nil || len(got.Episodes) != 1 {
		t.Fatalf("Memory = %d episodes, %v", len(got.Episodes), err)
	}
	row := got.Episodes[0]
	if len(row.Ask) > memread.ListedTextBytes || !utf8.ValidString(row.Ask) ||
		!strings.HasPrefix(ask, row.Ask) || row.AskBytes != len(ask) {
		t.Fatalf("the listed ask is %d bytes (valid %v) of a %d-byte text named %d, want "+
			"its opening of at most %d bytes, whole characters, and the whole size",
			len(row.Ask), utf8.ValidString(row.Ask), len(ask), row.AskBytes, memread.ListedTextBytes)
	}
	if len(row.PlanSummary) > memread.ListedTextBytes || !strings.HasPrefix(account, row.PlanSummary) ||
		row.PlanSummaryBytes != len(account) {
		t.Fatalf("the listed account is %d bytes of %d named %d, want its opening and the "+
			"whole size", len(row.PlanSummary), len(account), row.PlanSummaryBytes)
	}

	whole, err := reader.Episode(t.Context(), "swe", "long")
	if err != nil || whole.Episode == nil {
		t.Fatalf("Episode = %+v, %v", whole, err)
	}
	if e := whole.Episode; e.Ask != ask || e.PlanSummary != account ||
		e.AskBytes != len(ask) || e.PlanSummaryBytes != len(account) || whole.HeldBy != "solo" {
		t.Fatalf("the episode read whole carries a %d-byte ask and a %d-byte account (held "+
			"by %q), want both whole", len(e.Ask), len(e.PlanSummary), whole.HeldBy)
	}
	for _, id := range []string{"gone", "theirs"} {
		if absent, err := reader.Episode(t.Context(), "swe", id); err != nil || absent.Episode != nil {
			t.Errorf("reading %q of swe = %+v, %v; want no episode and no failure", id, absent, err)
		}
	}
}

// WHAT A SEAT IS SHOWN WHOLE IS LISTED WHOLE. The screen's opening is the
// prompt's own figure (learning.EpisodeAccountBytes): an ask exactly that long
// is one a recalled turn shows a seat whole — no rewrite asked for — and the
// listing carries it whole too; a byte longer, the prompt condenses it and the
// listing opens it. Two figures, they drift: a screen cutting what every seat
// reads whole, or listing whole what no seat was ever shown.
func TestWhatASeatIsShownWholeIsListedWhole(t *testing.T) {
	t.Parallel()
	n := newNode(t, "solo:1")
	at := learning.EpisodeAccountBytes
	shown := strings.Repeat("x", at)
	turns := []learning.Episode{{
		ID: "shown", Handle: "swe", TurnID: "turn-1", WorkKey: "wk-shown", Kind: learning.KindRaw,
		TaskSummary: "a message", Ask: shown, StartedAt: pinned, EndedAt: pinned.Add(time.Minute),
	}, {
		ID: "condensed", Handle: "swe", TurnID: "turn-2", WorkKey: "wk-condensed", Kind: learning.KindRaw,
		TaskSummary: "a message", Ask: shown + "y", StartedAt: pinned, EndedAt: pinned.Add(2 * time.Minute),
	}}
	for _, ep := range turns {
		if _, err := n.stores.Episodes.Append(t.Context(), ep); err != nil {
			t.Fatalf("episode %s: %v", ep.ID, err)
		}
	}
	// The zero binding can rewrite nothing: a text the prompt asks it to
	// condense would fail, and one shown whole never reaches it.
	if prompt := learning.PastTurns(t.Context(), turns[:1], compact.Bound{}, at); prompt[0].Ask != shown {
		t.Fatalf("the prompt showed a %d-byte ask as %q, want it whole", at, prompt[0].Ask)
	}
	reader := &memread.Reader{Owner: n.owner, Local: n.stores}
	got, err := reader.Memory(t.Context(), "swe", 0)
	if err != nil || len(got.Episodes) != 2 {
		t.Fatalf("Memory = %d episodes, %v", len(got.Episodes), err)
	}
	listed := map[string]memread.EpisodeRow{}
	for _, row := range got.Episodes {
		listed[row.ID] = row
	}
	if row := listed["shown"]; row.Ask != shown || row.AskBytes != at {
		t.Fatalf("the ask a seat is shown whole was listed as %d of %d bytes", len(row.Ask), row.AskBytes)
	}
	if row := listed["condensed"]; len(row.Ask) >= row.AskBytes || row.AskBytes != at+1 {
		t.Fatalf("an ask the prompt condenses was listed as %d of %d bytes, want its opening",
			len(row.Ask), row.AskBytes)
	}
}

// A PAGE OF LONG ASKS IS NOT REFUSED BY THE TRANSPORT. Fifty turns woken by
// large pasted logs were fifty whole asks in one reply, past what the transport
// carries, and the holder refused the whole memory read — the screen showed the
// seat unavailable for as long as those were its newest turns. Listed as their
// openings, the page is a few dozen kilobytes.
func TestAPageOfLongAsksIsAnsweredByItsHolder(t *testing.T) {
	t.Parallel()
	f := newFleet()
	a, b := newNode(t, "node-a:1"), newNode(t, "node-b:1")
	ask := strings.Repeat("x", queue.MaxPayloadBytes/memread.PageLimit+1)
	for i := range memread.PageLimit {
		at := pinned.Add(time.Duration(i) * time.Second)
		if _, err := a.stores.Episodes.Append(t.Context(), learning.Episode{
			ID: "ep-" + strconv.Itoa(i), Handle: "swe", TurnID: "turn-" + strconv.Itoa(i),
			WorkKey: "wk-" + strconv.Itoa(i), TaskSummary: "a pasted log", Ask: ask,
			StartedAt: at, EndedAt: at.Add(time.Minute),
		}); err != nil {
			t.Fatalf("episode: %v", err)
		}
	}
	f.hold(t, "swe", a.owner)
	f.reader(t, a, "swe")
	readB := f.reader(t, b)
	got, err := readB.Memory(t.Context(), "swe", 0)
	if err != nil {
		t.Fatalf("a page of %d asks of %d bytes each was refused: %v", memread.PageLimit,
			len(ask), err)
	}
	if len(got.Episodes) != memread.PageLimit || got.Episodes[0].AskBytes != len(ask) {
		t.Fatalf("the page carried %d episodes, the first naming a %d-byte ask; want all %d "+
			"and the whole size", len(got.Episodes), got.Episodes[0].AskBytes, memread.PageLimit)
	}
}
