package natsobj_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
)

// A READ THAT MEETS A STREAM WITH NO LEADER WAITS THE ELECTION OUT inside its
// caller's deadline, on every path that asks the broker something only a
// leader answers: a member that knows its group is leaderless answers 503,
// err_code 10008, which is an election under way and not the read's answer
// (crewlet/crewlet#2096, where a git read failed on it mid-election).
//
// Each case holds the first request on its path, has the broker's member answer
// it leaderless, and lets the rest through: the read must ask again and finish.
//
// Mutant: natsobj's reads return the first leaderless answer (no
// ThroughElection) — every case fails on the 10008.
func TestAReadWaitsOutAStreamWithNoLeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, subject string
		read          func(context.Context, *natsobj.Backend, string) error
	}{
		{name: "a stat's metadata read", subject: jsapitest.MsgGet(natsobj.Stream), read: stat},
		{name: "a get's consumer", subject: jsapitest.ConsumerCreate(natsobj.Stream), read: get},
		{name: "a listing's read of the bucket's size", subject: jsapitest.StreamInfo(natsobj.Stream),
			read: list},
		{name: "a usage reading", subject: jsapitest.StreamInfo(natsobj.Stream), read: usage},
		{name: "the sweep's piece listing", subject: jsapitest.StreamInfo(natsobj.Stream), read: pending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, b, name := accountBucket(t)
			ctx := deadline(t)
			held := jsapitest.Hold(t, srv, tc.subject)
			done := make(chan error, 1)
			go func() { done <- tc.read(ctx, b, name) }()
			first, err := held.Next(ctx)
			if err != nil {
				t.Fatalf("the read never asked: %v", err)
			}
			held.Release()
			if err := first.Leaderless(); err != nil {
				t.Fatalf("answer the read leaderless: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("a read whose first answer was a stream with no leader failed rather "+
					"than asking again once the election could have ended: %v", err)
			}
		})
	}
}

// A READ WHOSE DEADLINE ENDS WHILE ITS STREAM HAS NO LEADER FAILS SAYING SO:
// still bounded by its caller, and naming the stream, so whoever reads the
// error knows it was an election rather than the object.
//
// Mutant: the wait ends without [jsprovision.NoLeader] (the last answer handed
// up) — the error names no stream without a leader.
func TestAReadWhoseDeadlineEndsWhileItsStreamHasNoLeaderSaysSo(t *testing.T) {
	t.Parallel()
	srv, b, name := accountBucket(t)
	held := jsapitest.Hold(t, srv, jsapitest.MsgGet(natsobj.Stream))
	answering, stop := context.WithCancel(t.Context())
	defer stop()
	go func() {
		for {
			r, err := held.Next(answering)
			if err != nil {
				return
			}
			if err := r.Leaderless(); err != nil {
				t.Errorf("answer the read leaderless: %v", err)
				return
			}
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err := b.Stat(ctx, name)
	want := fmt.Sprintf("the %s stream had no leader", natsobj.Stream)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("a stat whose stream had no leader until its deadline answered %v, want an "+
			"error saying %q", err, want)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the error %v is not its deadline's, so a caller cannot tell it from the "+
			"object's own answer", err)
	}
	if held.Count() < 2 {
		t.Errorf("the stat asked %d times, want it asked again while it waited", held.Count())
	}
}

// ONLY A GROUP WITH NO LEADER IS WAITED OUT: a stream the broker answers is
// not there is that answer, at once, since no election brings it back.
//
// Mutant: every 503 waited out, the stream's absence among them — the stat
// asks again until its deadline.
func TestAReadOfAStreamThatIsNotThereFailsAtOnce(t *testing.T) {
	t.Parallel()
	srv, b, name := accountBucket(t)
	held := jsapitest.Hold(t, srv, jsapitest.MsgGet(natsobj.Stream))
	ctx := deadline(t)
	done := make(chan error, 1)
	go func() { done <- stat(ctx, b, name) }()
	first, err := held.Next(ctx)
	if err != nil {
		t.Fatalf("the stat never asked: %v", err)
	}
	if err := first.Refuse(server.NewJSStreamNotFoundError()); err != nil {
		t.Fatalf("answer the stat not found: %v", err)
	}
	err = <-done
	if err == nil || strings.Contains(err.Error(), "had no leader") {
		t.Fatalf("a stat answered that its stream is not there = %v, want that answer", err)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the stat asked %d times, want once: no wait brings a stream back", n)
	}
}

// A READ WHOSE CONSUMER'S CREATE A MEMBER DROPS IS ASKED AGAIN, not waited for
// until its caller's deadline: while the metadata group elects, a member that
// is not its leader drops the create without a word (on a cluster too young to
// answer 10008), and the reply never comes once a leader is elected.
//
// Mutant: the create asked once — the get waits out its whole deadline.
func TestAReadWhoseConsumerCreateWasDroppedAsksAgain(t *testing.T) {
	t.Parallel()
	srv, _, name := accountBucket(t)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	client, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	// A TERM OF A SECOND, the production one being a clustered election's
	// fifteen: what is held is that the create is asked again, not when.
	timing := jsprovision.Clustered(false).Timing()
	timing.AskTerm = time.Second
	b, err := natsobj.OpenAt(t.Context(), client, natsobj.Config{MaxBytes: testCeiling, Replicas: 1},
		timing)
	if err != nil {
		t.Fatalf("open the bucket: %v", err)
	}
	dropped := jsapitest.DropFirst(t, srv, jsapitest.ConsumerCreate(natsobj.Stream), 1)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := get(ctx, b, name); err != nil {
		t.Fatalf("a get whose consumer's first create was dropped failed rather than asking "+
			"again: %v", err)
	}
	if n := dropped.Count(); n != 1 {
		t.Errorf("the broker dropped %d creates, want the 1 the case staged", n)
	}
}

func stat(ctx context.Context, b *natsobj.Backend, name string) error {
	_, err := b.Stat(ctx, name)
	return err
}

func get(ctx context.Context, b *natsobj.Backend, name string) error {
	rc, err := b.Get(ctx, name, 0, -1)
	if err != nil {
		return err
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err == nil && string(body) != "held" {
		return fmt.Errorf("read %q, want %q", body, "held")
	}
	return err
}

func list(ctx context.Context, b *natsobj.Backend, name string) error {
	var names []string
	err := b.List(ctx, func(info objstore.Info) error {
		names = append(names, info.Name)
		return nil
	})
	if err == nil && (len(names) != 1 || names[0] != name) {
		return fmt.Errorf("listed %q, want %q alone", names, name)
	}
	return err
}

func usage(ctx context.Context, b *natsobj.Backend, _ string) error {
	_, err := b.Usage(ctx)
	return err
}

func pending(ctx context.Context, b *natsobj.Backend, _ string) error {
	return b.Pending(ctx, func(p objstore.Pending) error {
		return fmt.Errorf("the one put finished, and %s is pending", p.ID)
	})
}
