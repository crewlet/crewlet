package kv

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue"
)

// ---- the bridged-call log ---------------------------------------------- //
//
// Two key classes in one ageless bucket, both built by [coord.DocumentKey]
// because a turn id and a launch id are values from elsewhere and a subject
// token cannot carry every byte they can:
//
//	call.<turn>.<launch>.<seq>         one call's record, created once and
//	                                   never written again
//	call.<turn>.<launch>.<seq>.<part>  one part of the whole of that call,
//	                                   filed UNDER its key, created once
//	next.<turn>.<launch>               the launch's numbering: the last Seq
//	                                   handed out, advanced by compare-and-set
//
// The CLASS LEADS, so each is a wildcard the broker can match: a launch's
// calls are `call.<turn>.<launch>.>`, which the counter beside them — one
// segment shorter — never matches.
//
// A PART IS ONE SEGMENT DEEPER THAN ITS CALL, and every reader here tells the
// two apart by that depth alone ([bridgeCallSeq] reads a call at exactly four
// segments, [bridgePartAddress] a part at exactly five). The same filter that
// selects a launch's calls selects their parts, so the purge that removes a
// launch's calls removes every part beneath them, on every build. And the
// depth is what makes a newer build's parts safe to put in front of a build
// that predates them: that build decodes calls with this same [bridgeCallSeq],
// which answers false for a five-segment key, so it lists, counts and pages a
// newer build's log without ever taking a part for a call, and its purge of a
// launch removes the parts with the calls. What such a build cannot do is list
// a launch that holds nothing BUT parts ([FleetStore.BridgeLaunches] here
// does), so a part filed after its launch was purged waits for a sweep run by
// a build that knows parts.

const (
	bridgeCallClass = "call"
	bridgeNextClass = "next"

	// kvStreamPrefix is what the NATS key-value protocol names a bucket's
	// stream: KV_<bucket>. A purge by subject is a STREAM operation, so it
	// needs the stream the bucket is.
	kvStreamPrefix = "KV_"
)

func bridgeCallKey(turnID, launchID string, seq uint64) string {
	return coord.DocumentKey(bridgeCallClass, turnID, launchID, strconv.FormatUint(seq, 10))
}

// bridgePartKey is one part of call seq's whole: the call's own key and one
// segment more, which is what files it under the call.
func bridgePartKey(turnID, launchID string, seq uint64, part int) string {
	return coord.DocumentKey(bridgeCallClass, turnID, launchID,
		strconv.FormatUint(seq, 10), strconv.Itoa(part))
}

func bridgeNextKey(turnID, launchID string) string {
	return coord.DocumentKey(bridgeNextClass, turnID, launchID)
}

func bridgeCallFilter(turnID, launchID string) string {
	return coord.DocumentFilter(bridgeCallClass, turnID, launchID)
}

// bridgeCallSeq recovers a call key's Seq, reporting false for a key this
// backend did not write under that launch — a part's key included, which is
// one segment deeper than a call's.
func bridgeCallSeq(key string) (uint64, bool) {
	segs, ok := coord.DocumentSegments(key)
	if !ok || len(segs) != 4 || segs[0] != bridgeCallClass {
		return 0, false
	}
	seq, err := strconv.ParseUint(segs[3], 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// bridgePartAddress recovers a part key's call Seq and part, reporting false
// for any other key, a call's included.
func bridgePartAddress(key string) (uint64, int, bool) {
	segs, ok := coord.DocumentSegments(key)
	if !ok || len(segs) != 5 || segs[0] != bridgeCallClass {
		return 0, 0, false
	}
	seq, err := strconv.ParseUint(segs[3], 10, 64)
	if err != nil || seq == 0 {
		return 0, 0, false
	}
	part, err := strconv.Atoi(segs[4])
	if err != nil || part < 1 {
		return 0, 0, false
	}
	return seq, part, true
}

// AppendBridgeCall files one call under its launch.
//
// THE NUMBER FIRST, THEN THE RECORD, and neither is a guess. The number is
// taken by a compare-and-set on the launch's counter, so two appends that land
// together get two numbers; the record is a Create, so a call is written once.
// A Create that finds its key taken is a record a purge missed — one a late
// append left after the counter it was numbered by was purged — and the next
// number is taken rather than the stray overwritten.
func (f *FleetStore) AppendBridgeCall(ctx context.Context, turnID, launchID string, value []byte) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	if err := withinCeiling("a bridged call", value); err != nil {
		return 0, err
	}
	for range fleetCASRetries {
		seq, err := f.nextBridgeSeq(ctx, turnID, launchID)
		if err != nil {
			return 0, err
		}
		created, err := f.createBridgeKey(ctx, "a bridged call", bridgeCallKey(turnID, launchID, seq), value)
		if err != nil {
			return 0, err
		}
		if created {
			return seq, nil
		}
	}
	return 0, contended("append", turnID+"/"+launchID)
}

// ReserveBridgeCall takes the launch's next number and files nothing at it.
func (f *FleetStore) ReserveBridgeCall(ctx context.Context, turnID, launchID string) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	return f.nextBridgeSeq(ctx, turnID, launchID)
}

// CreateBridgeCall files a call's record at a number ReserveBridgeCall took.
func (f *FleetStore) CreateBridgeCall(ctx context.Context, turnID, launchID string, seq uint64, value []byte) (bool, error) {
	if err := validBridgeAddress(turnID, launchID, seq, 1); err != nil {
		return false, err
	}
	if err := withinCeiling("a bridged call", value); err != nil {
		return false, err
	}
	return f.createBridgeKey(ctx, "a bridged call", bridgeCallKey(turnID, launchID, seq), value)
}

// CreateBridgeCallPart files one part of a call's whole under the call.
func (f *FleetStore) CreateBridgeCallPart(ctx context.Context, turnID, launchID string, seq uint64, part int, value []byte) (bool, error) {
	if err := validBridgeAddress(turnID, launchID, seq, part); err != nil {
		return false, err
	}
	if err := withinCeiling("a part of a bridged call", value); err != nil {
		return false, err
	}
	return f.createBridgeKey(ctx, "a part of a bridged call", bridgePartKey(turnID, launchID, seq, part), value)
}

// createBridgeKey creates one key of the log, reporting false when it is
// already there: every key here is written once, and one that is taken is a
// record or a part a purge missed, which is never overwritten. what names the
// value for an error, as [withinCeiling] does.
func (f *FleetStore) createBridgeKey(ctx context.Context, what, key string, value []byte) (bool, error) {
	_, err := f.calls.Create(ctx, key, value)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	default:
		// The limit is read off the LIVE connection, after the refusal: a
		// reconnect in between names the server the connection is on now.
		return false, createRefusal(err, what, len(value), f.js.Conn().MaxPayload())
	}
}

// withinCeiling refuses a value past the contract's ceiling.
//
// REFUSED HERE rather than by the client, so the refusal names the contract's
// ceiling instead of the connection's, and so the memory twin and this backend
// refuse at the same length.
func withinCeiling(what string, value []byte) error {
	if len(value) > coord.MaxBridgeCallBytes {
		return fmt.Errorf("coord/kv: %s of %d bytes is past the %d-byte ceiling one record "+
			"may hold (coord.MaxBridgeCallBytes): %w",
			what, len(value), coord.MaxBridgeCallBytes, coord.ErrTooLarge)
	}
	return nil
}

// createRefusal classifies a call record or part the broker did not take:
// what names it, size is its value's length, and accepts is the max_payload the
// server this connection is on announces.
//
// THE CLIENT'S SIZE REFUSAL IS PERMANENT. It measures a message against the
// max_payload the server announced, and [OpenFleet] refuses a server that
// announced less than the contract's ceiling — but a client re-reads that
// announcement on every reconnect, so a cluster member configured below it is
// met at run time, not at boot. Answered as [coord.ErrUnavailable] it would
// read as a blip worth retrying; the same bytes are refused the same way every
// time, so it is [coord.ErrTooLarge], naming the setting that caused it.
//
// IT NAMES THE SERVER'S OWN LIMIT, beside the contract's, because the limit
// the client enforced is the server's: a refusal that named only the contract
// would send a reader looking for an oversized value, when a value within the
// contract's ceiling was refused and the fault is a server's setting.
//
// Read after the refusal, off the live connection, so it is what the server
// the connection is on NOW announces, and the error says so rather than that
// the value exceeds it: a reconnect in between replaces the announcement the
// value was measured against. A limit at or above the contract's is always
// such a replacement, since no value within the contract's ceiling reaches it
// with the headers a create carries.
func createRefusal(err error, what string, size int, accepts int64) error {
	if !errors.Is(err, nats.ErrMaxPayload) {
		return unavailable("record "+what, err)
	}
	limit := fmt.Sprintf("the server this connection is on announces %d bytes, below the %d bytes "+
		"a coordination record is sized against (queue.MaxPayloadBytes)", accepts, queue.MaxPayloadBytes)
	if accepts >= queue.MaxPayloadBytes {
		limit = fmt.Sprintf("the server this connection is on announces %d bytes, at least the %d "+
			"bytes a coordination record is sized against (queue.MaxPayloadBytes), so the value was "+
			"measured against an announcement the connection has since replaced, from a server set "+
			"lower", accepts, queue.MaxPayloadBytes)
	}
	return fmt.Errorf("coord/kv: %s of %d bytes was refused by the NATS client, which measures a "+
		"value and the headers its create carries against the max_payload the server announces; "+
		"%s: set max_payload to at least %d on every server of the cluster: %w: %w",
		what, size, limit, queue.MaxPayloadBytes, coord.ErrTooLarge, err)
}

// nextBridgeSeq takes the launch's next number.
func (f *FleetStore) nextBridgeSeq(ctx context.Context, turnID, launchID string) (uint64, error) {
	key := bridgeNextKey(turnID, launchID)
	for range fleetCASRetries {
		entry, err := f.calls.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			_, err = f.calls.Create(ctx, key, []byte("1"))
			switch {
			case err == nil:
				return 1, nil
			case errors.Is(err, jetstream.ErrKeyExists):
				continue
			default:
				return 0, unavailable("number the bridged call", err)
			}
		}
		if err != nil {
			return 0, unavailable("read the bridged-call numbering", err)
		}
		last, err := strconv.ParseUint(string(entry.Value()), 10, 64)
		if err != nil {
			return 0, unavailable("decode the bridged-call numbering", err)
		}
		next := last + 1
		_, err = f.calls.Update(ctx, key, []byte(strconv.FormatUint(next, 10)), entry.Revision())
		switch {
		case err == nil:
			return next, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
			// Another append took this number, or a purge took the
			// counter: re-read rather than guess.
			continue
		default:
			return 0, unavailable("number the bridged call", err)
		}
	}
	return 0, contended("number", turnID+"/"+launchID)
}

// BridgeCalls returns every call of one launch, in Seq order, with its parts.
//
// ONE WALK carries the records and their parts together: the launch's filter
// selects both, since a part is filed under its call, and this is the read a
// caller makes every call whole from — so reading the parts again call by call
// would move each of them twice.
func (f *FleetStore) BridgeCalls(ctx context.Context, turnID, launchID string) ([]coord.BridgeCallRecord, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return nil, err
	}
	out := []coord.BridgeCallRecord{}
	parts := map[uint64][]coord.BridgeCallPart{}
	err := f.eachUnder(ctx, f.calls, bridgeCallFilter(turnID, launchID), "the bridged calls",
		func(kve jetstream.KeyValueEntry) error {
			if seq, ok := bridgeCallSeq(kve.Key()); ok {
				out = append(out, coord.BridgeCallRecord{
					TurnID: turnID, LaunchID: launchID, Seq: seq,
					// COPIED: the entry's buffer is the client's.
					Value: slices.Clone(kve.Value()),
				})
				return nil
			}
			if seq, part, ok := bridgePartAddress(kve.Key()); ok {
				parts[seq] = append(parts[seq], coord.BridgeCallPart{
					Part: part, Value: slices.Clone(kve.Value()),
				})
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	// The walk delivers in stream order, which is the order the records
	// were COMMITTED; two appends that took consecutive numbers can commit
	// the other way round.
	slices.SortFunc(out, func(a, b coord.BridgeCallRecord) int { return cmp.Compare(a.Seq, b.Seq) })
	for i := range out {
		// Each call takes the parts filed under ITS number, in part order.
		// Parts under a number with no record belong to no call, and are
		// left for the purge of their launch.
		found := parts[out[i].Seq]
		slices.SortFunc(found, func(a, b coord.BridgeCallPart) int { return cmp.Compare(a.Part, b.Part) })
		out[i].Parts = found
	}
	return out, nil
}

// BridgeCallPage returns one page of one launch's calls.
//
// THE KEYS ARE LISTED AND ONLY THE PAGE IS READ. A page is a question about a
// window of the log, and the log's values can each be megabytes, so the
// listing leaves them at the broker and a Get fetches each call the page
// carries — which is what makes the byte budget bound what is READ as well
// as what is returned. A [coord.BridgeCallQuery.Last] page is the same walk
// taken from the newest end, which the listing makes as cheap as the oldest.
//
// The listing takes the parts' keys along with the calls' — the launch's
// filter selects both — and [bridgeCallSeq] leaves every one of them out, so
// a part is never paged, read or counted in the Total.
func (f *FleetStore) BridgeCallPage(ctx context.Context, q coord.BridgeCallQuery) (coord.BridgeCallPage, error) {
	if err := validBridgeQuery(q); err != nil {
		return coord.BridgeCallPage{}, err
	}
	var seqs []uint64
	err := eachKeyUnder(ctx, f.calls, bridgeCallFilter(q.TurnID, q.LaunchID), "the bridged calls",
		func(key string) error {
			if seq, ok := bridgeCallSeq(key); ok {
				seqs = append(seqs, seq)
			}
			return nil
		})
	if err != nil {
		return coord.BridgeCallPage{}, err
	}
	slices.Sort(seqs)
	page := coord.BridgeCallPage{Calls: []coord.BridgeCallRecord{}, Total: len(seqs)}
	// What the cursor admits, in the order the page takes it.
	admitted := seqs[sort.Search(len(seqs), func(i int) bool { return seqs[i] > q.After }):]
	if q.Last {
		admitted = slices.Clone(admitted)
		slices.Reverse(admitted)
	}
	weight := 0
	for _, seq := range admitted {
		if len(page.Calls) == q.Limit || (len(page.Calls) > 0 && weight > q.MaxBytes) {
			// Full, or a first call that alone outweighed the budget:
			// what is left is more, without spending a read to say so.
			page.More = true
			break
		}
		entry, err := f.calls.Get(ctx, bridgeCallKey(q.TurnID, q.LaunchID, seq))
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Purged between the listing and the read: the run ended,
			// and a call of an ended run is not one to show.
			continue
		}
		if err != nil {
			return coord.BridgeCallPage{}, unavailable("read a bridged call", err)
		}
		if len(page.Calls) > 0 && weight+len(entry.Value()) > q.MaxBytes {
			page.More = true
			break
		}
		weight += len(entry.Value())
		page.Calls = append(page.Calls, coord.BridgeCallRecord{
			TurnID: q.TurnID, LaunchID: q.LaunchID, Seq: seq, Value: slices.Clone(entry.Value()),
		})
	}
	if q.Last {
		// Taken newest first; handed back in the order the run made them.
		slices.Reverse(page.Calls)
	}
	return page, nil
}

// BridgeLaunches returns every launch that holds any key in the log.
//
// EVERY KIND OF KEY, because each can outlive the others: a launch whose
// counter a purge took can still hold a record a late append left, one whose
// every record-create failed holds only its counter, and one purged while a
// call's parts were being filed can hold nothing but the parts filed after the
// purge. A sweep that listed fewer would never find the rest's strays.
func (f *FleetStore) BridgeLaunches(ctx context.Context) ([]coord.BridgeLaunch, error) {
	seen := map[coord.BridgeLaunch]struct{}{}
	err := eachKeyUnder(ctx, f.calls, jetstream.AllKeys, "the bridged-call launches",
		func(key string) error {
			segs, ok := coord.DocumentSegments(key)
			if !ok || len(segs) < 3 {
				return nil
			}
			switch {
			case segs[0] == bridgeCallClass && (len(segs) == 4 || len(segs) == 5),
				segs[0] == bridgeNextClass && len(segs) == 3:
				seen[coord.BridgeLaunch{TurnID: segs[1], LaunchID: segs[2]}] = struct{}{}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Keys(seen))
	slices.SortFunc(out, func(a, b coord.BridgeLaunch) int {
		return cmp.Or(cmp.Compare(a.TurnID, b.TurnID), cmp.Compare(a.LaunchID, b.LaunchID))
	})
	return out, nil
}

// PurgeBridgeCalls removes every record of one launch, the parts filed under
// them, and its numbering.
//
// A STREAM PURGE BY SUBJECT rather than a key purge per record. A key purge
// leaves a marker message behind for every key, and in a bucket with no age
// each marker is kept for the life of the deployment — one per call, for every
// run the company ever made. A purge by subject removes the messages and
// leaves nothing. The launch's filter selects every key beneath it, so the
// parts go in the same purge as their calls.
//
// RECORDS FIRST, THEN THE COUNTER. A failure between the two leaves the
// counter, which the sweep lists and purges again; the other order would leave
// records with nothing numbering them, which the sweep still finds (see
// [FleetStore.BridgeLaunches]) but a reader of the counter alone would not.
func (f *FleetStore) PurgeBridgeCalls(ctx context.Context, turnID, launchID string) error {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return err
	}
	stream, err := f.js.Stream(ctx, kvStreamPrefix+f.calls.Bucket())
	if err != nil {
		return unavailable("find the bridged-call stream", err)
	}
	subjects := stream.CachedInfo().Config.Subjects
	if len(subjects) != 1 || !strings.HasSuffix(subjects[0], ">") {
		return fmt.Errorf("coord/kv: the bridged-call stream carries subjects %q, not the one "+
			"wildcard a key-value bucket's stream has", subjects)
	}
	prefix := strings.TrimSuffix(subjects[0], ">")
	for _, key := range []string{bridgeCallFilter(turnID, launchID), bridgeNextKey(turnID, launchID)} {
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject(prefix+key)); err != nil {
			return unavailable("purge the bridged calls", err)
		}
	}
	return nil
}

func validBridgeLaunch(turnID, launchID string) error {
	if turnID == "" || launchID == "" {
		return errors.New("coord/kv: a bridged call needs a turn id and a launch id")
	}
	return nil
}

// validBridgeAddress refuses an address nothing numbers: a call is numbered
// from 1 and so is each of its parts. A zero would also build a key this
// backend's own readers refuse ([bridgeCallSeq], [bridgePartAddress]), so it
// would be stored and never read back.
func validBridgeAddress(turnID, launchID string, seq uint64, part int) error {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return err
	}
	if seq == 0 || part < 1 {
		return fmt.Errorf("coord/kv: a bridged call is numbered from 1 and so is each "+
			"part of it, got call %d part %d", seq, part)
	}
	return nil
}

func validBridgeQuery(q coord.BridgeCallQuery) error {
	if err := validBridgeLaunch(q.TurnID, q.LaunchID); err != nil {
		return err
	}
	if q.Limit < 1 || q.MaxBytes < 1 {
		return fmt.Errorf("coord/kv: a page of bridged calls needs a limit and a byte "+
			"budget of at least one, got %d and %d", q.Limit, q.MaxBytes)
	}
	return nil
}
