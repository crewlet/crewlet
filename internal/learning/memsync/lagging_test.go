package memsync

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
)

// laggingJS is a JetStream whose replay consumers are served by a COPY OF THE
// CHANGELOG THAT IS BEHIND its leader by the newest `behind` rows.
//
// That is what a replay on a fleet can be: the consumer is an R1 ephemeral the
// server places on a random member of the stream, and a follower applies a row
// a moment after the leader acknowledged it. Staged rather than raced, because
// the window is the gap between an acknowledgement and a follower's apply, and
// a case that waits for it to open passes with the fix removed.
//
// The changelog itself — the identity check and the leader's end — is the real
// one, by embedding: only the replay is the copy's.
type laggingJS struct {
	jetstream.JetStream
	behind int
	// catchesUp says whether the copy applies the rows it is missing once
	// it has handed over what it had, as a follower does a moment later,
	// or never does within the hydration, as one cut off does.
	catchesUp bool
}

func (j laggingJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	real, err := j.JetStream.CreateConsumer(ctx, stream, cfg)
	if err != nil {
		return nil, err
	}
	// EVERYTHING THE LEADER HOLDS, read once through the real consumer, so
	// each row the copy hands over is a real message with real metadata.
	info, err := real.Info(ctx)
	if err != nil {
		return nil, err
	}
	var all []jetstream.Msg
	if n := int(info.NumPending); n > 0 {
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
	return &copyConsumer{Consumer: real, all: all, held: held, catchesUp: j.catchesUp}, nil
}

// copyConsumer serves a replay out of a copy that holds the first `held` of
// the leader's rows, and holds the rest only once it has caught up.
type copyConsumer struct {
	jetstream.Consumer
	all       []jetstream.Msg
	held      int
	next      int
	catchesUp bool
}

// Info counts what the COPY holds, which is all a consumer on it can count.
func (c *copyConsumer) Info(context.Context) (*jetstream.ConsumerInfo, error) {
	return &jetstream.ConsumerInfo{NumPending: uint64(c.held - c.next)}, nil
}

// Fetch hands over up to batch rows the copy holds. Asked for more once it has
// handed over everything it held, a copy that catches up applies the rest and
// serves them; one that does not hands over nothing, which is what a fetch
// that waited out its budget returns.
func (c *copyConsumer) Fetch(batch int, _ ...jetstream.FetchOpt) (jetstream.MessageBatch, error) {
	if c.next == c.held && c.catchesUp {
		c.held = len(c.all)
	}
	out := make(chan jetstream.Msg, batch)
	for ; c.next < c.held && len(out) < batch; c.next++ {
		out <- copyMsg{Msg: c.all[c.next], pending: uint64(c.held - c.next - 1)}
	}
	close(out)
	return copyBatch{msgs: out}, nil
}

// copyMsg is a real row whose metadata counts what is left on the COPY, as
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
// what its copy held, delivered it, and reported success. The hydration now
// reads where the leader's rows end first and is not done until the replay has
// reached it: a copy that catches up hands over the rest, and one that does not
// fails the hydration, which refuses the seat until the next sweep rather than
// admitting it having forgotten what it did last.
//
// Mutation: drop the leader's end from the loop's condition, so the replay
// stops once its copy says it has nothing more, and both cases come back as a
// success one row short.
func TestAReplayServedByACopyThatIsBehindIsNeverAShortHydration(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	owner := openStore(t)
	seedMemory(t, owner)
	if published, err := syncerOn(t, owner, conn).Publish(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish: %v", err)
	} else if published != len(tables) {
		t.Fatalf("published %d rows, want %d", published, len(tables))
	}

	t.Run("a copy that catches up hands over the rest", func(t *testing.T) {
		t.Parallel()
		next := openStore(t)
		syncer := syncerOn(t, next, conn)
		syncer.js = laggingJS{JetStream: syncer.js, behind: 1, catchesUp: true}
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(tables) {
			t.Fatalf("Hydrate = (%d, %v), want all %d rows: the copy caught up, so "+
				"the row it was missing is there to be read", carried, err, len(tables))
		}
	})

	t.Run("a copy that never catches up fails the hydration", func(t *testing.T) {
		t.Parallel()
		next := openStore(t)
		syncer := syncerOn(t, next, conn)
		syncer.js = laggingJS{JetStream: syncer.js, behind: 1}
		carried, err := syncer.Hydrate(t.Context(), seat.Handle)
		if err == nil {
			t.Fatalf("Hydrate reported success with %d of %d rows: the seat would serve "+
				"having forgotten the newest thing it learned", carried, len(tables))
		}
	})
}
