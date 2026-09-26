package transfer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// member is one data node: its own disk, serving on the shared broker, and
// able to fall silent the way a node that died does.
type member struct {
	name   string
	disk   *disk.Store
	silent atomic.Bool
}

// fleet is some members serving on one broker, and the map they are placed by.
type fleet struct {
	t       *testing.T
	broker  *memory.Broker
	members map[string]*member
	m       placement.Map
}

func newFleet(t *testing.T, replicas int, names ...string) *fleet {
	t.Helper()
	f := &fleet{t: t, broker: memory.NewBroker(), members: map[string]*member{}}
	f.m = placement.Map{Epoch: 1, Replicas: replicas}
	for _, name := range slices.Sorted(slices.Values(names)) {
		f.m.Members = append(f.m.Members, placement.Member{Node: name, Weight: 1})
	}
	for _, name := range names {
		store, err := disk.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		mem := &member{name: name, disk: store}
		f.members[name] = mem
		stop, err := Serve(t.Context(), silencer{q: f.queue(), m: mem}, name, store, f.maps)
		if err != nil {
			t.Fatalf("serve %s: %v", name, err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	return f
}

func (f *fleet) maps() (placement.Map, bool) { return f.m, true }

func (f *fleet) queue() *memory.Queue {
	q := f.broker.Client()
	if err := q.Start(f.t.Context()); err != nil {
		f.t.Fatalf("start: %v", err)
	}
	f.t.Cleanup(func() { _ = q.Stop(context.Background()) })
	return q
}

// client is a node's client: a member's own when self names one, a stateless
// node's otherwise.
func (f *fleet) client(self string) *Client {
	f.t.Helper()
	opts := ClientOptions{Queue: f.queue(), Self: self, Maps: f.maps}
	if m, ok := f.members[self]; ok {
		opts.Local = m.disk
	}
	c, err := NewClient(opts)
	if err != nil {
		f.t.Fatal(err)
	}
	c.attempt = 200 * time.Millisecond
	return c
}

// holders is every member holding h.
func (f *fleet) holders(h objstore.Hash) []string {
	var out []string
	for name, m := range f.members {
		if m.disk.Has(h) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

type silencer struct {
	q *memory.Queue
	m *member
}

func (s silencer) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return s.q.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		if s.m.silent.Load() {
			return nil, errors.New("silent")
		}
		return h(ctx, raw)
	})
}

// A WRITE LANDS ON THE CHUNK'S UP SET AND NOWHERE ELSE, and a node holding
// nothing reads it back.
func TestAWriteLandsOnItsUpSetAndReadsBackAnywhere(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c", "data-d")
	writer := f.client("agent-1")
	for i := range 20 {
		data := []byte{byte(i), 'x'}
		h := objstore.HashOf(data)
		stored, err := writer.Put(t.Context(), h, data)
		if err != nil || stored != 2 {
			t.Fatalf("Put = %d, %v; want 2 copies", stored, err)
		}
		want := slices.Sorted(slices.Values(f.m.Up(h.PG())))
		if got := f.holders(h); !slices.Equal(got, want) {
			t.Fatalf("chunk %d is on %v, its up set is %v", i, got, want)
		}
		got, err := f.client("agent-2").Get(t.Context(), h)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get = %q, %v", got, err)
		}
	}
}

// A HOLDER THAT DOES NOT ANSWER COSTS A COPY IN ANOTHER PLACE, NOT A COPY
// FEWER — and a reader walking the same ranking finds it there.
func TestASilentHolderIsReplacedByTheNextMemberInTheRanking(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c", "data-d")
	data := []byte("the displaced write")
	h := objstore.HashOf(data)
	ranked := f.m.Ranked(h.PG())
	f.members[ranked[0]].silent.Store(true)

	stored, err := f.client("agent-1").Put(t.Context(), h, data)
	if err != nil || stored != 2 {
		t.Fatalf("Put = %d, %v; want 2 copies despite the silent primary", stored, err)
	}
	want := slices.Sorted(slices.Values([]string{ranked[1], ranked[2]}))
	if got := f.holders(h); !slices.Equal(got, want) {
		t.Fatalf("the chunk is on %v, want the next two in the ranking %v", got, want)
	}
	if got, err := f.client("agent-2").Get(t.Context(), h); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get with the primary silent = %q, %v", got, err)
	}
}

// FEWER THAN A QUORUM IS AN ERROR, never a success with a footnote: the record
// that would name this chunk cannot rely on one disk.
func TestFewerThanAQuorumIsAnError(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 3, "data-a", "data-b", "data-c")
	f.members["data-b"].silent.Store(true)
	f.members["data-c"].silent.Store(true)
	data := []byte("nowhere to go")
	stored, err := f.client("agent-1").Put(t.Context(), objstore.HashOf(data), data)
	if !errors.Is(err, ErrUnderReplicated) || stored != 1 {
		t.Fatalf("Put = %d, %v; want 1 copy and ErrUnderReplicated", stored, err)
	}

	// A majority of three is two: one silent member is not an error.
	f.members["data-c"].silent.Store(false)
	stored, err = f.client("agent-1").Put(t.Context(), objstore.HashOf([]byte("two of three")), []byte("two of three"))
	if err != nil || stored != 2 {
		t.Fatalf("Put with one member silent = %d, %v; want 2 copies", stored, err)
	}
}

// A FLEET WITH NO MAP HAS NOWHERE TO PUT ANYTHING, and says so by name.
func TestNoMapIsRefusedByName(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	c, err := NewClient(ClientOptions{Queue: f.queue(), Self: "agent-1",
		Maps: func() (placement.Map, bool) { return placement.Map{}, false }})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("x")
	if _, err := c.Put(t.Context(), objstore.HashOf(data), data); !errors.Is(err, ErrNoMap) {
		t.Fatalf("Put with no map = %v, want ErrNoMap", err)
	}
	if _, err := c.Get(t.Context(), objstore.HashOf(data)); !errors.Is(err, ErrNoMap) {
		t.Fatalf("Get with no map = %v, want ErrNoMap", err)
	}
}

// A MISSING MAP IS RE-READ BEFORE A REQUEST IS REFUSED. A fleet's first map is
// written moments after it boots, and a node whose last read came just before
// would otherwise refuse every upload until its next refresh — which is how a
// seat's first file failed in internal/e2e. A held map is never re-read here:
// an older map is correct until a newer one arrives.
func TestAMissingMapIsReadAgainBeforeARefusal(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	var held atomic.Bool
	var reads atomic.Int32
	maps := func() (placement.Map, bool) {
		if held.Load() {
			return f.m, true
		}
		return placement.Map{}, false
	}
	c, err := NewClient(ClientOptions{Queue: f.queue(), Self: "agent-1", Maps: maps,
		Refresh: func(context.Context) error {
			reads.Add(1)
			held.Store(true)
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("the first file this fleet stores")
	if _, err := c.Put(t.Context(), objstore.HashOf(data), data); err != nil {
		t.Fatalf("Put after the map was written = %v", err)
	}
	if _, err := c.Get(t.Context(), objstore.HashOf(data)); err != nil {
		t.Fatalf("Get = %v", err)
	}
	if got := reads.Load(); got != 1 {
		t.Errorf("the map was re-read %d times; once on the miss and never while held", got)
	}

	// A RE-READ THAT FAILS is still the refusal by name, carrying why.
	broken, err := NewClient(ClientOptions{Queue: f.queue(), Self: "agent-2",
		Maps:    func() (placement.Map, bool) { return placement.Map{}, false },
		Refresh: func(context.Context) error { return errors.New("the store is unreachable") }})
	if err != nil {
		t.Fatal(err)
	}
	_, err = broken.Put(t.Context(), objstore.HashOf(data), data)
	if !errors.Is(err, ErrNoMap) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("Put with a failing re-read = %v, want ErrNoMap naming the failure", err)
	}
}

// A READ FALLS THROUGH A HOLDER THAT LOST ITS COPY to one that did not, and a
// chunk nobody holds is ErrNotFound.
func TestAReadFallsThroughAHolderThatLostItsCopy(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c")
	data := []byte("half remembered")
	h := objstore.HashOf(data)
	if _, err := f.client("agent-1").Put(t.Context(), h, data); err != nil {
		t.Fatal(err)
	}
	up := f.m.Up(h.PG())
	if err := f.members[up[0]].disk.Delete(h); err != nil {
		t.Fatal(err)
	}
	if got, err := f.client("agent-1").Get(t.Context(), h); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get past a lost copy = %q, %v", got, err)
	}
	if err := f.members[up[1]].disk.Delete(h); err != nil {
		t.Fatal(err)
	}
	if _, err := f.client("agent-1").Get(t.Context(), h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a chunk nobody holds = %v, want ErrNotFound", err)
	}
}

// counting is an asker that counts the requests each subject was sent.
type counting struct {
	Asker
	mu    sync.Mutex
	asked map[string]int
}

func (c *counting) Ask(ctx context.Context, subject string, req []byte, want int) ([][]byte, error) {
	c.mu.Lock()
	c.asked[subject]++
	c.mu.Unlock()
	return c.Asker.Ask(ctx, subject, req, want)
}

func (c *counting) count(subject string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asked[subject]
}

// A MEMBER THAT DID NOT ANSWER IS ASKED LAST FOR A WHILE, so a dead holder
// costs one attempt per client rather than one per chunk — and is asked first
// again once the cooldown has passed.
func TestASilentMemberIsAskedLast(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a", "data-b", "data-c")
	asker := &counting{Asker: f.queue(), asked: map[string]int{}}
	now := time.Now()
	c, err := NewClient(ClientOptions{Queue: asker, Self: "agent-1", Maps: f.maps,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	// Three chunks whose primary is the same member, which falls silent.
	var chunks [][]byte
	var primary string
	for i := 0; len(chunks) < 3; i++ {
		data := []byte{byte(i), byte(i >> 8), 'k'}
		p := f.m.Up(objstore.HashOf(data).PG())[0]
		if primary == "" {
			primary = p
		}
		if p == primary {
			chunks = append(chunks, data)
		}
	}
	f.members[primary].silent.Store(true)
	put := func(data []byte) {
		t.Helper()
		if _, err := c.Put(t.Context(), objstore.HashOf(data), data); err != nil {
			t.Fatal(err)
		}
	}

	put(chunks[0])
	put(chunks[1])
	if got := asker.count(Subject(primary)); got != 1 {
		t.Fatalf("the silent primary was asked %d times over two writes, want once", got)
	}
	now = now.Add(suspectFor + time.Second)
	put(chunks[2])
	if got := asker.count(Subject(primary)); got != 2 {
		t.Fatalf("after the cooldown the primary was asked %d times in all, want 2", got)
	}
}

// A HAS ANSWERS WHAT THE MEMBER HOLDS AND WHETHER ITS OWN MAP PLACES IT THERE,
// at the epoch it judged by — the three facts a collector needs before it
// deletes a copy anywhere else.
func TestHasAnswersHeldPlacedAndTheEpoch(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a", "data-b")
	f.m.Epoch = 7
	var placedOnA []byte
	for i := 0; ; i++ {
		data := []byte{byte(i), 'p'}
		if f.m.Up(objstore.HashOf(data).PG())[0] == "data-a" {
			placedOnA = data
			break
		}
	}
	var elsewhere []byte
	for i := 0; ; i++ {
		data := []byte{byte(i), 'q'}
		if f.m.Up(objstore.HashOf(data).PG())[0] == "data-b" {
			elsewhere = data
			break
		}
	}
	for _, data := range [][]byte{placedOnA, elsewhere} {
		if err := f.members["data-a"].disk.Put(objstore.HashOf(data), data); err != nil {
			t.Fatal(err)
		}
	}
	absent := objstore.HashOf([]byte("never written"))
	got, err := f.client("agent-1").Has(t.Context(), "data-a",
		[]objstore.Hash{objstore.HashOf(placedOnA), objstore.HashOf(elsewhere), absent})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Held, []bool{true, true, false}) {
		t.Errorf("held = %v", got.Held)
	}
	if got.Placed[0] != true || got.Placed[1] != false {
		t.Errorf("placed = %v, want the first placed on data-a and the second not", got.Placed)
	}
	if got.Epoch != 7 {
		t.Errorf("epoch = %d, want 7", got.Epoch)
	}
}

// AN OBJECT STREAMS BACK WHOLE, and a manifest whose chunks do not make up the
// object it names fails at the end rather than ending quietly.
func TestAnObjectStreamsBackWholeAndAWrongManifestFails(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 2, "data-a", "data-b", "data-c")
	writer := f.client("agent-1")
	body := make([]byte, 3*objstore.ChunkSize+99)
	for i := range body {
		body[i] = byte(i * 7)
	}
	m, err := objstore.Split(t.Context(), bytes.NewReader(body), int64(len(body)),
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			_, err := writer.Put(ctx, c.Hash, data)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.client("agent-2").Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, %v; want the %d written", len(got), err, len(body))
	}

	lying := m
	lying.Hash = objstore.HashOf([]byte("some other object"))
	r, err = f.client("agent-2").Open(t.Context(), lying)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a manifest naming another object read to a clean end")
	}
	_ = r.Close()
}

// CLOSING A READER HALF WAY STOPS IT, and a chunk nobody holds is an error at
// the read that needed it.
func TestAReaderStopsWhenClosedAndFailsOnAMissingChunk(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	writer := f.client("agent-1")
	body := make([]byte, 2*objstore.ChunkSize)
	for i := range body {
		body[i] = byte(i * 3)
	}
	m, err := objstore.Split(t.Context(), bytes.NewReader(body), int64(len(body)),
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			_, err := writer.Put(ctx, c.Hash, data)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.client("agent-2").Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}

	if err := f.members["data-a"].disk.Delete(m.Chunks[1].Hash); err != nil {
		t.Fatal(err)
	}
	r, err = f.client("agent-2").Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); !errors.Is(err, ErrNotFound) {
		t.Fatalf("reading past a missing chunk = %v, want ErrNotFound", err)
	}
}

// A MEMBER ALWAYS ANSWERS, even a request it cannot read: silence would read
// as a member that is down.
func TestAMemberAnswersEvenWhatItCannotRead(t *testing.T) {
	t.Parallel()
	store, err := disk.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	maps := func() (placement.Map, bool) { return placement.Map{}, false }
	for name, raw := range map[string][]byte{
		"empty":        nil,
		"short header": {0, 0, 0, 9, '{'},
		"not json":     {0, 0, 0, 1, 'x'},
	} {
		var rep reply
		if _, err := unframe(answer(t.Context(), "data-a", store, maps, raw), &rep); err != nil {
			t.Fatalf("%s: the answer is not a frame: %v", name, err)
		}
		if rep.Status != statusRefused || rep.Node != "data-a" {
			t.Errorf("%s: answered %+v, want a refusal naming the node", name, rep)
		}
	}
	unknown, err := frame(request{Op: "compact"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var rep reply
	if _, err := unframe(answer(t.Context(), "data-a", store, maps, unknown), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Status != statusRefused {
		t.Errorf("an operation from a newer build answered %+v, want a refusal", rep)
	}
}

// A RANGED READ ANSWERS EXACTLY THE BYTES ASKED FOR, across chunk boundaries,
// short at the end of the object and empty past it.
func TestARangedReadAnswersItsRange(t *testing.T) {
	t.Parallel()
	f := newFleet(t, 1, "data-a")
	writer := f.client("agent-1")
	body := make([]byte, 2*objstore.ChunkSize+500)
	for i := range body {
		body[i] = byte(i * 13)
	}
	m, err := objstore.Split(t.Context(), bytes.NewReader(body), int64(len(body)),
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			_, err := writer.Put(ctx, c.Hash, data)
			return err
		})
	if err != nil {
		t.Fatal(err)
	}
	reader := f.client("agent-2")
	for _, c := range []struct{ off, n int64 }{
		{0, 10}, {objstore.ChunkSize - 5, 10}, {objstore.ChunkSize, objstore.ChunkSize},
		{2*objstore.ChunkSize + 490, 100}, {int64(len(body)), 5}, {0, int64(len(body))},
	} {
		got, err := reader.ReadAt(t.Context(), m, c.off, c.n)
		if err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", c.off, c.n, err)
		}
		end := min(c.off+c.n, int64(len(body)))
		want := body[min(c.off, end):end]
		if !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d, %d) answered %d bytes, want %d", c.off, c.n, len(got), len(want))
		}
	}
}
