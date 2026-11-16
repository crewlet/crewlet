package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
)

// A data node raises the nats bucket to a larger store.objects.nats_max_bytes
// at boot, once the state logs hold their reservations: grow only, one node at
// a time under a lease, and never past the room the coordination records keep.

// objectsRaiseClass is the lease a data node holds while it raises the bucket,
// one resource for the bucket's stream.
const objectsRaiseClass coord.Class = "objects_raise"

// objectsRaisePoll is how often a node waiting on another's raise asks for the
// lease again: a raise is two reads and one update, milliseconds on a settled
// broker, so a quarter second keeps the wait short for a coordination read.
const objectsRaisePoll = 250 * time.Millisecond

// bucketRaiser is the nats bucket as a raise reads and lifts it.
type bucketRaiser interface {
	Usage(ctx context.Context) (objstore.Usage, error)
	Raise(ctx context.Context, maxBytes int64) (int64, error)
}

// objectsRaise is a raise of the bucket a node's boot did not make: the figure
// it was configured with, the reservation the bucket kept, and why.
type objectsRaise struct {
	Wanted, Kept int64
	Why          string
}

// raiseObjects raises the nats bucket to a data node's explicit
// store.objects.nats_max_bytes and answers the raise it did not make, nil where
// there was none to refuse. owner names this boot for the lease.
func raiseObjects(ctx context.Context, b *config.Bootstrap, out *Backends, owner string) *objectsRaise {
	want := b.Store.Objects.NatsMaxBytes
	bucket, ok := out.Objects.(bucketRaiser)
	// A NODE WITHOUT `data` NEVER RAISES IT: the data nodes size the
	// broker's shared streams.
	if want <= 0 || !ok || !HoldsEstate(b) {
		return nil
	}
	timing := jsprovision.Clustered(clusteredStream(b)).Timing()
	held, kept := bucketFits(ctx, bucket, want)
	if kept != nil || held >= want || held <= 0 {
		return kept
	}
	// HELD FOR TWICE THE RAISE'S OWN BOUND, so a holder's raise always ends
	// before its lease can lapse to another node.
	release, err := holdObjectsRaise(ctx, out.Coord, owner, 2*timing.Budget)
	if err != nil {
		return refuseRaise(ctx, want, held, err.Error())
	}
	defer release()
	raiseCtx, cancel := context.WithTimeout(ctx, timing.Budget)
	defer cancel()
	// READ AGAIN UNDER THE LEASE: another data node may have raised it while
	// this one waited.
	if held, kept = bucketFits(raiseCtx, bucket, want); kept != nil || held >= want || held <= 0 {
		return kept
	}
	if why := objectsRoom(raiseCtx, out.Queue, held, want); why != "" {
		return refuseRaise(ctx, want, held, why)
	}
	raised, err := bucket.Raise(raiseCtx, want)
	switch {
	case errors.Is(err, natsobj.ErrNoRoom):
		return refuseRaise(ctx, want, held, fmt.Sprintf("the broker refused to reserve "+
			"the difference: %v", err))
	case err != nil:
		return refuseRaise(ctx, want, held, fmt.Sprintf("the raise did not complete: %v", err))
	case raised < want:
		return refuseRaise(ctx, want, raised, "the broker answered the raise with a "+
			"smaller reservation")
	}
	log.InfoContext(ctx, "objects_bucket_raised", "bucket", natsobj.Bucket,
		"from", held, "to", raised,
		"detail", "store.objects.nats_max_bytes is larger than the reservation the "+
			"bucket held, and the broker could grant the difference beside the state "+
			"logs and the coordination records' floor")
	return nil
}

// bucketFits reads the reservation the bucket holds, saying a larger one than
// want is kept, and answers an unreadable one as a raise refused.
func bucketFits(ctx context.Context, bucket bucketRaiser, want int64) (int64, *objectsRaise) {
	usage, err := bucket.Usage(ctx)
	if err != nil {
		return 0, refuseRaise(ctx, want, 0, fmt.Sprintf("the bucket's reservation "+
			"could not be read: %v", err))
	}
	if usage.MaxBytes > want {
		log.WarnContext(ctx, "objects_bucket_not_lowered", "bucket", natsobj.Bucket,
			"holds", usage.MaxBytes, "configured", want,
			"detail", "store.objects.nats_max_bytes is smaller than the bucket's "+
				"reservation, and a reservation is never lowered, so the bucket keeps it")
	}
	return usage.MaxBytes, nil
}

// objectsRoom is why the broker cannot grant a raise from held to want beside
// what it has reserved and [objectsCoordinationFloor], empty where it can.
func objectsRoom(ctx context.Context, q any, held, want int64) string {
	broker, ok := q.(budgetReader)
	if !ok {
		return "this node's broker states no room it can read"
	}
	room, err := broker.StreamBudget(ctx)
	switch {
	case err != nil:
		return fmt.Sprintf("the broker's room could not be read: %v", err)
	case room.Limit < 0:
		return "the broker states no limit this node can read, so the raise cannot " +
			"be held to the room the coordination records keep"
	}
	grow, fits := want-held, room.Available()-objectsCoordinationFloor
	if grow <= fits {
		return ""
	}
	most := "no raise fits"
	if fits > 0 {
		most = fmt.Sprintf("the most it can be raised to is %s", objstore.SizeWords(held+fits))
	}
	return fmt.Sprintf("raising it reserves %s more, and the broker has %s left of its "+
		"%s limit once the state logs and the bucket are counted, %s of which the "+
		"coordination records keep: %s", objstore.SizeWords(grow),
		objstore.SizeWords(room.Available()), objstore.SizeWords(room.Limit),
		objstore.SizeWords(objectsCoordinationFloor), most)
}

// refuseRaise logs a raise not made and answers it for the alarm.
func refuseRaise(ctx context.Context, want, kept int64, why string) *objectsRaise {
	log.WarnContext(ctx, "objects_bucket_raise_refused", "bucket", natsobj.Bucket,
		"configured", want, "holds", kept, "why", why,
		"detail", "store.objects.nats_max_bytes is larger than the bucket's reservation "+
			"and this node kept it; its next boot decides again")
	return &objectsRaise{Wanted: want, Kept: kept, Why: why}
}

// holdObjectsRaise takes the bucket's raise lease for ttl, waiting up to ttl
// for another node's raise to end, and answers its release.
func holdObjectsRaise(ctx context.Context, backend coord.Backend, owner string,
	ttl time.Duration) (func(), error) {

	resource := objectsRaiseClass.Resource(natsobj.Stream)
	deadline := time.Now().Add(ttl)
	for {
		// UNGATED: raising a bucket is not a seat's work, and a rolling
		// upgrade is when an operator raises a figure.
		lease, _, err := backend.TryAcquire(ctx, resource, coord.AcquireOptions{
			Owner: owner, TTL: ttl, Ungated: true})
		if err != nil {
			return nil, fmt.Errorf("the coordination store did not grant the raise's "+
				"lease: %w", err)
		}
		if lease != nil {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
					jsprovision.LookupBudget)
				defer cancel()
				if _, err := backend.Release(releaseCtx, resource, owner, lease.Epoch); err != nil {
					log.WarnContext(ctx, "objects_raise_lease_kept", "error", err,
						"detail", "it lapses on its own")
				}
			}, nil
		}
		if !time.Now().Before(deadline) {
			return nil, errors.New("another data node held the raise's lease for " +
				"longer than a raise takes")
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the boot ended while another data node raised the "+
				"bucket: %w", ctx.Err())
		case <-time.After(objectsRaisePoll):
		}
	}
}
