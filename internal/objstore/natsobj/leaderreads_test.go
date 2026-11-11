package natsobj_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsapi/jsapitest"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// accountBucket is the bucket on a broker with no JetStream domain, through
// the account's own API — the broker [jsapitest] can stand between a leader
// read and the stream's leader on — holding one object, whose name it answers.
func accountBucket(t *testing.T) (*server.Server, *natsobj.Backend, string) {
	t.Helper()
	return accountBucketAt(t, natsobj.Config{MaxBytes: testCeiling, Replicas: 1})
}

// accountBucketAt is [accountBucket] opened as cfg, through a client built
// with opts.
func accountBucketAt(t *testing.T, cfg natsobj.Config,
	opts ...jetstream.JetStreamOpt) (*server.Server, *natsobj.Backend, string) {

	t.Helper()
	srv, err := server.NewServer(&server.Options{
		ServerName:        "natsobj-leader-test",
		JetStream:         true,
		DontListen:        true,
		StoreDir:          t.TempDir(),
		JetStreamMaxStore: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(30 * time.Second) {
		srv.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	// The account's own API, as [jsapi.Account] builds it — through
	// jetstream.New, which is that API with the client's options.
	c, err := jetstream.New(nc, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if api, err := jsapi.Of(c); err != nil || api != jsapi.Account() {
		t.Fatalf("the case's client speaks %v (%v), want the account's own API", api, err)
	}
	b := openWith(t, c, cfg)
	name := objstore.KeyAt(time.Now()).Name()
	if err := b.Put(deadline(t), name, bytes.NewReader([]byte("held")), objstore.PutMeta{}); err != nil {
		t.Fatalf("put: %v", err)
	}
	return srv, b, name
}

// AN OBJECT'S METADATA READ THAT A MEMBER DROPS IS ASKED AGAIN.
//
// A stat reads the object's metadata from the stream LEADER, because a replica
// behind the quorum answers a just-uploaded file as not there. Only the leader
// answers it, so a member that still believes in a leader its group has just
// lost drops it — and one request held for the caller's whole context was a
// stat answered "the store did not answer" a second before the new leader
// would have.
func TestAStatWhoseLeaderReadAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	srv, b, name := accountBucket(t)
	held := jsapitest.DropFirst(t, srv, jsapitest.MsgGet(natsobj.Stream), 1)

	info, err := b.Stat(context.Background(), name)
	if err != nil {
		t.Fatalf("a stat whose metadata read the broker dropped once failed "+
			"rather than asking again: %v", err)
	}
	if info.Name != name || info.Size != int64(len("held")) {
		t.Errorf("stat = %+v, want %s of %d bytes", info, name, len("held"))
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d leader reads, want the 1 it dropped", n)
	}
}

// THE SWEEP'S PIECE LISTING THAT A MEMBER DROPS IS ASKED AGAIN.
//
// [natsobj.Backend.Pending] begins with the stream's subject listing, a
// stream info only the leader answers. Sent as one request, a member that
// dropped it held the collector's sweep for the whole of its context; asked
// again, the sweep finds what the leader holds — here nothing, since the one
// put finished.
func TestAPieceListingAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	srv, b, _ := accountBucket(t)
	held := jsapitest.DropFirst(t, srv, jsapitest.StreamInfo(natsobj.Stream), 1)

	var pending []objstore.Pending
	err := b.Pending(context.Background(), func(p objstore.Pending) error {
		pending = append(pending, p)
		return nil
	})
	if err != nil {
		t.Fatalf("a piece listing the broker dropped once failed rather than "+
			"asking again: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("a bucket whose one put finished answered %v pending", pending)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d stream infos, want the 1 it dropped", n)
	}
}

// A LISTING'S READ OF THE NEWEST METADATA THAT A MEMBER DROPS IS ASKED AGAIN.
//
// What the object collector's boot pass lost on a loaded fleet of three:
// "list the bucket: read its newest metadata from the stream's leader: jsapi:
// read OBJ_crewlet_files from its leader: context deadline exceeded", on every
// member at once — the pass's context carries no deadline, so one dropped
// request waited out the client's five seconds, and the next pass was ten
// minutes away. Asked again, the listing names the object.
func TestAListingWhoseLeaderReadAMemberDropsIsAskedAgain(t *testing.T) {
	t.Parallel()
	srv, b, name := accountBucket(t)
	held := jsapitest.DropFirst(t, srv, jsapitest.MsgGet(natsobj.Stream), 1)

	var names []string
	err := b.List(context.Background(), func(info objstore.Info) error {
		names = append(names, info.Name)
		return nil
	})
	if err != nil {
		t.Fatalf("a listing whose leader read the broker dropped once failed "+
			"rather than asking again: %v", err)
	}
	if len(names) != 1 || names[0] != name {
		t.Errorf("listed %q, want [%s]", names, name)
	}
	if n := held.Count(); n != 1 {
		t.Errorf("the broker held %d leader reads, want the 1 it dropped", n)
	}
}

// A LISTING THE BROKER BEGINS PAST ITS CLIENT'S OWN BOUND FINISHES, and one it
// never begins fails naming the time it was given: the broker reads the bucket
// before it makes the listing's consumer, and the collector sets no deadline.
func TestAListingIsGivenWhatItsBeginningTakes(t *testing.T) {
	t.Parallel()
	const bound = time.Second
	for _, c := range []struct {
		name string
		// late is how long the broker takes to begin the listing; zero
		// never begins it.
		late time.Duration
	}{
		{"begun_past_the_clients_bound", 2 * bound},
		{"never_begun", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv, b, name := accountBucketAt(t, natsobj.Config{MaxBytes: testCeiling, Replicas: 1},
				jetstream.WithDefaultTimeout(bound))
			// A bucket of a few bytes, one mebibyte begun, given what a
			// large one is: four of the client's bounds beside its own.
			natsobj.SetBeginPace(b, 4*bound)
			held := jsapitest.Hold(t, srv, jsapitest.ConsumerCreate(natsobj.Stream))
			answered := make(chan error, 1)
			go func() {
				if c.late == 0 {
					answered <- nil
					return
				}
				r, err := held.Next(t.Context())
				if err == nil {
					time.Sleep(c.late) //fixedwait:simulated a broker reading the bucket's blocks past its client's bound
					held.Release()
					err = r.Answer()
				}
				answered <- err
			}()
			var names []string
			err := b.List(context.Background(), func(info objstore.Info) error {
				names = append(names, info.Name)
				return nil
			})
			if aerr := <-answered; aerr != nil {
				t.Fatalf("answer the listing's consumer: %v", aerr)
			}
			given := fmt.Sprintf("in the %v it was given", bound+4*bound)
			switch {
			case c.late > 0 && err != nil:
				t.Fatalf("a listing the broker began %v in, past its client's %v, failed: %v",
					c.late, bound, err)
			case c.late > 0 && !slices.Equal(names, []string{name}):
				t.Fatalf("the listing named %q, want [%s]", names, name)
			case c.late == 0 && (!errors.Is(err, context.DeadlineExceeded) ||
				!strings.Contains(err.Error(), given)):
				t.Fatalf("a listing the broker never began = %v, want its deadline %s", err, given)
			}
		})
	}
}

// THE SLOWEST LISTING BEGINNING MEASURED FITS ITS BUDGET FOUR TIMES OVER (a
// bucket of 580 MB began in 13.6 s on a volume 98% full), and a bucket holding
// nothing is given the client's own bound on one request.
func TestTheSlowestListingBeginningMeasuredFitsItsBudget(t *testing.T) {
	t.Parallel()
	const bound = 5 * time.Second
	_, b, _ := accountBucketAt(t, natsobj.Config{MaxBytes: testCeiling, Replicas: 1},
		jetstream.WithDefaultTimeout(bound))
	if got := natsobj.ListBegin(b, 0); got != bound {
		t.Errorf("a listing of an empty bucket is given %v, want the client's %v", got, bound)
	}
	const size, took = 580_000_000, 13600 * time.Millisecond
	if got := natsobj.ListBegin(b, size); got < 4*took {
		t.Errorf("a listing of %d bytes is given %v to begin, under four times the %v "+
			"one took on a volume 98%% full", size, got, took)
	}
}

// A READ WHOSE ANSWER IS A WHOLE PIECE IS GIVEN THE PIECE'S CROSSING, ACROSS
// A LINK — AND ONLY THERE.
//
// The leader sends a message read's reply whole, and the reader hears none of
// it until its last byte is in: a piece of [natsobj.MessageBytes],
// base64-encoded, takes five seconds to cross a leaf link at
// [objstore.MiBPace]. Presumed lost at the read term, the read of what
// follows on a get's pieces, of a ranged walk's and of an unfinished upload's
// oldest piece was asked again while its reply was still crossing — another
// whole piece on the link a node's lease renewals cross, for every second the
// first took. A member's reply crosses a cluster route in milliseconds, so
// there a piece read keeps the read term, and a dropped one is asked again at
// two seconds rather than seven. A metadata read's reply is a few hundred
// bytes, and keeps the read term everywhere.
func TestAPieceReadIsGivenAPiecesCrossingAcrossALink(t *testing.T) {
	t.Parallel()
	crossing := time.Duration(float64(objstore.MiBPace) *
		float64(base64.StdEncoding.EncodedLen(natsobj.MessageBytes)) / float64(objstore.MiB))
	for _, c := range []struct {
		name  string
		cfg   natsobj.Config
		piece func(time.Duration) bool
		want  string
	}{
		{"across a link", acrossLink,
			func(got time.Duration) bool { return got >= jsprovision.ReadTerm+crossing },
			fmt.Sprintf("at least the read term and a piece's crossing at the floor, %v",
				jsprovision.ReadTerm+crossing)},
		{"on a member", natsobj.Config{MaxBytes: testCeiling, Replicas: 1},
			func(got time.Duration) bool { return got == jsprovision.ReadTerm },
			fmt.Sprintf("the read term %v", jsprovision.ReadTerm)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, b, name := accountBucketAt(t, c.cfg)
			for _, filter := range []string{natsobj.PieceSubject("x"), natsobj.PieceSubject("") + ">"} {
				if got := natsobj.LeaderTerm(b, filter); !c.piece(got) {
					t.Errorf("a read of what follows on %s is given %v, want %s", filter, got, c.want)
				}
			}
			for _, filter := range []string{natsobj.MetaSubject(name), natsobj.MetaSubject("") + ">"} {
				if got := natsobj.LeaderTerm(b, filter); got != jsprovision.ReadTerm {
					t.Errorf("a metadata read on %s is given %v, want the read term %v", filter, got,
						jsprovision.ReadTerm)
				}
			}
		})
	}
}

// THE STREAM INFO A USAGE READING AND A SEAL BEGIN WITH, DROPPED BY A MEMBER,
// IS ASKED AGAIN.
//
// Only the stream's leader answers its info, so a member that still believes
// in a leader its group has just lost drops it. Sent as one request on the
// bucket's handle, a usage reading — the object gauge's, on every turn of the
// collector's duty, which carries no deadline — waited out the client's five
// seconds and read the bucket as unreadable, and a seal the migration's last
// step makes failed a move that had copied every object. Asked again, the new
// leader answers.
func TestTheStreamInfoAUsageAndASealReadIsAskedAgainWhenDropped(t *testing.T) {
	t.Parallel()
	for name, read := range map[string]func(context.Context, *natsobj.Backend) error{
		"a usage reading": func(ctx context.Context, b *natsobj.Backend) error {
			u, err := b.Usage(ctx)
			if err == nil && (u.MaxBytes != testCeiling || u.Bytes <= 0) {
				return fmt.Errorf("usage %+v, want some bytes of a %d ceiling", u, testCeiling)
			}
			return err
		},
		"a seal": func(ctx context.Context, b *natsobj.Backend) error {
			if err := b.Seal(ctx); err != nil {
				return err
			}
			if err := b.Put(ctx, objstore.KeyAt(time.Now()).Name(),
				bytes.NewReader([]byte("x")), objstore.PutMeta{}); err == nil {
				return errors.New("a put after the seal was stored")
			}
			return nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, b, _ := accountBucket(t)
			held := jsapitest.DropFirst(t, srv, jsapitest.StreamInfo(natsobj.Stream), 1)
			if err := read(context.Background(), b); err != nil {
				t.Fatalf("a read whose stream info the broker dropped once failed rather "+
					"than asking again: %v", err)
			}
			if n := held.Count(); n != 1 {
				t.Errorf("the broker held %d stream infos, want the 1 it dropped", n)
			}
		})
	}
}

// A STREAM INFO NOBODY ANSWERS, FOR A CALLER WITH NO DEADLINE, ENDS AT ITS
// CLIENT'S OWN DEFAULT — asked again inside it, and never stretched to the
// lookup ceiling: the bound the one request had is the bound its callers were
// sized against. The client here is given a default of 3.9 seconds — just
// short of a third request, at four — so the read is asked twice inside it,
// at none and two seconds, where a read held to the five seconds of a default
// it did not take from its client is asked three times, and one held to the
// thirty-second ceiling fifteen.
//
// THE READ RUNS IN A SYNCTEST BUBBLE, and the broker outside it: inside, every
// timer of the read fires at exactly its instant, so it ends at exactly 3.9
// seconds with exactly two requests sent. On the machine's clock the bound and
// the third request were a tenth of a second apart, and a CI runner that
// stalled across both sent the third — past the bound, which the hedge no
// longer does ([jsprovision.ReadBoundedWith]) — so the verdict turned on how
// late a loaded runner woke. The requests are counted at the broker behind a
// fence on the read's own connection ([jsapitest.Held.Fence]): in the
// bubble's few real milliseconds the broker may not yet have handed them over.
func TestAStreamInfoNobodyAnswersEndsAtTheClientsDefault(t *testing.T) {
	t.Parallel()
	const bound = 3900 * time.Millisecond
	srv, b, _ := accountBucketAt(t, natsobj.Config{MaxBytes: testCeiling, Replicas: 1},
		jetstream.WithDefaultTimeout(bound))
	held := jsapitest.Hold(t, srv, jsapitest.StreamInfo(natsobj.Stream))
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if _, err := b.Usage(t.Context()); err == nil {
			t.Fatal("a usage reading nobody answered succeeded")
		}
		if took := time.Since(start); took != bound {
			t.Errorf("the read ended after %v, want its client's %v", took, bound)
		}
	})
	client, ok := clients.Load(objstore.Backend(b))
	if !ok {
		t.Fatal("the backend was not opened through openWith()")
	}
	asked, err := held.Fence(deadline(t), client.(jetstream.JetStream).Conn())
	if err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Errorf("the read was asked %d times inside its client's %v, want 2: at none and "+
			"two seconds", asked, bound)
	}
}
