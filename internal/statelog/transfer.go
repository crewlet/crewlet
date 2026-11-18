package statelog

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/words"
)

// The transfer's subjects: infrastructure, not a domain's, so no domain in
// their path.
const (
	// SubjectOffer is where a joining node asks who can donate. Every
	// hydrated node may answer.
	SubjectOffer = "crewlet.statelog.snapshot.offer"

	// SubjectFetchPrefix plus a donor's node id is where that donor streams
	// its artefact from: per donor, because a joiner picks ONE offer and a
	// shared subject would race every donor into its reply inbox. The body
	// is the deliver subject; the chosen artefact rides in
	// [HeaderFetchArtifact].
	SubjectFetchPrefix = "crewlet.statelog.snapshot.fetch."

	// HeaderFetchArtifact names, on a fetch, the artefact the joiner chose
	// from the offer, so the donor streams that copy or refuses rather than
	// whatever it holds by then. A fetch naming none is refused: streaming
	// the newest is the race the name closes. A header, so the body stays
	// the deliver subject alone, read without decoding.
	HeaderFetchArtifact = "Crewlet-Snapshot-Artifact"
)

// TransferChunkWait bounds the wait for the NEXT chunk, not the transfer. It
// only has to outlast a healthy pause (a chunk read from disk plus its
// credit's round trip): a stalled transfer fails rather than being waited out,
// since the artefact lands at a temporary path and a joiner that gives up asks
// again.
const TransferChunkWait = 30 * time.Second

// TransferReportEvery is how often a running transfer says how far it has got
// ([FetchArtefact]).
//
// A join at boot is inside the node's recovery, whose startup probe is sized
// partly by join time, so progress lines are what that sizing measures and
// what tells an operator a slow transfer from a stuck one. TEN SECONDS, the
// broker's own recovery cadence, so one boot's two waits read alike, and the
// longest join a rejoin window admits (a day) stays under nine thousand lines.
const TransferReportEvery = 10 * time.Second

// Fetched is what a transfer wrote: how many bytes, and the SHA-256 of them
// in the order they were written.
type Fetched struct {
	Bytes  int64
	SHA256 string
}

// transferPace is how a fetch waits and reports: [TransferChunkWait] and
// [TransferReportEvery] in production, a value so a case about either can run
// at a scale it can wait out.
type transferPace struct {
	wait   time.Duration
	report time.Duration
	log    *slog.Logger
}

// OfferWindow is the longest a joining node collects offers before choosing:
// long enough for a busy donor to answer, short enough that a node below the
// floor, refusing every read and write meanwhile, is not held by a peer that
// never replies. A CEILING: a joiner that names the donors it expects stops
// once each has answered ([CollectOffers]).
const OfferWindow = 5 * time.Second

// OfferRequest is what a joining node asks with.
type OfferRequest struct {
	// NodeID is who is asking, for the donor's log.
	NodeID string `json:"node_id"`

	// Need is, per domain, the lowest position an artefact must name to
	// be usable: one below the higher of the stream's first surviving
	// sequence and the published trim floor. An offer below it names a
	// resume point whose records are already gone.
	Need map[string]uint64 `json:"need"`

	// Generations is, per domain, the generation the joiner's live stream
	// is on. An artefact from another generation names a different
	// history, however large its sequences look.
	Generations map[string]uint32 `json:"generations"`

	// StreamCreatedAt is, per domain, the creation instant of the stream
	// the JOINER is live on: a second identity term beside the generation,
	// which only an operator's reanchor moves. A stream deleted and rebuilt
	// returns at generation 0 counting from 1, and an artefact from before
	// it matches on generation, record version and replay protocol while
	// naming a history that no longer exists; only the creation instant
	// ([StreamCreatedAt]) separates them, so every checkpoint and manifest
	// carries it. Zero is NO CLAIM: a joiner that could not read its
	// stream's instant asks without one rather than refusing every donor.
	StreamCreatedAt map[string]time.Time `json:"stream_created_at,omitempty"`
}

// Offer is what a donor answers with: the artefact it holds, or — with no
// Fetch — that it holds none ([Offer.Declined]).
type Offer struct {
	// Donor is the node that answered, on an offer and a decline alike,
	// because a joiner counts its expected donors off it
	// ([CollectOffers]). Not the manifest's NodeID, which names who TOOK
	// the artefact and is absent from a decline.
	Donor string `json:"donor"`

	// Manifest is the artefact's own claim about itself, absent on a
	// decline.
	Manifest Manifest `json:"manifest,omitzero"`

	// Fetch is the subject to fetch this artefact from, and empty on a
	// decline.
	Fetch string `json:"fetch,omitempty"`
}

// Declined reports an answer from a donor that holds nothing to offer.
func (o Offer) Declined() bool { return o.Fetch == "" }

// Usable reports whether this offer can serve the request, and why not.
// known is every migration of the replicated estate this binary carries
// ([store.KnownMigrations]).
//
// Every clause is answerable from the manifest, so a joiner refuses BEFORE a
// gigabyte-scale transfer nobody needed. That includes the schema: an offer
// whose manifest lists a migration this binary lacks is refused here rather
// than fetched and refused at adoption, which still checks the file against
// its own list.
func (o Offer) Usable(req OfferRequest, build map[string]Registered, known []string) error {
	if o.Manifest.V != ManifestVersion {
		return fmt.Errorf("the artefact is a version %d manifest and this build "+
			"reads %d", o.Manifest.V, ManifestVersion)
	}
	// EXACTLY THE LOGS THIS BUILD REGISTERS: a log this build does not run
	// is rows no applier here keeps; the loop below refuses a missing one.
	for name := range o.Manifest.Domains {
		if _, ours := build[name]; !ours {
			return fmt.Errorf("the artefact names the log %q, which this build does "+
				"not register — its rows are a history no applier here keeps", name)
		}
	}
	if ahead := aheadOf(o.Manifest.Migrations, known); len(ahead) > 0 {
		return fmt.Errorf("the artefact carries migrations this binary does not: "+
			"%s — its rows are shaped by code this node does not run", words.And(ahead))
	}
	for name, reg := range build {
		pos, ok := o.Manifest.Domains[name]
		if !ok {
			// AN OFFER MISSING A DOMAIN IS REFUSED, never partially
			// adopted: this node would believe itself caught up on a
			// domain the file has no rows for.
			return fmt.Errorf("the artefact names no position for %q, which this "+
				"build registers — an artefact is adopted wholesale, so a domain "+
				"it does not name is one this node would believe it was caught "+
				"up on", name)
		}
		if want := reg.Domain.RecordVersion(); pos.RecordVersion > want {
			return fmt.Errorf("%s's rows were applied from records up to version "+
				"%d and this build reads %d — its checkpoint sits above records "+
				"this node cannot read, and once those sequences are trimmed it "+
				"never can", name, pos.RecordVersion, want)
		}
		if spec := reg.Spec; pos.Replay != spec.Replay {
			return fmt.Errorf("%s was written under the %q replay protocol and "+
				"this build declares %q — adopting a compacted position into a "+
				"strict loop is a permanent stall", name, pos.Replay, spec.Replay)
		}
		if gen, named := req.Generations[name]; named && pos.Generation != gen {
			return fmt.Errorf("%s was taken at generation %d and this node's "+
				"stream is on %d — the sequences name a different history",
				name, pos.Generation, gen)
		}
		// THROUGH [IdentityOf], never a bare Equal: the instant is stored
		// at microseconds and the broker reports nanoseconds, so an exact
		// compare makes every honest artefact look recreated.
		if created, named := req.StreamCreatedAt[name]; named &&
			IdentityOf(pos.StreamCreatedAt, created, true) == StreamRecreated {

			return fmt.Errorf("%s was taken while its donor was applying the "+
				"stream created at %s and this node's stream was created at "+
				"%s — a stream that was rebuilt counts from 1 again, so the "+
				"artefact's sequences name a history this one does not have",
				name, pos.StreamCreatedAt.UTC().Format(time.RFC3339Nano),
				created.UTC().Format(time.RFC3339Nano))
		}
		if need, named := req.Need[name]; named && pos.Seq < need {
			return fmt.Errorf("%s names position %d and this node needs at least "+
				"%d — an artefact below the floor resumes at records that are "+
				"already gone", name, pos.Seq, need)
		}
	}
	return nil
}

// Newest is the highest position any domain in this offer reached, which is
// how a joiner ranks offers: newer is strictly better, since all an older
// artefact buys is a longer replay.
func (o Offer) Newest() uint64 { return newestSeq(o.Manifest.Domains) }

// Dialer opens a connection for a transfer.
//
// ITS OWN CONNECTION, from the same credentials: a blocked consumer stalls a
// connection's single read loop and every seat mailbox with it (the projection
// package's note), and a transfer is tens of gigabytes. A second connection
// adds no authority and no configuration; both sides open one and close it
// when the transfer ends.
type Dialer func(ctx context.Context) (*nats.Conn, error)

// DonorDeps is everything the donor half needs.
type DonorDeps struct {
	// NodeID names this donor, and its fetch subject.
	NodeID string

	// Dial opens the transfer's own connection.
	Dial Dialer

	// Newest answers this node's current artefact, false when it has none
	// to donate: a function, because the newest artefact changes under it.
	Newest func() (Manifest, bool)

	// Path answers where an artefact's bytes are, given its manifest.
	Path func(Manifest) string

	// MaxTransfers bounds how many fetches this node streams at once; zero
	// takes [DefaultMaxTransfers]. Each transfer holds a goroutine, an open
	// file and a credit window, and every joiner in a fleet restarting
	// together can reach the fetch subject at once. Over the bound a fetch
	// is TERMINATED with a reason, never queued: a joiner told no asks the
	// next donor, while one on a queue it cannot see spends its window on
	// a node that will not answer.
	MaxTransfers int

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence (see loggerOr).
	Logger *slog.Logger
}

// DefaultMaxTransfers is how many artefacts one node streams at once.
//
// FOUR, a disk number rather than a network one: a transfer is a sequential
// read of one large file, and a node serving four is already seeking between
// them. Not derived from GOMAXPROCS: the donor's disk and the joiners'
// patience do not scale with cores.
const DefaultMaxTransfers = 4

// Donor answers offer requests and streams its artefact.
type Donor struct {
	deps DonorDeps
	log  *slog.Logger

	// transfers admits at most [DonorDeps.MaxTransfers] concurrent
	// streams; a send that cannot proceed is refused, never waited on.
	transfers chan struct{}

	// serving is whether the server has confirmed [Donor.Serve] is
	// listening ([Donor.Serving]).
	serving atomic.Bool
}

// NewDonor builds the donor half.
func NewDonor(d DonorDeps) (*Donor, error) {
	switch {
	case d.NodeID == "":
		return nil, fmt.Errorf("statelog: a donor with no node id has no fetch subject")
	case d.Dial == nil:
		return nil, fmt.Errorf("statelog: a donor cannot open a transfer connection")
	case d.Newest == nil || d.Path == nil:
		return nil, fmt.Errorf("statelog: a donor has nothing to offer")
	}
	logger := loggerOr(d.Logger)
	limit := d.MaxTransfers
	if limit <= 0 {
		limit = DefaultMaxTransfers
	}
	return &Donor{deps: d, log: logger, transfers: make(chan struct{}, limit)}, nil
}

// admissionRefused tells a joiner this donor is full, so it asks the next one.
func (d *Donor) admissionRefused(nc *nats.Conn, msg *nats.Msg) {
	deliver := string(msg.Data)
	if deliver == "" {
		// Nowhere to answer, as [Donor.stream] has nowhere either.
		return
	}
	d.log.Info("statelog_donation_refused",
		"node", d.deps.NodeID, "limit", cap(d.transfers),
		"detail", "this node is already streaming as many artefacts as it will "+
			"at once; the joiner was told so rather than queued, so it can ask "+
			"another donor inside its own collection window")
	// 429, NOT 503: the broker sends 503 for "no responders", and the
	// client turns it into [nats.ErrNoResponders] before a subscriber sees
	// the description, so the joiner would read "nobody is listening".
	d.terminate(nc, deliver, 429,
		fmt.Sprintf("this donor is streaming %d artefact(s) already, which is its "+
			"limit; ask another node", cap(d.transfers)))
}

// FetchSubject is where this donor streams from.
func (d *Donor) FetchSubject() string { return SubjectFetchPrefix + d.deps.NodeID }

// ErrDonorConnectionClosed is what [Donor.Serve] returns when NATS closed its
// connection for good (no further reconnect). The donor had subscribed,
// confirmed or not ([Donor.Serving]), so the caller redials rather than
// waiting out a failure.
var ErrDonorConnectionClosed = errors.New("statelog: the donor's connection was closed")

// Serve answers offer requests and fetches until the context ends, or until
// the connection it dialled is closed under it ([ErrDonorConnectionClosed]).
//
// It returns on that close rather than waiting on ctx alone: a connection the
// client closed for good (a chunk past a lowered max_payload, credentials no
// longer accepted) would leave the donor subscribed to nothing while the
// snapshot register still advertises its artefact. The node runs on, since a
// donor's loss is not the node's; only the connection's owner can notice, and
// the engine's donor loop redials.
func (d *Donor) Serve(ctx context.Context) error {
	nc, err := d.deps.Dial(ctx)
	if err != nil {
		return fmt.Errorf("statelog: open the donor's connection: %w", err)
	}
	defer nc.Close()
	// BEFORE THE SUBSCRIPTIONS, so no close goes unheard: a listener hears
	// only later changes, and a connection already closed is refused at
	// Subscribe ([nats.ErrConnectionClosed]), failing Serve as a setup.
	// That is the only way an arrived-closed connection is told, never as
	// [ErrDonorConnectionClosed]: the engine's loop redials that within a
	// second, logging each time, while a dial that gave the donor nothing
	// is redialled on the doubling wait.
	closed := nc.StatusChanged(nats.CLOSED)
	defer nc.RemoveStatusListener(closed)

	offers, err := nc.Subscribe(SubjectOffer, func(msg *nats.Msg) {
		d.answerOffer(ctx, msg)
	})
	if err != nil {
		return fmt.Errorf("statelog: listen for offer requests: %w", err)
	}
	defer func() { _ = offers.Unsubscribe() }()

	fetches, err := nc.Subscribe(d.FetchSubject(), func(msg *nats.Msg) {
		select {
		case d.transfers <- struct{}{}:
			go func() {
				defer func() { <-d.transfers }()
				d.stream(ctx, nc, msg)
			}()
		default:
			// REFUSED NOW, never queued ([DonorDeps.MaxTransfers]).
			d.admissionRefused(nc, msg)
		}
	})
	if err != nil {
		return fmt.Errorf("statelog: listen for fetches: %w", err)
	}
	defer func() { _ = fetches.Unsubscribe() }()

	// SERVING ONCE THE SERVER HAS BOTH SUBSCRIPTIONS, not when Subscribe
	// returns, which only queues the interest: a join on this node asks on
	// ANOTHER connection, and could publish first and wait out the whole
	// [OfferWindow] for an answer nobody was asked for.
	defer d.serving.Store(false)
	if err := d.confirmListening(ctx, nc); err != nil {
		return err
	}
	d.serving.Store(true)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-closed:
		return donorConnectionClosed(nc)
	}
}

// donorConnectionClosed is [ErrDonorConnectionClosed] carrying what the client
// last saw on the connection, which is why the server closed it.
func donorConnectionClosed(nc *nats.Conn) error {
	if last := nc.LastError(); last != nil {
		return fmt.Errorf("%w: %w", ErrDonorConnectionClosed, last)
	}
	return ErrDonorConnectionClosed
}

// confirmListening returns once the server has acknowledged everything nc has
// sent so far ([Donor.Serve]'s two subscriptions), when ctx ends, or when nc
// has closed for good.
//
// A failed confirmation is ASKED AGAIN, never ends the donor: the
// subscriptions stay on the connection and are resent on reconnect, so the
// donor answers meanwhile and only waits to SAY it serves, and ending Serve
// over a moment's slow broker loses the donor for the node's life. Each
// attempt is bounded by [OfferWindow], the longest a joiner listens, so longer
// buys nothing. No pause between attempts: one fails at once only when the
// connection just dropped, and the next ping waits in the reconnect buffer.
func (d *Donor) confirmListening(ctx context.Context, nc *nats.Conn) error {
	unconfirmed := false
	for {
		attempt, cancel := context.WithTimeout(ctx, OfferWindow)
		err := nc.FlushWithContext(attempt)
		cancel()
		switch {
		case err == nil:
			if unconfirmed {
				d.log.InfoContext(ctx, "statelog_donor_serving", "node", d.deps.NodeID)
			}
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case nc.IsClosed():
			// CLOSED UNDER A DONOR THAT HAD SUBSCRIBED, so
			// [ErrDonorConnectionClosed], which its loop redials promptly;
			// one that arrived closed was refused at Subscribe instead.
			return fmt.Errorf("statelog: the server had not confirmed the "+
				"donor's subscriptions (%v): %w", err, donorConnectionClosed(nc))
		}
		// SAID ONCE, not at every attempt: a broker that does not answer
		// for minutes is reported by everything else that talks to it.
		if !unconfirmed {
			unconfirmed = true
			d.log.WarnContext(ctx, "statelog_donor_unconfirmed",
				"node", d.deps.NodeID, "error", err.Error(),
				"detail", "the server has not confirmed this node's donor is "+
					"listening, so a join on this node does not wait for it; "+
					"asked again until it does")
		}
	}
}

// Serving reports whether this donor is answering offer requests: from the
// server's confirmation of [Donor.Serve]'s subscriptions until Serve returns.
//
// A join on this node asks it before naming this node among the donors it
// waits for ([AdoptDeps.Donors]): a lone node below the floor ends its
// collection on its own donor's answer, but at BOOT the join runs before any
// donor starts, and naming itself there would wait out the [OfferWindow].
func (d *Donor) Serving() bool { return d.serving.Load() }

// answerOffer replies with this node's artefact, or with a decline when it
// holds none.
//
// A DECLINE RATHER THAN SILENCE: a joiner that knows whom to expect stops once
// each has answered ([CollectOffers]), and silence looks like a donor that has
// not heard yet, holding the joiner, refusing reads and writes, for the whole
// [OfferWindow]. The commonest such donor is the joiner's own, on a node that
// has never snapshotted.
//
// THE REQUEST IS NOT READ: every data node keeps the one replicated estate,
// and whether an offer is usable is the joiner's to judge ([Offer.Usable]).
func (d *Donor) answerOffer(ctx context.Context, msg *nats.Msg) {
	answer := Offer{Donor: d.deps.NodeID}
	if m, ok := d.deps.Newest(); ok {
		answer.Manifest, answer.Fetch = m, d.FetchSubject()
	}
	body, err := json.Marshal(answer)
	if err != nil {
		d.log.WarnContext(ctx, "statelog_snapshot_offer_failed",
			"node", d.deps.NodeID, "error", err.Error())
		return
	}
	if err := msg.Respond(body); err != nil {
		d.log.WarnContext(ctx, "statelog_snapshot_offer_failed",
			"node", d.deps.NodeID, "error", err.Error())
	}
}

// stream sends the artefact the fetch names to the deliver subject its body
// names.
//
// THE ARTEFACT THE JOINER CHOSE, OR NONE: a copy taken since the offer is
// refused with 410 rather than streamed and refused after by a checksum it
// could never match, and a fetch naming no artefact ([HeaderFetchArtifact])
// with 400, since the newest is not what it was offered.
func (d *Donor) stream(ctx context.Context, nc *nats.Conn, msg *nats.Msg) {
	deliver := string(msg.Data)
	if deliver == "" {
		return
	}
	var artifact string
	if msg.Header != nil {
		artifact = msg.Header.Get(HeaderFetchArtifact)
	}
	if artifact == "" {
		d.terminate(nc, deliver, 400, "the fetch names no artefact; ask for offers again")
		return
	}
	m, ok := d.deps.Newest()
	switch {
	case !ok:
		d.terminate(nc, deliver, 404, "this node holds no snapshot")
		return
	case m.Artifact != artifact:
		d.terminate(nc, deliver, 410, fmt.Sprintf("the artefact %s was replaced by "+
			"%s since it was offered; ask for offers again", artifact, m.Artifact))
		return
	}
	path := d.deps.Path(m)
	file, err := os.Open(path)
	if err != nil {
		d.terminate(nc, deliver, 500, fmt.Sprintf("open the artefact: %v", err))
		return
	}
	defer func() { _ = file.Close() }()

	// THE CREDIT WINDOW: every chunk carries a reply subject and at most a
	// window's worth is outstanding, keeping the pipe full where
	// stop-and-wait would cap the transfer at one chunk per round trip.
	credits, err := nc.SubscribeSync(nats.NewInbox())
	if err != nil {
		d.terminate(nc, deliver, 500, fmt.Sprintf("open the credit inbox: %v", err))
		return
	}
	defer func() { _ = credits.Unsubscribe() }()

	buf := make([]byte, SnapshotChunkBytes)
	outstanding := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		for outstanding >= SnapshotTransferWindow {
			if _, err := credits.NextMsg(TransferChunkWait); err != nil {
				d.terminate(nc, deliver, 408,
					fmt.Sprintf("the recipient stopped returning credits: %v", err))
				return
			}
			outstanding--
		}
		n, err := file.Read(buf)
		if n > 0 {
			chunk := nats.NewMsg(deliver)
			chunk.Reply = credits.Subject
			chunk.Data = append([]byte(nil), buf[:n]...)
			// THE PUBLISH ERROR MUST NOT LAND ON THE OUTER err, which is
			// file.Read's and which the EOF check below reads: it would
			// end the loop on a good read or lose the read's failure.
			//nolint:govet // shadow: deliberate; see the paragraph above
			if err := nc.PublishMsg(chunk); err != nil {
				d.terminate(nc, deliver, 500, fmt.Sprintf("send a chunk: %v", err))
				return
			}
			outstanding++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			d.terminate(nc, deliver, 500, fmt.Sprintf("read the artefact: %v", err))
			return
		}
	}
	// EVERY OUTSTANDING CREDIT IS DRAINED before the terminator, so the
	// verdict cannot overtake a chunk still in flight.
	for outstanding > 0 {
		if _, err := credits.NextMsg(TransferChunkWait); err != nil {
			d.terminate(nc, deliver, 408,
				fmt.Sprintf("the recipient stopped returning credits: %v", err))
			return
		}
		outstanding--
	}
	d.terminate(nc, deliver, 204, "")
	d.log.InfoContext(ctx, "statelog_snapshot_sent",
		"node", d.deps.NodeID, "bytes", m.Bytes, "sha256", m.SHA256)
}

// terminate ends a transfer with a verdict: an empty message carrying a
// status, since a failure otherwise looks exactly like a normal end of stream
// and the header is all that keeps a truncated file from passing as a
// snapshot.
func (d *Donor) terminate(nc *nats.Conn, deliver string, status int, detail string) {
	msg := nats.NewMsg(deliver)
	msg.Header = nats.Header{}
	msg.Header.Set("Status", strconv.Itoa(status))
	if detail != "" {
		msg.Header.Set("Description", detail)
	}
	_ = nc.PublishMsg(msg)
	_ = nc.Flush()
}

// CollectOffers asks the fleet who can donate and returns the offers, best
// first, equals shuffled so a fleet restarting together spreads over donors
// ([rankOffers]); a decline ([Offer.Declined]) is counted, never returned. It
// collects for a window, since the fastest reply is not the newest artefact.
//
// expect names every donor that can answer ([AdoptDeps.Donors]); the collection
// ends once each has, else at the window. It must be a SUPERSET of who can
// offer: one it omits is heard only while a named one is still to answer.
// Whichever of the window and ctx ends first ends the collection, but ctx
// ending, like a connection that stops delivering, is an ERROR, never an empty
// result: "nobody answered" brings a boot up on its own history or backs a
// rejoin off, and an interrupted or deaf node must do neither.
func CollectOffers(ctx context.Context, nc *nats.Conn, req OfferRequest, window time.Duration,
	expect ...string) ([]Offer, error) {
	var named func(context.Context) ([]string, error)
	if len(expect) > 0 {
		named = func(context.Context) ([]string, error) { return expect, nil }
	}
	return collectOffers(ctx, nc, req, window, named)
}

// collectOffers is [CollectOffers] with its expectation READ BESIDE the
// collection: expect runs once, on a context the window bounds, while offers
// arrive, and donors that answered before it returned count. Nil expects
// nobody. Beside, not before, because [Adopter.Join]'s expectation is a
// coordination read, which run first would add a slow store's time ahead of
// the window on a node refusing every read and write while it joins. A
// listing still running at the window's close is abandoned; one that fails or
// names nobody leaves the window to decide.
//
// expect must return once its context ends: the collection waits for it, and
// nothing a collection starts outlives the call.
func collectOffers(ctx context.Context, nc *nats.Conn, req OfferRequest, window time.Duration,
	expect func(context.Context) ([]string, error)) ([]Offer, error) {
	// A CALLER THAT HAS ALREADY GIVEN UP ASKS NOBODY: a request published
	// now is one every donor answers for a joiner that will not read it.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("statelog: ask for offers: %w", err)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("statelog: encode an offer request: %w", err)
	}
	inbox := nats.NewInbox()
	sub, err := nc.SubscribeSync(inbox)
	if err != nil {
		return nil, fmt.Errorf("statelog: open the offer inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()

	// THE WINDOW IS A CHILD OF THE CALLER'S CONTEXT, so one wait observes
	// both ends: a SIGTERM or a Stop ends it, as a separate deadline could
	// not.
	collect, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	if err := nc.PublishRequest(SubjectOffer, inbox, body); err != nil {
		return nil, fmt.Errorf("statelog: ask for offers: %w", err)
	}
	// Bounded by the window too: a broker that has not confirmed the ask
	// by then asked nobody, a failure rather than an answer, named as the
	// window's end rather than wrapped as the caller's.
	if err := nc.FlushWithContext(collect); err != nil {
		return nil, boundedWaitErr(ctx, err, "flush the offer request",
			fmt.Errorf("statelog: the broker did not confirm the offer request "+
				"within the %s offer window, so no donor was asked", window))
	}

	// THE EXPECTATION, read on its own goroutine under the window. Its
	// answer INTERRUPTS the wait below (arrived): every donor it names may
	// have answered already, and a wait for an answer never coming would
	// hold the collection to the window.
	wait := collect
	var expected chan []string
	if expect != nil {
		expected = make(chan []string, 1)
		var arrived context.CancelFunc
		wait, arrived = context.WithCancel(collect)
		listed := make(chan struct{})
		go func() {
			defer close(listed)
			defer arrived()
			named, err := expect(collect)
			if err != nil {
				named = nil
			}
			expected <- named
		}()
		defer func() { cancel(); <-listed }()
	}
	// waiting is nil until the expectation is known, and from then the
	// donors it names that have not answered.
	var waiting map[string]bool
	answered := map[string]bool{}
	var out []Offer
	take := func(msg *nats.Msg) {
		var o Offer
		if err := json.Unmarshal(msg.Data, &o); err != nil {
			return
		}
		answered[o.Donor] = true
		delete(waiting, o.Donor)
		if !o.Declined() {
			out = append(out, o)
		}
	}
	for waiting == nil || len(waiting) > 0 {
		msg, err := sub.NextMsgWithContext(wait)
		if err != nil {
			// THE CALLER'S END FIRST, and tested on the PARENT: the
			// child is done in both cases, so its own error cannot say
			// whose end it was.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("statelog: collect offers: %w", ctx.Err())
			}
			// THE EXPECTATION ARRIVING, window still open, its answer sent
			// before the interruption: fold it in, less whoever has
			// answered, and wait on the window alone. An answer that came
			// with it stays in the subscription, read next turn or in the
			// drain below.
			if expected != nil && wait.Err() != nil && collect.Err() == nil {
				named := <-expected
				expected, wait = nil, collect
				if len(named) > 0 {
					waiting = make(map[string]bool, len(named))
					for _, donor := range named {
						if !answered[donor] {
							waiting[donor] = true
						}
					}
				}
				continue
			}
			// The window closed, or the broker reported no subscriber at
			// all: the same answer sooner, which a lone node hears at boot
			// before its own donor starts.
			if collect.Err() != nil || errors.Is(err, nats.ErrNoResponders) {
				break
			}
			// ANYTHING ELSE IS A JOINER THAT CAN NO LONGER HEAR (a closed
			// connection, a dropped subscription), never a fleet with
			// nothing to give.
			return nil, fmt.Errorf("statelog: collect offers: %w", err)
		}
		take(msg)
	}
	// WHAT HAS ALREADY ARRIVED IS READ BEFORE RETURNING, without waiting: a
	// wait whose context ended answers that end even with a message pending,
	// so an answer arriving in the same instant (from an unnamed donor, or
	// as the window closed) would be dropped, perhaps the only usable offer.
	for {
		msg, err := sub.NextMsg(0)
		if err != nil {
			break
		}
		take(msg)
	}
	// THE CALLER'S CANCELLATION is answered as itself, never with whatever
	// arrived before it.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("statelog: collect offers: %w", err)
	}
	return rankOffers(out, rand.Uint64()), nil
}

// FetchArtefact streams an offer's bytes to dest and returns what it wrote.
// dest must not exist: its path is deterministic, so a file there is this
// function's debris, never a resume point. Every wait is bounded by
// [TransferChunkWait] and by ctx, reported apart ([boundedWaitErr]).
//
// It logs `statelog_join_fetching` once the donor is asked (donor, size,
// [ProjectJoinSeconds]) and `statelog_join_progress` every
// [TransferReportEvery], which a join's startup probe is sized from; logger
// nil is the package's own. The checksum ([Fetched.SHA256]) is taken as bytes
// are written, in file order, so no uncancellable second read follows. A
// failure creating, writing, syncing or closing dest is this node's disk
// ([nodeFault]), ending the join rather than refusing this donor.
func FetchArtefact(ctx context.Context, nc *nats.Conn, offer Offer, dest string,
	logger *slog.Logger) (Fetched, error) {

	return fetchArtefact(ctx, nc, offer, dest, transferPace{
		wait: TransferChunkWait, report: TransferReportEvery, log: loggerOr(logger),
	})
}

// fetchArtefact is [FetchArtefact] with its pace a parameter, so a case about
// the bound firing or the progress lines runs short.
func fetchArtefact(ctx context.Context, nc *nats.Conn, offer Offer, dest string,
	pace transferPace) (Fetched, error) {

	wait := pace.wait

	// A CALLER THAT HAS ALREADY GIVEN UP FETCHES NOTHING: a request now
	// has a donor publish a full credit window nobody reads, then hold its
	// file, goroutine and credit subscription for [TransferChunkWait]
	// before its 408.
	if err := ctx.Err(); err != nil {
		return Fetched{}, fmt.Errorf("statelog: fetch the artefact: %w", err)
	}
	deliver := nats.NewInbox()
	sub, err := nc.SubscribeSync(deliver)
	if err != nil {
		return Fetched{}, fmt.Errorf("statelog: open the transfer inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	// Sized for the credit window: a smaller buffer drops exactly the
	// chunks the window lets the donor have in flight.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := sub.SetPendingLimits(SnapshotTransferWindow*4,
		SnapshotChunkBytes*SnapshotTransferWindow*4); err != nil {
		return Fetched{}, fmt.Errorf("statelog: size the transfer buffer: %w", err)
	}

	// THE BODY IS THE DELIVER SUBJECT, and the artefact chosen rides in a
	// header ([HeaderFetchArtifact]).
	fetch := nats.NewMsg(offer.Fetch)
	fetch.Reply = nats.NewInbox()
	fetch.Data = []byte(deliver)
	fetch.Header.Set(HeaderFetchArtifact, offer.Manifest.Artifact)
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := nc.PublishMsg(fetch); err != nil {
		return Fetched{}, fmt.Errorf("statelog: ask %s for the artefact: %w", offer.Fetch, err)
	}
	// THE FLUSH TAKES THE CALLER'S CONTEXT, so a Stop or signal ends it (a
	// plain Flush waits the client's own ten seconds), bounded by the chunk
	// wait: a broker that has not confirmed in the time a donor gets per
	// chunk is a stalled transfer, reported by name, never as a deadline
	// the caller did not set.
	flushCtx, cancelFlush := context.WithTimeout(ctx, wait)
	err = nc.FlushWithContext(flushCtx)
	cancelFlush()
	if err != nil {
		return Fetched{}, boundedWaitErr(ctx, err, "flush the fetch",
			fmt.Errorf("statelog: the broker did not confirm the fetch request "+
				"to %s within %s", offer.Fetch, wait))
	}

	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Fetched{}, ownFault(fmt.Errorf("statelog: create %s: %w", dest, err))
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	donor, total := offer.Manifest.NodeID, offer.Manifest.Bytes
	pace.log.InfoContext(ctx, "statelog_join_fetching",
		"donor", donor, "artifact", offer.Manifest.Artifact, "total_bytes", total,
		"projected_join_seconds", int(ProjectJoinSeconds(total)),
		"detail", "fetching a peer's snapshot of the replicated estate; at boot "+
			"this is inside the node's recovery, so its startup probe waits for it")
	sum := sha256.New()
	started := time.Now()
	reported := started
	var written int64
	for {
		chunk, err := nextTransferChunk(ctx, sub, wait)
		if err != nil {
			_ = os.Remove(dest)
			return Fetched{}, err
		}
		if len(chunk.Data) == 0 {
			// THE TERMINATOR CARRIES THE VERDICT ([Donor.terminate]).
			if verdict := transferVerdict(chunk); verdict != nil {
				_ = os.Remove(dest)
				return Fetched{}, verdict
			}
			break
		}
		n, err := file.Write(chunk.Data)
		if err != nil {
			_ = os.Remove(dest)
			return Fetched{}, ownFault(fmt.Errorf("statelog: write %s: %w", dest, err))
		}
		// IN THE ORDER IT REACHED THE FILE — a hash.Hash never fails.
		sum.Write(chunk.Data[:n])
		written += int64(n)
		if now := time.Now(); now.Sub(reported) >= pace.report {
			reported = now
			elapsed := now.Sub(started)
			pace.log.InfoContext(ctx, "statelog_join_progress",
				"donor", donor, "bytes", written, "total_bytes", total,
				"elapsed_seconds", int(elapsed.Seconds()),
				"bytes_per_second", int64(float64(written)/elapsed.Seconds()))
		}
		// THE CREDIT, returned before the next read: the donor waits on
		// it to send the chunk after next.
		if chunk.Reply != "" {
			if err := chunk.Respond(nil); err != nil {
				_ = os.Remove(dest)
				return Fetched{}, fmt.Errorf("statelog: keep the transfer flowing: %w", err)
			}
		}
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(dest)
		return Fetched{}, ownFault(fmt.Errorf("statelog: flush %s: %w", dest, err))
	}
	closed = true
	if err := file.Close(); err != nil {
		_ = os.Remove(dest)
		return Fetched{}, ownFault(fmt.Errorf("statelog: close %s: %w", dest, err))
	}
	return Fetched{Bytes: written, SHA256: hex.EncodeToString(sum.Sum(nil))}, nil
}

// nextTransferChunk waits for the next message, bounded by wait as well as by
// ctx.
func nextTransferChunk(ctx context.Context, sub *nats.Subscription,
	wait time.Duration) (*nats.Msg, error) {

	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	msg, err := sub.NextMsgWithContext(waitCtx)
	if err != nil {
		return nil, boundedWaitErr(ctx, err, "receive a chunk",
			fmt.Errorf("statelog: the donor stopped sending mid-transfer "+
				"(no chunk for %s)", wait))
	}
	return msg, nil
}

// boundedWaitErr says whose end a bounded wait reached. Each wait (the ask's
// flush, the fetch's flush, a chunk) is bounded by the caller's ctx and by
// this package's own limit set as its child, so the child's error cannot say
// which ended it:
//
//   - the CALLER'S end is tested on the parent and wrapped, so errors.Is
//     against context.Canceled or DeadlineExceeded tells the caller;
//   - the package's OWN bound returns own, named and deliberately NOT
//     wrapping DeadlineExceeded, which would claim a deadline the caller
//     never set;
//   - anything else (a closed connection, a dropped subscription) is the
//     transport's, kept with %w.
func boundedWaitErr(ctx context.Context, err error, step string, own error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("statelog: %s: %w", step, ctx.Err())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return own
	}
	return fmt.Errorf("statelog: %s: %w", step, err)
}

// transferVerdict reads the terminator's status.
func transferVerdict(msg *nats.Msg) error {
	if msg.Header == nil {
		return nil
	}
	status := msg.Header.Get("Status")
	if status == "" || status == "204" {
		return nil
	}
	detail := msg.Header.Get("Description")
	if detail == "" {
		detail = "no reason given"
	}
	return fmt.Errorf("statelog: the donor abandoned the transfer (%s): %s",
		status, detail)
}

// rankOffers puts the best artefact first and shuffles equals, so joiners
// restarting together do not all pick one donor past its
// [DonorDeps.MaxTransfers]. The seed is a parameter so a case can assert the
// spread; the collection passes a fresh one per join. The shuffle runs BEFORE
// the stable sort, so order between positions is exact and among equals is
// this joiner's own; shuffling after would undo the sort.
func rankOffers(offers []Offer, seed uint64) []Offer {
	spread := rand.New(rand.NewPCG(seed, 0))
	spread.Shuffle(len(offers), func(i, j int) { offers[i], offers[j] = offers[j], offers[i] })
	slices.SortStableFunc(offers, func(a, b Offer) int {
		return cmp.Compare(b.Newest(), a.Newest())
	})
	return offers
}
