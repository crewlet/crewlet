// The MARKERS a removal leaves, and the two operations that must never depend
// on one: creating a key, and sweeping the markers themselves.
//
// A KV delete or purge does not remove a key's subject from its stream: it
// appends a message whose KV-Operation header says DEL or PURGE, and on these
// buckets — one revision per key — that marker replaces the value it removes.
// A bucket the broker ages takes the marker with everything else. A bucket it
// never ages keeps every marker for the life of the deployment, and every one
// of them is a leader read on each listing whose pass delivers it (walk.go, "A
// tombstone the pass delivered is not an answer"). So they are swept.
//
// # Every removal is a purge
//
// In a bucket no clock ages, a record is removed with a PURGE rather than a
// delete. On a bucket keeping one revision per key the two leave the same
// single marker, so the marker is not the reason. The reason is the key's
// OLDER revisions: a purge removes every one of them whatever history the
// bucket was created with, and a bucket here is adopted as it already exists
// ([openBucket]), so its history is not this build's to assume. A listing
// never returns the marker either way ([visitLive]), and [sweepMarkers]
// removes it once it is older than [coord.MarkerRetention]. The follows are
// the deliberate exception in the other direction — a bucket with an age,
// whose markers leave with it (see [FleetStore.Unfollow]).
//
// # A create asks the LEADER, and survives the sweep
//
// The client's own Create (nats.go jetstream/kv.go) writes at an expected
// revision of zero, and when that is refused it reads the key and, finding a
// marker, writes again at the marker's revision. Three things are wrong with
// that shape here, and the sweep adds the third:
//
//   - its read is the bucket's Get, a DIRECT get any replica answers (walk.go,
//     "Every certifying answer is the LEADER's"). A replica that has not
//     applied the removal answers with the old value, and a create over a key
//     removed on the leader reports the key as held by a record that is gone;
//   - the second write's refusal is returned as the client got it, and on a
//     REPLICATED stream that is a bare API error matching no sentinel —
//     measured at three replicas, a fifth of the losers of a create over a
//     marker — so a lost race read as an outage;
//   - and a marker SWEPT between its read and its second write fails that
//     write against a subject that now holds nothing, which the client reports
//     as the key existing.
//
// [createKey] is the one create this package makes. It conditions on what the
// stream LEADER holds and loops: a live value is the key existing, a marker is
// the revision to write over, and nothing at all — never written, or its
// marker swept — is a write at zero. A marker's persistence is therefore
// something no write here needs.
//
// # The sweep removes only what it saw
//
// NOT the client's PurgeDeletes, which collects markers with a watcher and
// then purges each one's subject OUTRIGHT: a key re-created between the
// watcher delivering its marker and the purge loses its new value, the one
// outcome a sweep of records nobody needs may never have. [sweepMarkers]
// purges each subject THROUGH the marker's own revision
// ([server.JSApiStreamPurgeRequest] with a sequence bound), so a value written
// after the pass saw the marker has a later sequence and is untouched — by
// the broker, in the same step that removes the marker, with no read to
// race. And its pass is deliberately UNCERTIFIED: a marker it misses is swept
// on the next tick, and a marker it delivers from a replica that is behind is
// either still the key's newest message or already replaced, and the bound
// removes it or nothing.

package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// createRounds bounds [createKey]'s loop.
//
// An ordinary create takes ONE round; a create over a marker takes two (the
// refusal, then the write at the marker's revision); and a marker swept
// between that read and that write takes three. Every round past those needs
// ANOTHER writer to remove the key again inside this one's read-to-write
// window, which nothing in this estate does repeatedly. Eight is that twice
// over with room, and exhausting it is reported as UNKNOWN through
// [contended] — never as the key being held, since losing says nothing about
// who holds it.
const createRounds = 8

// createKey writes value under key only when the key holds no live value,
// answering the revision it wrote, or an error wrapping
// [jetstream.ErrKeyExists] when the stream leader holds a live value there.
//
// Every other failure is returned as it came, for the caller to classify as
// unavailable: a create that could not be made establishes nothing. See the
// file doc for why this is not the bucket handle's own Create.
func createKey(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	key string, value []byte) (uint64, error) {

	// Resolved up front, although only a refused create reads, for
	// [eachEntryUnder]'s reason: a client built on an API this engine does
	// not speak must fail every create rather than only the ones that raced.
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return 0, err
	}
	var expect uint64 // zero: the key must hold no message at all
	for range createRounds {
		revision, err := kv.Update(ctx, key, value, expect)
		switch {
		case err == nil:
			return revision, nil
		case !isWrongLastSequence(err):
			return 0, err
		}
		last, err := read.last(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Never written, or its marker swept since the refusal.
			expect = 0
		case err != nil:
			return 0, err
		case last.Operation() == jetstream.KeyValuePut:
			return 0, fmt.Errorf("%w: %s holds revision %d", jetstream.ErrKeyExists, key, last.Revision())
		default:
			expect = last.Revision()
		}
	}
	return 0, contended("create", kv.Bucket()+"/"+key)
}

// sweepMarkers removes, from one bucket, every delete or purge marker written
// before cutoff, reporting how many messages went.
//
// A failure stops the sweep of this bucket and reports what it had removed
// beside the error: every marker left is swept on the next tick, and a sweep
// that pressed on past a broker refusing it would only log the same refusal
// once per marker.
func sweepMarkers(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	cutoff time.Time) (int64, error) {

	what := "the markers in " + kv.Bucket()
	purge, err := newLeaderPurge(js, kv)
	if err != nil {
		return 0, fmt.Errorf("coord/kv: sweep %s: %w", what, err)
	}
	pass, err := watchWalk(ctx, kv, jetstream.AllKeys, what)
	if err != nil {
		return 0, err
	}
	// KEY ORDER, so two sweeps of one bucket walk it identically and a
	// failure part-way names the same key on every retry.
	keys := make([]string, 0, len(pass))
	for key, kve := range pass {
		if kve.Operation() != jetstream.KeyValuePut && kve.Created().Before(cutoff) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var swept int64
	for _, key := range keys {
		n, err := purge.through(ctx, key, pass[key].Revision())
		if err != nil {
			return swept, unavailable("sweep "+what, fmt.Errorf("remove the marker on %q: %w", key, err))
		}
		swept += int64(n)
	}
	return swept, nil
}

// leaderPurge removes messages on one key's subject through a revision, with a
// `$JS.API.STREAM.PURGE` the stream leader performs.
//
// ON THE WIRE rather than through the client's Stream.Purge for one reason:
// that call answers only an error, and the broker's answer carries how many
// messages the purge removed. A marker the pass saw may already have been
// replaced by the time it is purged, and a sweep that counted what it ASKED
// for would report removing markers nobody removed.
type leaderPurge struct {
	conn    *nats.Conn
	subject string // the PURGE subject of this bucket's stream, in the client's API
	pre     string // the subject prefix a key is written under
	timeout time.Duration
}

// newLeaderPurge addresses the purges of kv's stream in the API js speaks.
func newLeaderPurge(js jetstream.JetStream, kv jetstream.KeyValue) (*leaderPurge, error) {
	api, err := apiOf(js)
	if err != nil {
		return nil, err
	}
	subject, err := api.Subject(fmt.Sprintf(server.JSApiStreamPurgeT, bucketStream(kv)))
	if err != nil {
		return nil, err
	}
	return &leaderPurge{
		conn: js.Conn(), subject: subject, pre: bucketSubjects(kv),
		timeout: js.Options().DefaultTimeout,
	}, nil
}

// through removes every message on key's subject at or below revision,
// answering how many there were. A message written after revision survives:
// the broker applies the bound itself, under the stream's own lock.
func (p *leaderPurge) through(ctx context.Context, key string, revision uint64) (uint64, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	// "Purge up to but not including sequence", so one past the marker.
	req, err := json.Marshal(server.JSApiStreamPurgeRequest{
		Subject: p.pre + key, Sequence: revision + 1,
	})
	if err != nil {
		return 0, fmt.Errorf("encode the purge: %w", err)
	}
	reply, err := p.conn.RequestWithContext(ctx, p.subject, req)
	if err != nil {
		return 0, err
	}
	var resp server.JSApiStreamPurgeResponse
	if err := json.Unmarshal(reply.Data, &resp); err != nil {
		return 0, fmt.Errorf("decode the leader's answer: %w", err)
	}
	switch {
	case resp.Error != nil:
		return 0, resp.Error
	case !resp.Success:
		return 0, errors.New("the leader answered the purge with neither success nor an error")
	}
	return resp.Purged, nil
}

// SweepMarkers removes every marker written before cutoff from each bucket the
// broker never ages — see [coord.Markers].
//
// EVERY such bucket rather than the ones known to remove keys: the set is
// derived from the retention each bucket is opened with ([OpenFleet]), so a
// family that starts removing records is swept without anybody remembering
// to add it, and a bucket that holds no marker costs one pass that finds
// none. A bucket the broker ages needs nothing — its markers age out with
// its records. Every bucket is tried even when one fails, for the maintenance
// tick's own reason: one unreachable stream must not stop the rest.
func (f *FleetStore) SweepMarkers(ctx context.Context, cutoff time.Time) (int64, error) {
	var (
		swept int64
		errs  []error
	)
	for _, kv := range f.ageless {
		n, err := sweepMarkers(ctx, f.js, kv, cutoff)
		swept += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return swept, errors.Join(errs...)
}
