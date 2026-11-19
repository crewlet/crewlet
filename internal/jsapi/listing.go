package jsapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// THE PAGED LISTINGS, EACH PAGE ITS OWN READ.
//
// Subjects, stream names and consumers come in pages that only a leader
// answers: a member still believing in a leader its group lost drops each
// without a word. So each PAGE is asked as a metadata read is
// ([jsprovision.ReadBoundedWith]), never the whole listing as one read: asked
// whole, a listing slower than the term restarts from its first page with every
// earlier run still going, each costing the leader the whole listing again,
// while a dropped page costs one page. The client's listers held the whole
// listing to one deadline, which a 13-second million-subject listing never
// meets. Terms: [Leader.Subjects], [consumerListWait]; overlap: [sorted].

// ErrLeaderless is a page a member answered from its own copy while its group
// had no leader — a stream's info is the leader's EXCEPT then
// (server/jetstream_api.go, jsStreamInfoRequest), and a member that had not
// applied the last writes names less than the quorum holds. Refused rather
// than taken: a listing during an election is not one, and its caller treats
// it as a store that did not answer.
var ErrLeaderless = errors.New("jsapi: a member answered from its own copy while " +
	"its group had no leader, which can name less than the quorum holds")

// ErrNoStream is the metadata leader answering that the stream a listing named
// does not exist — an answer, not a failure: a stream deleted between being
// listed and having its consumers listed holds none.
var ErrNoStream = errors.New("jsapi: no such stream")

// consumerListWait is how long the metadata leader waits for the consumers'
// own leaders to answer a CONSUMER.LIST page before it answers with what it
// has and names the rest missing: the vendored server's
// jsClusteredConsumerListRequest, `const timeout = 4 * time.Second`. A page
// is presumed lost only once that wait and a read term have passed, since the
// leader is still gathering it until then. That is five seconds, the client's
// whole bound, so a page whose caller set no deadline is asked once, as the
// client asked it. A stream names page is a sort, the read term's.
const consumerListWait = 4 * time.Second

// requester is the one call a leader read or a listing's page makes of a
// connection: one request, answered by the first reply to it or ended with
// ctx. A *nats.Conn is one.
type requester interface {
	RequestWithContext(ctx context.Context, subject string, data []byte) (*nats.Msg, error)
}

// requesterOf is what the pages of a listing built over js are put on: js
// ITSELF where it answers requests ([Meta]'s seam), and otherwise js's
// connection.
func requesterOf(js jetstream.JetStream) requester {
	if own, ok := js.(requester); ok {
		return own
	}
	return js.Conn()
}

// ask puts one request on subject whose reply takes arrival to reach its
// reader whole once the leader takes it ([Arrival]) — presumed lost after the
// read term and that arrival, and asked again with every earlier request
// still outstanding, held where its caller set no deadline to bound (the
// client's own DefaultTimeout, and the arrival a carrying reader adds to it),
// and never past the lookup ceiling and that arrival
// ([jsprovision.Timing.Arriving], [jsprovision.ReadBoundedWith]) — and answers
// the first reply.
func ask(ctx context.Context, conn requester, subject string, req []byte,
	arrival, bound time.Duration) (*nats.Msg, error) {

	reply, _, err := askWith(ctx, conn, subject, req, jsprovision.Clustered(false).Timing().Arriving(arrival),
		bound)
	return reply, err
}

// askTimed is a request asked as [ask] asks one whose answer arrives at once,
// with term as its term in place of the read term, and how long the request
// whose reply it answers took to be answered: that request's own round trip,
// never the time the requests before it were asked for — a page whose first
// request a member dropped and whose second was answered at once is a page
// the leader answers at once.
func askTimed(ctx context.Context, conn requester, subject string, req []byte,
	term, bound time.Duration) (*nats.Msg, time.Duration, error) {

	timing := jsprovision.Clustered(false).Timing()
	timing.ReadTerm = term
	return askWith(ctx, conn, subject, req, timing, bound)
}

// askWith asks one request on subject at timing's term and ceiling, held
// where its caller set no deadline to bound ([jsprovision.ReadBoundedWith]),
// answering the first reply and that request's own round trip.
func askWith(ctx context.Context, conn requester, subject string, req []byte,
	timing jsprovision.Timing, bound time.Duration) (*nats.Msg, time.Duration, error) {

	type answer struct {
		reply *nats.Msg
		took  time.Duration
	}
	a, err := jsprovision.ReadBoundedWith(ctx, timing, bound, func(ctx context.Context) (answer, error) {
		start := time.Now()
		reply, err := conn.RequestWithContext(ctx, subject, req)
		return answer{reply: reply, took: time.Since(start)}, err
	}, nil)
	return a.reply, a.took, err
}

// page asks one page of a paged listing and decodes its answer into resp,
// reporting whether anybody answered it and how long it took: the round trip
// of the request that was answered, or — where none was — how long the page
// was asked for.
func page(ctx context.Context, conn requester, subject string, req any, term, bound time.Duration,
	resp any) (took time.Duration, heard bool, err error) {

	body, err := json.Marshal(req)
	if err != nil {
		return 0, false, fmt.Errorf("jsapi: encode the page request: %w", err)
	}
	start := time.Now()
	reply, took, err := askTimed(ctx, conn, subject, body, term, bound)
	if err != nil {
		return time.Since(start), false, err
	}
	if err := json.Unmarshal(reply.Data, resp); err != nil {
		return took, true, fmt.Errorf("jsapi: decode the page: %w", err)
	}
	return took, true, nil
}

// sorted walks a listing its leader cuts into pages out of a list sorted anew
// per request, calling visit with each page's entries no earlier page held,
// until the list's end or an error from visit. fetch asks for the page at
// offset, answering its entries by key and the list's size when it was cut.
//
// An entry REMOVED before a boundary moves the next one back into a page
// already read, which offset paging alone never reads. So each later page is
// asked from ONE ENTRY BEFORE where the last ended and must begin at or before
// that entry; one that begins after was cut after a removal and is asked again
// from further back, the step doubling. A repeat is not visited again.
func sorted[T any](fetch func(offset int) (entries map[string]T, total int, err error),
	visit func(fresh map[string]T) error) error {

	var (
		// last is the greatest key a page so far held, and read whether a
		// page has been taken.
		last string
		read bool
		// back is how much further back the next page is asked from,
		// should this one have been cut after a removal.
		back = 1
	)
	for offset := 0; ; {
		entries, total, err := fetch(offset)
		if err != nil {
			return err
		}
		least, greatest := bounds(entries)
		if read && offset > 0 && (len(entries) == 0 || least > last) {
			// CUT AFTER A REMOVAL: an entry between the one the last
			// page ended on and this page's first may be in neither.
			offset = max(offset-back, 0)
			back *= 2
			continue
		}
		fresh := entries
		if read {
			fresh = make(map[string]T, len(entries))
			for key, v := range entries {
				if key > last {
					fresh[key] = v
				}
			}
		}
		if len(fresh) > 0 {
			if err := visit(fresh); err != nil {
				return err
			}
		}
		switch {
		case len(entries) == 0 || offset+len(entries) >= total:
			return nil
		case len(entries) == 1:
			// A page of one entry short of the list's end is no page a
			// leader cuts — each holds hundreds at least — and the next
			// could never overlap it.
			return fmt.Errorf("jsapi: a listing's leader answered a page of one entry "+
				"from %d of %d, short of the list's end", offset, total)
		}
		if !read || greatest > last {
			last, read = greatest, true
		}
		// One entry back: the overlap that shows a removal.
		offset += len(entries) - 1
		back = 1
	}
}

// bounds is the least and the greatest of entries' keys, both empty for none.
func bounds[T any](entries map[string]T) (least, greatest string) {
	first := true
	for key := range entries {
		if first || key < least {
			least = key
		}
		if first || key > greatest {
			greatest = key
		}
		first = false
	}
	return least, greatest
}

// outlasted records that one of l's subjects pages went unanswered for took:
// a page takes at least that long, so the term the next page is given beside
// the read term rises to it — and never falls for it, since a page that went
// unanswered for less than the last one took says nothing that one did not.
// Only an answered page lowers it ([Leader.Subjects]).
func (l *Leader) outlasted(took time.Duration) {
	for {
		was := l.paged.Load()
		if int64(took) <= was || l.paged.CompareAndSwap(was, int64(took)) {
			return
		}
	}
}

// Subjects calls visit with the subjects under filter that the stream's
// LEADER holds a message on, each with its count, a page at a time, until the
// last page or an error from visit. Every subject held throughout is visited
// once ([sorted]); a page a leaderless member answered is [ErrLeaderless].
//
// A page's term is what its answer takes: the leader sorts EVERY subject under
// the filter to cut each page, so each costs the whole listing
// (BenchmarkASubjectListing: 0.13 s a page at 100,000 subjects, 1.26 at a
// million, 4.22 at three million). A page gets the read term beside the round
// trip of the last page answered (the read term alone for a reader's first
// ever); a page nobody answered counts as at least as long as it was asked
// for ([Leader.outlasted]).
func (l *Leader) Subjects(ctx context.Context, filter string, visit func(map[string]uint64) error) error {
	_, err := l.SubjectsSince(ctx, filter, visit)
	return err
}

// SubjectsSince is [Leader.Subjects], answering besides the stream's last
// sequence as the leader stated it on the listing's FIRST page, the earliest
// the listing read: a message stored while later pages were read may be counted
// on one of them but is never left out of a later listing's sequence, so two
// listings' sequences differ for every message stored between their first
// pages. From a later page, a message stored on an earlier page's subject plus
// one let go from it would leave count and sequence as the next listing finds
// them. Within a page the leader states the sequence before it counts the
// subjects (jsStreamInfoRequest), which errs the same way.
func (l *Leader) SubjectsSince(ctx context.Context, filter string,
	visit func(map[string]uint64) error) (uint64, error) {

	var (
		last  uint64
		first = true
	)
	term := jsprovision.ReadTerm + time.Duration(l.paged.Load())
	err := sorted(func(offset int) (map[string]uint64, int, error) {
		var resp server.JSApiStreamInfoResponse
		took, heard, err := page(ctx, l.conn, l.info,
			server.JSApiStreamInfoRequest{ApiPagedRequest: server.ApiPagedRequest{Offset: offset},
				SubjectsFilter: filter}, term, l.timeout, &resp)
		if !heard && !errors.Is(err, nats.ErrNoResponders) {
			// A PAGE NOBODY ANSWERED IS A MEASUREMENT TOO: whatever kept
			// it — a sort longer than its bound, or a member that dropped
			// it — it was not answered in took, so the next listing's
			// first page is given at least that ([Leader.outlasted]).
			// Kept only from the answered pages, a page that outgrew its
			// bound left every listing after it starting from the read
			// term alone, asking the page that cannot be answered in
			// time at none, two and four seconds — three sorts of every
			// subject in flight on the leader where one was enough —
			// for as long as the stream stayed that size. "No
			// responders" reached no leader at all, and says nothing.
			l.outlasted(took)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("jsapi: list %s's subjects under %q from its leader: %w",
				l.stream, filter, err)
		}
		switch {
		case resp.Error != nil:
			return nil, 0, fmt.Errorf("jsapi: list %s's subjects under %q from its leader: %w",
				l.stream, filter, resp.Error)
		case resp.StreamInfo == nil:
			return nil, 0, fmt.Errorf("jsapi: the leader of %s answered a subject listing "+
				"with neither a stream nor an error", l.stream)
		case resp.Cluster != nil && resp.Cluster.Leader == "":
			return nil, 0, fmt.Errorf("jsapi: list %s's subjects under %q: %w", l.stream,
				filter, ErrLeaderless)
		}
		// The next page is the same work, asked of the same leader a
		// moment later — and so is the next listing's first.
		term = jsprovision.ReadTerm + took
		l.paged.Store(int64(took))
		if first {
			last, first = resp.State.LastSeq, false
		}
		return resp.State.Subjects, resp.Total, nil
	}, visit)
	if err != nil {
		return 0, err
	}
	return last, nil
}

// Meta reads the listings only the METADATA leader answers (the stream names,
// a stream's consumers), each page its own read.
//
// The pages go on the client's connection unless the client answers requests
// itself (RequestWithContext, the one call a page makes). No client the engine
// builds does (each is made by [API.Client], and a test holds that); a TEST'S
// FAKE does, so a case can be the metadata leader inside a synctest bubble:
// no broker's reply can reach a request made in one, and a claim about the
// clock is exact only there. The seam is BELOW the hedge, so a page's term,
// bound, re-asks and paging are this package's own under a fake; whether a
// caller's reads are asked again at all is proved at the broker ([jsapitest]).
type Meta struct {
	// conn is what the pages are put on: the client's connection, or the
	// client itself where it answers requests ([requesterOf]).
	conn  requester
	names string
	api   API
	// timeout bounds a page whose context has no deadline — the client's
	// own default, as a [Leader]'s reads are bounded.
	timeout time.Duration
}

// NewMeta addresses the metadata leader's listings in the API js speaks, on
// js's connection — or on js, where js answers requests itself ([Meta]).
func NewMeta(js jetstream.JetStream) (*Meta, error) {
	api, err := Of(js)
	if err != nil {
		return nil, err
	}
	names, err := api.Subject(server.JSApiStreams)
	if err != nil {
		return nil, err
	}
	return &Meta{conn: requesterOf(js), names: names, api: api, timeout: js.Options().DefaultTimeout}, nil
}

// StreamNames calls visit with every stream's name, in the leader's order,
// until visit returns an error, which StreamNames then returns. One page holds
// 1,024 names, and every stream that exists throughout the listing is visited
// once (see this file's head).
func (m *Meta) StreamNames(ctx context.Context, visit func(name string) error) error {
	return sorted(func(offset int) (map[string]struct{}, int, error) {
		var resp server.JSApiStreamNamesResponse
		if _, _, err := page(ctx, m.conn, m.names,
			server.JSApiStreamNamesRequest{ApiPagedRequest: server.ApiPagedRequest{Offset: offset}},
			jsprovision.ReadTerm, m.timeout, &resp); err != nil {
			return nil, 0, fmt.Errorf("jsapi: list the streams from the metadata leader: %w", err)
		}
		if resp.Error != nil {
			return nil, 0, fmt.Errorf("jsapi: list the streams from the metadata leader: %w",
				resp.Error)
		}
		names := make(map[string]struct{}, len(resp.Streams))
		for _, name := range resp.Streams {
			names[name] = struct{}{}
		}
		return names, resp.Total, nil
	}, func(fresh map[string]struct{}) error {
		for _, name := range slices.Sorted(maps.Keys(fresh)) {
			if err := visit(name); err != nil {
				return err
			}
		}
		return nil
	})
}

// Consumer is one consumer a stream's listing named: its info, or — for one
// the broker holds but cannot run — the reason it gave instead.
type Consumer struct {
	Name string
	// Info is the consumer's info, nil for an Offline one.
	Info *jetstream.ConsumerInfo
	// Offline is why the broker will not run the consumer — one a server
	// newer than this one made with what this server does not support —
	// and empty for every other.
	Offline string
}

// UnreadError is a consumers page that named consumers whose own leaders did
// not answer the metadata leader within its wait ([consumerListWait]): the
// listing cannot say what they are, so it is no listing.
type UnreadError struct {
	Stream    string
	Consumers []string
}

func (e *UnreadError) Error() string {
	return fmt.Sprintf("jsapi: the metadata leader listed %d consumer(s) of %s whose own "+
		"leaders did not answer it (%s); list again", len(e.Consumers), e.Stream,
		strings.Join(e.Consumers, ", "))
}

// Consumers calls visit with every consumer of stream, in the leader's order,
// until visit returns an error, which Consumers then returns. One page holds
// 256, and every consumer that exists throughout the listing is visited once
// (see this file's head). A stream the metadata leader does not hold is
// [ErrNoStream].
//
// A CONSUMER WHOSE LEADER DID NOT ANSWER fails the listing ([UnreadError])
// rather than being left out of it: the metadata leader names it missing and
// carries no info for it, and the client's own lister dropped it and then
// counted its pages by the infos they carried, so the consumer after it was
// listed twice and the one it stood for not at all.
func (m *Meta) Consumers(ctx context.Context, stream string, visit func(Consumer) error) error {
	subject, err := m.api.Subject(fmt.Sprintf(server.JSApiConsumerListT, stream))
	if err != nil {
		return err
	}
	type entry struct {
		Consumer
		unread bool
	}
	return sorted(func(offset int) (map[string]entry, int, error) {
		var resp consumerList
		if _, _, err := page(ctx, m.conn, subject,
			server.JSApiConsumersRequest{ApiPagedRequest: server.ApiPagedRequest{Offset: offset}},
			jsprovision.ReadTerm+consumerListWait, m.timeout, &resp); err != nil {
			return nil, 0, fmt.Errorf("jsapi: list %s's consumers from the metadata leader: %w",
				stream, err)
		}
		switch {
		case resp.Error != nil && server.IsNatsErr(resp.Error, server.JSStreamNotFoundErr):
			return nil, 0, fmt.Errorf("jsapi: list %s's consumers: %w", stream, ErrNoStream)
		case resp.Error != nil:
			return nil, 0, fmt.Errorf("jsapi: list %s's consumers from the metadata leader: %w",
				stream, resp.Error)
		}
		// Every name the page covered is answered with an info, named
		// offline, or named missing — one that is missing and still
		// listed is a consumer every peer of which is offline, its info
		// built from the leader's own record of it. A consumer a solo
		// server is closing is answered with none of them, so a page
		// covers what it names and its size is never read: one that
		// names fewer than it covered is asked on from earlier, which an
		// overlap only repeats.
		entries := make(map[string]entry, len(resp.Consumers)+len(resp.Missing))
		for _, info := range resp.Consumers {
			if info != nil {
				entries[info.Name] = entry{Consumer: Consumer{Name: info.Name, Info: info}}
			}
		}
		for _, name := range resp.Missing {
			if _, listed := entries[name]; listed {
				continue
			}
			if reason, offline := resp.Offline[name]; offline {
				entries[name] = entry{Consumer: Consumer{Name: name, Offline: reason}}
				continue
			}
			entries[name] = entry{Consumer: Consumer{Name: name}, unread: true}
		}
		return entries, resp.Total, nil
	}, func(fresh map[string]entry) error {
		names := slices.Sorted(maps.Keys(fresh))
		var unread []string
		for _, name := range names {
			if fresh[name].unread {
				unread = append(unread, name)
			}
		}
		if len(unread) > 0 {
			return &UnreadError{Stream: stream, Consumers: unread}
		}
		for _, name := range names {
			if err := visit(fresh[name].Consumer); err != nil {
				return err
			}
		}
		return nil
	})
}

// consumerList is a CONSUMER.LIST page, its infos in the CLIENT's shape — the
// server's and the client's are one wire format, and every caller reads the
// client's.
type consumerList struct {
	server.ApiResponse
	server.ApiPaged
	Consumers []*jetstream.ConsumerInfo `json:"consumers"`
	Missing   []string                  `json:"missing,omitempty"`
	Offline   map[string]string         `json:"offline,omitempty"`
}
