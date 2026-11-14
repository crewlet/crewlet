package memsync

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// laggingJS is a JetStream whose replay consumers are served by a COPY OF THE
// CHANGELOG THAT IS BEHIND its leader by the newest `behind` messages each
// replay reads: a table's newest rows and the hydration's barrier past them,
// since a hydration replays a table at a time, each to the barrier.
//
// That is what a replay on a fleet can be: the consumer is an R1 ephemeral the
// server places on a random member of the stream, and a follower applies a row
// a moment after the leader acknowledged it. Staged rather than raced, because
// the window is the gap between an acknowledgement and a follower's apply, and
// a case that waits for it to open passes with the fix removed.
//
// The changelog itself — the identity check and the barrier's publish — is the
// real one, by embedding: only the replay is the copy's.
type laggingJS struct {
	jetstream.JetStream
	behind int
	// catchesUp says whether the copy applies the messages it is missing
	// once it has handed over what it had, as a follower does a moment
	// later, or never does within the hydration, as one cut off does.
	catchesUp bool
	// stale is a message the copy still holds that the changelog has since
	// replaced, read off it before: an earlier hydration's barrier, which a
	// copy that has not applied this hydration's still holds. The copy holds
	// it where it lies below the first message it lacks, as a copy applies the
	// stream in its order; nil for none.
	stale jetstream.Msg
}

func (j laggingJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	real, err := j.JetStream.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	// EVERYTHING THE LEADER HOLDS, read once through the real consumer, so
	// each message the copy hands over is a real one with real metadata.
	var all []jetstream.Msg
	if n := int(real.CachedInfo().NumPending); n > 0 {
		batch, err := real.Fetch(n, jetstream.FetchContext(ctx))
		if err != nil {
			return nil, err
		}
		for msg := range batch.Messages() {
			all = append(all, msg)
		}
		if err := batch.Error(); err != nil {
			return nil, err
		}
	}
	held := max(len(all)-j.behind, 0)
	copied := slices.Clone(all[:held])
	if j.stale != nil && held < len(all) && slices.Contains(cfg.FilterSubjects, j.stale.Subject()) {
		at, err := sequence(j.stale)
		if err != nil {
			return nil, err
		}
		lacked, err := sequence(all[held])
		if err != nil {
			return nil, err
		}
		if at < lacked {
			place := len(copied)
			for i, msg := range copied {
				seq, err := sequence(msg)
				if err != nil {
					return nil, err
				}
				if seq > at {
					place = i
					break
				}
			}
			copied = slices.Insert(copied, place, j.stale)
		}
	}
	return &copyConsumer{Consumer: real, copied: copied, rest: all[held:], catchesUp: j.catchesUp}, nil
}

// sequence is a message's stream sequence.
func sequence(msg jetstream.Msg) (uint64, error) {
	md, err := msg.Metadata()
	if err != nil {
		return 0, err
	}
	return md.Sequence.Stream, nil
}

// copyConsumer serves a replay out of a copy that holds copied, and holds the
// rest only once it has caught up.
type copyConsumer struct {
	jetstream.Consumer
	copied    []jetstream.Msg
	rest      []jetstream.Msg
	next      int
	catchesUp bool
}

// CachedInfo counts what the COPY holds, which is all a consumer on it can
// count, and all the reply that created it says: the consumer's own, the
// name it is deleted by included, with the copy's count.
func (c *copyConsumer) CachedInfo() *jetstream.ConsumerInfo {
	info := *c.Consumer.CachedInfo()
	info.NumPending = uint64(len(c.copied) - c.next)
	return &info
}

// Fetch hands over up to batch messages the copy holds. Asked for more once it
// has handed over everything it held, a copy that catches up applies the rest
// and serves them; one that does not hands over nothing, which is what a fetch
// that waited out its budget returns.
func (c *copyConsumer) Fetch(batch int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if c.next == len(c.copied) && c.catchesUp {
		c.copied, c.rest = append(c.copied, c.rest...), nil
	}
	out := make(chan jetstream.Msg, batch)
	for ; c.next < len(c.copied) && len(out) < batch; c.next++ {
		out <- copyMsg{Msg: c.copied[c.next], pending: uint64(len(c.copied) - c.next - 1)}
	}
	close(out)
	return copyBatch{msgs: out}, nil
}

// copyMsg is a real message whose metadata counts what is left on the COPY, as
// every message a consumer delivers does.
type copyMsg struct {
	jetstream.Msg
	pending uint64
}

func (m copyMsg) Metadata() (*jetstream.MsgMetadata, error) {
	md, err := m.Msg.Metadata()
	if err != nil {
		return nil, err
	}
	onCopy := *md
	onCopy.NumPending = m.pending
	return &onCopy, nil
}

type copyBatch struct{ msgs chan jetstream.Msg }

func (b copyBatch) Messages() <-chan jetstream.Msg { return b.msgs }
func (b copyBatch) Error() error                   { return nil }

// A REPLAY SERVED BY A COPY THAT IS BEHIND IS NEVER A HYDRATION THAT IS SHORT.
//
// The rows a lagging copy has not applied are the seat's NEWEST — the ones its
// last owner wrote just before placement moved it — and the replay counted
// what its copy held, delivered it, and reported success. A table's replay is
// now done only once its copy has delivered a barrier at or past the one the
// hydration published before it and holds nothing more: the copy here lacks
// each table's newest row and the barrier past it, and one that catches up
// hands both over, while one that does not refuses the seat until the next
// sweep, naming the barrier it never delivered, rather than admitting the
// seat having forgotten what it did last.
//
// Mutation: take the copy's word for the end, ending a table's replay once its
// copy says it holds nothing more whether or not the barrier has arrived, and
// both cases come back as a success short of each table's newest row.
func TestAReplayServedByACopyThatIsBehindIsNeverAShortHydration(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	owner := openStore(t)
	seedMemory(t, owner)
	if published, err := syncerOn(t, owner, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	} else if published.Rows != len(tables) {
		t.Fatalf("published %d rows, want %d", published.Rows, len(tables))
	}

	t.Run("a copy that catches up hands over the rest", func(t *testing.T) {
		t.Parallel()
		next := openStore(t)
		syncer := syncerOn(t, next, conn)
		syncer.js = laggingJS{JetStream: syncer.js, behind: 2, catchesUp: true}
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(tables) {
			t.Fatalf("Hydrate = (%d, %v), want all %d rows: the copy caught up, so "+
				"the rows it was missing are there to be read", carried, err, len(tables))
		}
	})

	t.Run("a copy that never catches up fails the hydration", func(t *testing.T) {
		t.Parallel()
		next := openStore(t)
		syncer := syncerOn(t, next, conn)
		syncer.js = laggingJS{JetStream: syncer.js, behind: 2}
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		if err == nil {
			t.Fatalf("Hydrate reported success with %d of %d rows: the seat would serve "+
				"having forgotten the newest thing it learned", carried, len(tables))
		}
		if !strings.Contains(err.Error(), "had not applied the barrier this hydration published") {
			t.Errorf("the hydration was refused for another reason than the barrier its copy "+
				"never delivered: %v", err)
		}
	})
}
