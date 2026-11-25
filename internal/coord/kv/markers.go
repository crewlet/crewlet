// The MARKERS a removal leaves: a DEL or PURGE message that, at one revision
// per key, replaces the value. A bucket the broker never ages keeps each one,
// a leader read on every listing that delivers it (package doc, "A tombstone
// the pass delivered is not an answer"), so [sweepMarkers] removes them, and
// [createKey] must not depend on one.
//
// # Every removal is a purge
//
// In a bucket no clock ages, a record is removed with a PURGE, not a delete:
// a purge also removes the key's OLDER revisions, whatever history a bucket
// adopted as it exists ([openBucket]) was created with. A listing never
// returns the marker ([visitLive]); it goes after [coord.MarkerRetention].

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

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/jsapi"
)

// createRounds bounds [createKey]'s loop. A create takes one round, two over
// a marker (the refusal, then the write at its revision), three if the marker
// is swept in between. More needs another writer removing the key inside this
// one's read-to-write window, or on a replicated bucket a peer's create of the
// key still IN FLIGHT (refused, and the leader reads nothing until it lands),
// which ends when the peer commits: at three replicas, 200 bursts of 16
// concurrent creates of one key needed at most two writes. Eight is that twice
// over with room; exhausting it is UNKNOWN ([contended]), never the key held,
// since losing says nothing about who holds it.
const createRounds = 8

// errCreateUnanswered marks the one [createKey] failure after which the key
// may hold the value: its own write went out and the answer was not a refusal
// (most often no answer). A MARK, because the loop reads between writes: after
// any other failure every write had been REFUSED, so nothing was written, and
// a caller reporting to a person ([coord.ErrWriteUnknown]) must tell the two
// apart. Package-private: callers that ignore it lose nothing, and one that
// needs it says so in its contract ([FleetStore.putChatReadAt]).
var errCreateUnanswered = errors.New("the create was sent and its answer was not a refusal")

// createKey writes value under key, stating standing with it (nil: none), only
// when the key holds no live value, answering the revision written, or an error
// wrapping [jetstream.ErrKeyExists] when the stream LEADER holds a live value.
// Each refused round asks the leader: a live value is the key existing, a marker
// the revision to write over, nothing (never written, or swept) a write at zero.
// NOT the bucket's Create, whose retry reads a DIRECT get any replica answers
// (package doc, "Every single-key read is the leader's"), returns a replicated
// stream's refusal as a bare API error (a lost race read as an outage), and
// reports a marker swept before its second write as the key existing. Other
// failures are the caller's to call unavailable, marked [errCreateUnanswered] or
// not; [coord.ErrValueTooLarge] returns as is, since nothing was sent.
func createKey(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	key string, value []byte, standing *coord.RunStanding) (uint64, error) {

	// Resolved up front though only a refused create reads, for
	// [eachEntryUnder]'s reason: a client on an API this engine does not
	// speak fails every create, not only those that raced.
	read, err := newLeaderReader(js, kv)
	if err != nil {
		return 0, err
	}
	write := kv.Update
	if standing != nil {
		h, ok := kv.(headedWriter)
		if !ok {
			return 0, fmt.Errorf("coord/kv: create %s/%s: not a bucket settled where it was opened, "+
				"so nothing writes a standing with its value", kv.Bucket(), key)
		}
		write = func(c context.Context, k string, v []byte, at uint64) (uint64, error) {
			return h.update(c, k, v, at, standing)
		}
	}
	var expect uint64 // zero: the key must hold no message at all
	for range createRounds {
		revision, err := write(ctx, key, value, expect)
		switch {
		case err == nil:
			return revision, nil
		case errors.Is(err, coord.ErrValueTooLarge):
			// REFUSED BEFORE IT WAS SENT: nothing may have landed.
			return 0, err
		case !isWrongLastSequence(err):
			// NOT A REFUSAL, so the broker may have taken it: what came
			// back (most often nothing in time) is not its answer.
			return 0, fmt.Errorf("%w: %w", errCreateUnanswered, err)
		}
		// REFUSED, like every round before it: a failure from here on
		// left the key as it found it.
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

// headedWriter is a bucket that writes a standing in the message of a value
// ([settledBucket.update]).
type headedWriter interface {
	update(ctx context.Context, key string, value []byte, revision uint64,
		standing *coord.RunStanding) (uint64, error)
}

// sweepPass is one walk of a bucket the sweep reads, and the purge that
// removes what it finds.
type sweepPass struct {
	keys  map[string]jetstream.KeyValueEntry
	purge *leaderPurge
}

// walkForSweep walks kv whole, each key's metadata and never its value
// ([watchWalk]), for the sweeps of its keys.
//
// UNCERTIFIED, with NO INDEX: a key it misses is swept next tick, so its pass
// ends on the quiet rather than waiting on removals ([passOver]). A marker
// from a lagging replica is still the key's newest message or already
// replaced, and the purge's bound removes it or nothing.
func walkForSweep(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue) (sweepPass, error) {
	what := "the markers in " + kv.Bucket()
	purge, err := newLeaderPurge(js, kv)
	if err != nil {
		return sweepPass{}, fmt.Errorf("coord/kv: sweep %s: %w", what, err)
	}
	keys, err := watchWalk(ctx, kv, jetstream.AllKeys, what)
	if err != nil {
		return sweepPass{}, err
	}
	return sweepPass{keys: keys, purge: purge}, nil
}

// sweepMarkers removes, from one bucket, every delete or purge marker written
// before cutoff, reporting how many messages went ([walkForSweep], then
// [sweepMarkersIn]).
//
// NOT the client's PurgeDeletes, which purges each marker's subject OUTRIGHT
// and so takes a value re-created after its watcher saw the marker. Each
// subject is purged THROUGH the marker's own revision ([leaderPurge.through]),
// a bound the broker applies with no read to race.
func sweepMarkers(ctx context.Context, js jetstream.JetStream, kv jetstream.KeyValue,
	cutoff time.Time) (int64, error) {

	pass, err := walkForSweep(ctx, js, kv)
	if err != nil {
		return 0, err
	}
	return sweepMarkersIn(ctx, kv, pass, cutoff)
}

// sweepMarkersIn removes, from one bucket's pass, every delete or purge marker
// written before cutoff, reporting how many messages went.
//
// A failure stops this bucket's sweep, reporting what it removed beside the
// error: what is left goes next tick, and pressing on past a refusing broker
// would only log the refusal once per marker.
func sweepMarkersIn(ctx context.Context, kv jetstream.KeyValue, walked sweepPass, cutoff time.Time) (int64, error) {
	what := "the markers in " + kv.Bucket()
	pass, purge := walked.keys, walked.purge
	// KEY ORDER, so a failure part-way names the same key on every retry.
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
// ON THE WIRE, not the client's Stream.Purge, which answers only an error:
// the broker's answer counts what was removed, and a marker the pass saw may
// already be replaced, so counting what was ASKED would overreport.
type leaderPurge struct {
	conn    *nats.Conn
	subject string // the PURGE subject of this bucket's stream, in the client's API
	pre     string // the subject prefix a key is written under
	timeout time.Duration
}

// newLeaderPurge addresses the purges of kv's stream in the API js speaks.
func newLeaderPurge(js jetstream.JetStream, kv jetstream.KeyValue) (*leaderPurge, error) {
	api, err := jsapi.Of(js)
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
// answering how many there were. A later message survives: the broker applies
// the bound under the stream's own lock.
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
// EVERY such bucket, not those known to remove keys: the set derives from
// each bucket's retention ([OpenFleet]), so a family that starts removing
// records is swept without anyone adding it; a markerless bucket costs one
// pass. An aged bucket's markers age out with its records. One unreachable
// stream does not stop the rest.
func (f *FleetStore) SweepMarkers(ctx context.Context, cutoff time.Time) (int64, error) {
	var (
		swept int64
		errs  []error
	)
	for _, kv := range f.ageless {
		pass, err := walkForSweep(ctx, f.js, kv)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		n, err := sweepMarkersIn(ctx, kv, pass, cutoff)
		swept += n
		if err != nil {
			errs = append(errs, err)
		}
		if kv.Bucket() != f.runs.Bucket() {
			continue
		}
		// AND THE ONE VALUE A REMOVAL CAN LEAVE: a sandbox run's part (log,
		// handed set, leads) whose record went while its node stopped first.
		// Found in the SAME pass, so the logs (up to half a megabyte each)
		// are walked once a tick, not twice.
		n, err = f.sweepRunParts(ctx, pass, cutoff)
		swept += n
		if err != nil {
			errs = append(errs, err)
		}
		// AND A MINTED SESSION'S CLAIM, which carries its run's record's
		// lifetime as a part does.
		n, err = f.sweepSessionClaims(ctx, pass, cutoff)
		swept += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return swept, errors.Join(errs...)
}

// SweepBodies removes the activation's bodies no pointer names, written before
// cutoff — see [coord.ActivationBodies]. Its own walk of the config bucket,
// metadata only, at its own horizon ([coord.BodyRetention]).
func (f *FleetStore) SweepBodies(ctx context.Context, cutoff time.Time) (int64, error) {
	pass, err := walkForSweep(ctx, f.js, f.config)
	if err != nil {
		return 0, err
	}
	return f.sweepBodies(ctx, pass, cutoff)
}

// sweepBodies removes, from the config bucket's pass, every activation body
// written before cutoff that the pointer, read AFTER the pass, does not name,
// each purged through the revision the pass saw ([leaderPurge.through]).
//
// SAFE AGAINST EVERY FLIP: one that already landed is in the pointer read
// here; one still coming names a body written within [flipBy] of it, and that
// write either moved the body's revision past the pass (the purge removes
// nothing newer) or came before it, so the body is younger than a cutoff
// [coord.BodyRetention] back. The other half of the retention is for clock
// skew. A pointer that cannot be read keeps every body this tick.
func (f *FleetStore) sweepBodies(ctx context.Context, walked sweepPass, cutoff time.Time) (int64, error) {
	what := "the activation's bodies"
	record, _, held, unread := f.pointer(ctx)
	if unread != nil {
		return 0, fmt.Errorf("coord/kv: sweep %s: %w", what, unread)
	}
	named := ""
	if held {
		named = bodyKey(record.Body)
	}
	keys := make([]string, 0)
	for key, kve := range walked.keys {
		class, _ := coord.KeyClass(key)
		if class != coord.BodyClass || key == named || kve.Operation() != jetstream.KeyValuePut ||
			!kve.Created().Before(cutoff) {
			continue
		}
		keys = append(keys, key)
	}
	// KEY ORDER, so a failure part-way names the same key on every retry.
	slices.Sort(keys)
	var swept int64
	for _, key := range keys {
		n, err := walked.purge.through(ctx, key, walked.keys[key].Revision())
		if err != nil {
			return swept, unavailable("sweep "+what, fmt.Errorf("remove %q: %w", key, err))
		}
		swept += int64(n)
	}
	return swept, nil
}
