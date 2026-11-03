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
// Three of the API's reads come back in pages: a stream's subjects under a
// filter (100,000 a page, from the stream's leader), the stream names and a
// stream's consumers (1,024 and 256 a page, from the metadata leader). Only
// a leader answers any of them, so each is dropped without a word by a member
// that still believes in a leader its group has just lost, exactly as a
// message read is — and so each PAGE is asked as a metadata read is
// ([jsprovision.ReadBoundedWith]): presumed lost after its term, asked again
// with every earlier request still outstanding, the first answer taken, held
// where its caller set no deadline to the client's own five-second default,
// and never past the lookup ceiling. The client's own listers asked each page
// once and held the WHOLE listing for the caller's deadline, or for the
// client's five seconds where the caller had none — which a listing of a
// million subjects, thirteen seconds long (below), could never finish inside:
// the object collector's sweep runs on its duty's lifetime with no deadline,
// so a bucket that size could never be listed. A page is the unit the bound
// is kept for, as it is the unit asked again.
//
// # Never a whole listing as one read
//
// The client's listers send a page, wait for it, send the next. Asked whole
// through the hedge, a listing whose answer takes longer than the term is
// presumed lost and started again from its first page, every earlier one
// still running and each costing the leader the whole listing again, until
// the ceiling ends them all — and a subjects listing does take that long once
// a stream holds enough subjects (below). A page is the unit the broker
// answers, so a page is what is asked again: a dropped page costs one page,
// and a slow one is never asked again as every page before it.
//
// # A page's term is what its answer legitimately takes
//
// A subjects page is not a fixed cost: the leader gathers and sorts EVERY
// subject under the filter to cut each page out of them
// (server/jetstream_api.go, jsStreamInfoRequest), so every page of a listing
// costs what the whole listing does, and that grows faster than the set.
// Measured on one solo member's file store over the object store's own piece
// subjects (BenchmarkASubjectListing, one listing each on four cores), a page
// came back in 0.13 seconds at a hundred thousand subjects, 0.39 at three
// hundred thousand, 1.26 at a million and 4.22 at three million — a whole
// listing 0.13, 1.36 (four pages), 13.2 (eleven) and 120 seconds
// (thirty-one), and a page reaches the five seconds a caller with no
// deadline keeps at about three and a half million. So a page is given the
// read term beside what the last page this reader asked took — the same
// work, asked of the same leader a moment later, measured as the round trip
// of the request that was answered — and a reader's first page ever the read
// term alone, the one figure nothing has measured: a listing of millions of
// subjects asks its first page again once for every two seconds the page
// takes, and never a page after it. A page NOBODY ANSWERED is measured too,
// as at least as long as it was asked for, and only an answered page lowers
// the figure again: a page that outgrows its bound is asked again inside it
// only the first time, and every listing after it asks it once — where,
// learnt from the answered pages alone, each began from the read term and
// asked it at none, two and four seconds, three sorts of every subject on the
// leader, for a page that could not be answered in time anyway.
//
// A consumers page is gathered by a scatter to every consumer's own leader,
// which the metadata leader waits up to four seconds for
// ([consumerListWait]), so its term is that wait beside the read term — five
// seconds, the client's whole bound, so a page whose caller set no deadline
// is asked once inside it, as the client asked it, and only a caller that
// waits longer has it asked again. A stream names page is a sort of the
// names, the read term's.
//
// # Every entry there throughout is listed, and none twice
//
// The leader cuts each page out of a sorted list it builds for that request
// alone, so a page boundary is an offset into a list that moves between two
// requests: an entry REMOVED before the boundary moves the one after it back
// across it, into a page already read, and offset paging alone — the client's
// own, which counted what it had to find the next page's offset — never reads
// it. So each later page is asked from ONE ENTRY BEFORE where the last ended,
// and must begin at or before the entry the last ended on: a page that begins
// after it was cut after a removal, and is asked again from further back, the
// step doubling, until one overlaps (a page from the list's start always
// does). Nothing between two overlapping pages can be in neither, since each
// page is a contiguous run of one sorted list. An entry a page repeats from
// those before is not visited again, and one added or removed while the
// listing ran may be visited or not. The overlap costs a quiet listing one
// entry a page — a page more only where the last would have held fewer
// entries than there were pages before it (a million subjects exactly, 11
// pages for 10) — and a removal across a boundary a page more for each
// doubling of the step.

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
// leader is still gathering it until then.
const consumerListWait = 4 * time.Second

// ask puts one request on subject — presumed lost after term and asked again
// with every earlier request still outstanding, held where its caller set no
// deadline to bound, the client's own DefaultTimeout, and never past the
// lookup ceiling ([jsprovision.ReadBoundedWith]) — and answers the first
// reply.
func ask(ctx context.Context, conn *nats.Conn, subject string, req []byte,
	term, bound time.Duration) (*nats.Msg, error) {

	reply, _, err := askTimed(ctx, conn, subject, req, term, bound)
	return reply, err
}

// askTimed is [ask], and how long the request whose reply it answers took to
// be answered: that request's own round trip, never the time the requests
// before it were asked for — a page whose first request a member dropped and
// whose second was answered at once is a page the leader answers at once.
func askTimed(ctx context.Context, conn *nats.Conn, subject string, req []byte,
	term, bound time.Duration) (*nats.Msg, time.Duration, error) {

	type answer struct {
		reply *nats.Msg
		took  time.Duration
	}
	timing := jsprovision.Clustered(false).Timing()
	timing.ReadTerm = term
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
func page(ctx context.Context, conn *nats.Conn, subject string, req any, term, bound time.Duration,
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

// sorted walks a listing its leader cuts into pages out of one list it sorts
// by key for each request, calling visit with each page's entries that no
// page before it held, until a page reaches the list's end or visit returns
// an error, which sorted then returns. fetch asks for the page at offset,
// answering its entries by key and how many the whole list held when it was
// cut. See this file's head for what it guarantees and why it overlaps.
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
// LEADER holds a message on, each with how many it holds there, a page at a
// time, until the last page or until visit returns an error, which Subjects
// then returns. Each page is its own read, and every subject that holds a
// message throughout the listing is visited once (see this file's head); a
// page answered by a member of a group with no leader is [ErrLeaderless].
func (l *Leader) Subjects(ctx context.Context, filter string, visit func(map[string]uint64) error) error {
	term := jsprovision.ReadTerm + time.Duration(l.paged.Load())
	return sorted(func(offset int) (map[string]uint64, int, error) {
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
		return resp.State.Subjects, resp.Total, nil
	}, visit)
}

// Meta reads the listings only the METADATA leader answers — the stream
// names, a stream's consumers — each page its own read (see this file's
// head).
type Meta struct {
	conn  *nats.Conn
	names string
	api   API
	// timeout bounds a page whose context has no deadline — the client's
	// own default, as a [Leader]'s reads are bounded.
	timeout time.Duration
}

// NewMeta addresses the metadata leader's listings in the API js speaks.
func NewMeta(js jetstream.JetStream) (*Meta, error) {
	api, err := Of(js)
	if err != nil {
		return nil, err
	}
	names, err := api.Subject(server.JSApiStreams)
	if err != nil {
		return nil, err
	}
	return &Meta{conn: js.Conn(), names: names, api: api, timeout: js.Options().DefaultTimeout}, nil
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
