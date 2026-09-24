package statelog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"
)

// The transfer's subjects. Infrastructure names in an infrastructure estate:
// they are not a domain's, so they carry no domain in their path.
const (
	// SubjectOffer is where a joining node asks who can donate. Every
	// hydrated node may answer.
	SubjectOffer = "crewlet.statelog.snapshot.offer"

	// SubjectFetchPrefix plus a donor's node id is where that donor
	// streams its artefact from. Per donor rather than shared, because a
	// joiner picks ONE offer and fetching from a shared subject would
	// race every other donor into the same reply inbox.
	SubjectFetchPrefix = "crewlet.statelog.snapshot.fetch."
)

// TransferChunkWait bounds the wait for the NEXT chunk, not the transfer.
//
// It only has to outlast a healthy pause — a chunk read from disk plus the
// round trip that returns its credit — because a stalled transfer is a failure
// rather than something to wait out: the artefact is fetched to a temporary
// path and a joiner that gives up simply asks again.
const TransferChunkWait = 30 * time.Second

// OfferWindow is how long a joining node collects offers before choosing.
//
// Long enough that a busy donor answers, short enough that a node below the
// floor — which is refusing every read and every write meanwhile — is not held
// there by a peer that will never reply.
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
	// the JOINER is live on.
	//
	// A SECOND IDENTITY TERM BESIDE THE GENERATION, because the two
	// answer different questions and only one of them is answered by a
	// number the fleet controls. A generation moves when an OPERATOR
	// re-anchors; a stream that is deleted and rebuilt comes back at
	// generation 0 with sequences counting from 1 again, and an artefact
	// from before it matches on generation, matches on record version,
	// matches on replay protocol, and names a history that no longer
	// exists. The broker's own creation instant is the only value that
	// separates them, which is why it is stamped into every checkpoint
	// and carried in every manifest.
	//
	// Zero is NO CLAIM — a joiner that could not read its own stream's
	// instant asks without one rather than refusing every donor.
	StreamCreatedAt map[string]time.Time `json:"stream_created_at,omitempty"`
}

// Offer is what a node answers with.
type Offer struct {
	// Manifest is the artefact's own claim about itself.
	Manifest Manifest `json:"manifest"`

	// Fetch is the subject to fetch this artefact from.
	Fetch string `json:"fetch"`
}

// Usable reports whether this offer can serve the request, and why not.
//
// # Every clause is a refusal a joiner must make BEFORE it transfers
//
// A gigabyte-scale transfer that ends in a refusal is a gigabyte-scale
// transfer nobody needed, and the refusals here are all answerable from the
// manifest alone.
func (o Offer) Usable(req OfferRequest, build map[string]Registered) error {
	if o.Manifest.V != ManifestVersion {
		return fmt.Errorf("the artefact is a version %d manifest and this build "+
			"reads %d", o.Manifest.V, ManifestVersion)
	}
	for name, reg := range build {
		pos, ok := o.Manifest.Domains[name]
		if !ok {
			// AN OFFER MISSING A DOMAIN IS REFUSED, never partially
			// adopted: a build that registers a domain the artefact
			// never named would otherwise conclude it is caught up on
			// it, from a checkpoint the file has no rows for.
			return fmt.Errorf("the artefact names no position for %q, which this "+
				"build registers — an artefact is adopted wholesale, so a domain "+
				"it does not name is one this node would believe it was caught "+
				"up on", name)
		}
		if want := reg.Domain.RecordVersion(); pos.RecordVersion > want {
			return fmt.Errorf("%s was written by a build reading record version "+
				"%d and this one reads %d — its checkpoint sits above records "+
				"this node cannot read, and once those sequences are trimmed it "+
				"never can", name, pos.RecordVersion, want)
		}
		if spec := reg.Domain.Stream(); pos.Replay != spec.Replay {
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
		// at microsecond precision and the broker reports nanoseconds, so
		// comparing them exactly makes every honest artefact look
		// recreated. That rule has one implementation and this is a
		// caller of it.
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
// how a joiner ranks offers.
//
// NEWER IS STRICTLY BETTER and an older offer is never preferable on age: the
// only thing an older artefact buys is a longer replay.
func (o Offer) Newest() uint64 { return newestSeq(o.Manifest.Domains) }

// Dialer opens a connection for a transfer.
//
// ITS OWN CONNECTION, from the same credentials. This tree's own note on the
// projection package says why: "a blocked watch consumer stalls the node's
// single NATS read loop and every seat mailbox with it" — and this transfer is
// tens of gigabytes rather than a memory row. A second connection adds no
// authority (same credentials, same account) and no configuration, and both
// sides open one and close it when the transfer ends.
type Dialer func(ctx context.Context) (*nats.Conn, error)

// DonorDeps is everything the donor half needs.
type DonorDeps struct {
	// NodeID names this donor, and its fetch subject.
	NodeID string

	// Dial opens the transfer's own connection.
	Dial Dialer

	// Newest answers this node's current offer, reporting false when it
	// has nothing to donate. It is a function rather than a value because
	// a node's newest artefact changes under it.
	Newest func() (Manifest, bool)

	// Path answers where an artefact's bytes are, given its manifest.
	Path func(Manifest) string

	// Logger is where this writes. Nil is the package's own component
	// logger, never silence: see loggerOr for what silence cost.
	Logger *slog.Logger
}

// Donor answers offer requests and streams its artefact.
type Donor struct {
	deps DonorDeps
	log  *slog.Logger
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
	return &Donor{deps: d, log: logger}, nil
}

// FetchSubject is where this donor streams from.
func (d *Donor) FetchSubject() string { return SubjectFetchPrefix + d.deps.NodeID }

// Serve answers offer requests and fetches until the context ends.
func (d *Donor) Serve(ctx context.Context) error {
	nc, err := d.deps.Dial(ctx)
	if err != nil {
		return fmt.Errorf("statelog: open the donor's connection: %w", err)
	}
	defer nc.Close()

	offers, err := nc.Subscribe(SubjectOffer, func(msg *nats.Msg) {
		d.answerOffer(ctx, msg)
	})
	if err != nil {
		return fmt.Errorf("statelog: listen for offer requests: %w", err)
	}
	defer func() { _ = offers.Unsubscribe() }()

	fetches, err := nc.Subscribe(d.FetchSubject(), func(msg *nats.Msg) {
		go d.stream(ctx, nc, msg)
	})
	if err != nil {
		return fmt.Errorf("statelog: listen for fetches: %w", err)
	}
	defer func() { _ = fetches.Unsubscribe() }()

	<-ctx.Done()
	return ctx.Err()
}

// answerOffer replies with this node's artefact, or stays silent.
//
// SILENT RATHER THAN A REFUSAL, because a joiner collects for a window and
// takes the best answer: a node with nothing to donate has nothing to say, and
// an explicit "no" would only make the joiner wait for it.
func (d *Donor) answerOffer(ctx context.Context, msg *nats.Msg) {
	m, ok := d.deps.Newest()
	if !ok {
		return
	}
	body, err := json.Marshal(Offer{Manifest: m, Fetch: d.FetchSubject()})
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

// stream sends the artefact to the deliver subject the request names.
func (d *Donor) stream(ctx context.Context, nc *nats.Conn, msg *nats.Msg) {
	deliver := string(msg.Data)
	if deliver == "" {
		return
	}
	m, ok := d.deps.Newest()
	if !ok {
		d.terminate(nc, deliver, 500, "this node holds no snapshot")
		return
	}
	path := d.deps.Path(m)
	file, err := os.Open(path)
	if err != nil {
		d.terminate(nc, deliver, 500, fmt.Sprintf("open the artefact: %v", err))
		return
	}
	defer func() { _ = file.Close() }()

	// THE CREDIT WINDOW. Every chunk carries a reply subject and the
	// donor keeps at most a window's worth outstanding — which is what
	// keeps a pipe full across a real round trip, where strict
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
			// THE PUBLISH ERROR MUST NOT LAND ON THE OUTER err: that
			// one is file.Read's, and the EOF check below the block is
			// what ends the transfer. Assigning here would either end
			// the loop on a successful read or lose the read's own
			// failure.
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

// terminate ends a transfer with a verdict.
//
// AN EMPTY MESSAGE CARRYING A STATUS, which is the shape this tree's other
// chunked transfer uses and for the same reason: every failure arrives looking
// exactly like a normal end of stream, and the header is the only thing
// between that and a truncated file reported as a snapshot.
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

// CollectOffers asks the fleet who can donate and returns what answers,
// newest first.
//
// It collects for a WINDOW rather than taking the first reply, because the
// first reply is the fastest peer rather than the best artefact — and newer is
// strictly better, since the only thing an older one buys is a longer replay.
//
// # What the context means
//
// The window is the joiner's patience, and ctx is its CALLER'S: whichever ends
// first ends the collection. The window ending is the ordinary answer — what
// arrived, possibly nothing. ctx ending is an ERROR wrapping ctx.Err(), never
// an empty result, because "nobody answered" is acted on — through
// [Adopter.Join] a boot comes up on the history it has and a runtime rejoin
// backs off to ask again — and a node whose boot was interrupted or whose
// state log is stopping must do neither. A connection that stops delivering
// is an error for the same reason: a joiner that cannot hear has not been told
// that nobody can donate.
func CollectOffers(ctx context.Context, nc *nats.Conn, req OfferRequest, window time.Duration) ([]Offer, error) {
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
	// both ends. A deadline computed beside ctx — the shape this replaced —
	// is a timer nothing else can interrupt: a SIGTERM during a boot's
	// window, or a Stop during a runtime rejoin, sat out the rest of it.
	collect, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	if err := nc.PublishRequest(SubjectOffer, inbox, body); err != nil {
		return nil, fmt.Errorf("statelog: ask for offers: %w", err)
	}
	// Bounded by the window as well: a broker that has not confirmed the
	// ask by the time the window closes has asked nobody, which is a
	// failure to ask rather than an answer — and it is the WINDOW's end,
	// not the caller's, so it is named rather than wrapped.
	if err := nc.FlushWithContext(collect); err != nil {
		return nil, boundedWaitErr(ctx, err, "flush the offer request",
			fmt.Errorf("statelog: the broker did not confirm the offer request "+
				"within the %s offer window, so no donor was asked", window))
	}

	var out []Offer
	for {
		msg, err := sub.NextMsgWithContext(collect)
		if err != nil {
			// THE CALLER'S END FIRST, and tested on the PARENT: the
			// child is done in both cases, so its own error cannot say
			// whose end it was.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("statelog: collect offers: %w", ctx.Err())
			}
			// The window closed, or the broker reported that nothing
			// subscribes to the subject at all — which is the same
			// answer sooner, and what a lone node hears at boot before
			// its own donor has started.
			if collect.Err() != nil || errors.Is(err, nats.ErrNoResponders) {
				break
			}
			// ANYTHING ELSE IS A JOINER THAT CAN NO LONGER HEAR — a
			// closed connection, a dropped subscription — and ending
			// the loop there would report a fleet it never heard from
			// as a fleet with nothing to give.
			return nil, fmt.Errorf("statelog: collect offers: %w", err)
		}
		var o Offer
		if err := json.Unmarshal(msg.Data, &o); err != nil {
			continue
		}
		out = append(out, o)
	}
	// NEWEST FIRST, so a caller's own refusals run against the best
	// artefact before the worse ones.
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j].Newest() > out[i].Newest() {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

// FetchArtefact streams an offer's bytes to dest and returns what it wrote.
//
// dest must not exist: a partial file from a previous attempt is debris rather
// than a resume point, and the path is deterministic so the debris is always
// this function's own.
//
// Every wait in it is bounded by [TransferChunkWait] as well as by ctx, and
// the two ends are reported differently — see [boundedWaitErr].
func FetchArtefact(ctx context.Context, nc *nats.Conn, offer Offer, dest string) (int64, error) {
	return fetchArtefact(ctx, nc, offer, dest, TransferChunkWait)
}

// fetchArtefact is [FetchArtefact] with the transfer's own bound as a
// parameter. Nothing in the engine passes anything but [TransferChunkWait];
// a test of what happens when that bound fires needs it shorter, or it spends
// thirty seconds proving one comparison.
func fetchArtefact(ctx context.Context, nc *nats.Conn, offer Offer, dest string,
	wait time.Duration) (int64, error) {

	// A CALLER THAT HAS ALREADY GIVEN UP FETCHES NOTHING, for
	// [CollectOffers]'s reason one step later. A request published now
	// sets a donor sending into an inbox this function lets go of as soon
	// as it notices: the donor opens the artefact, starts a goroutine and a
	// credit subscription, publishes up to a full credit window
	// ([SnapshotTransferWindow] chunks of [SnapshotChunkBytes]) that nobody
	// reads, and then holds all three for [TransferChunkWait] waiting for a
	// credit that never comes, before it ends the transfer with a 408.
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("statelog: fetch the artefact: %w", err)
	}
	deliver := nats.NewInbox()
	sub, err := nc.SubscribeSync(deliver)
	if err != nil {
		return 0, fmt.Errorf("statelog: open the transfer inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	// The subscription's own buffer, sized for the credit window: the
	// donor is allowed a window of chunks in flight, so a smaller buffer
	// here drops exactly what the window was for.
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := sub.SetPendingLimits(SnapshotTransferWindow*4,
		SnapshotChunkBytes*SnapshotTransferWindow*4); err != nil {
		return 0, fmt.Errorf("statelog: size the transfer buffer: %w", err)
	}

	//nolint:govet // shadow: scoped to this block; see .golangci.yml
	if err := nc.PublishRequest(offer.Fetch, nats.NewInbox(), []byte(deliver)); err != nil {
		return 0, fmt.Errorf("statelog: ask %s for the artefact: %w", offer.Fetch, err)
	}
	// THE FLUSH TAKES THE CALLER'S CONTEXT, and the patience every other
	// step of a transfer gets. A plain Flush is bounded by the client's
	// own ten seconds and by nothing the caller holds, so a Stop or a
	// signal that landed while the broker was slow to confirm the request
	// sat that out — the one wait in the transfer the caller could not
	// end. The transfer's own bound rather than a figure of its own: a
	// broker that has not confirmed the request in the time a donor is
	// given to send a chunk is a stalled transfer by the same measure,
	// and when that bound is what ends the wait it is reported by name,
	// never as a deadline the caller did not set.
	flushCtx, cancelFlush := context.WithTimeout(ctx, wait)
	err = nc.FlushWithContext(flushCtx)
	cancelFlush()
	if err != nil {
		return 0, boundedWaitErr(ctx, err, "flush the fetch",
			fmt.Errorf("statelog: the broker did not confirm the fetch request "+
				"to %s within %s", offer.Fetch, wait))
	}

	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("statelog: create %s: %w", dest, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
	}()

	var written int64
	for {
		chunk, err := nextTransferChunk(ctx, sub, wait)
		if err != nil {
			_ = os.Remove(dest)
			return 0, err
		}
		if len(chunk.Data) == 0 {
			// THE TERMINATOR CARRIES THE VERDICT. Every failure
			// arrives looking exactly like a normal end of stream.
			if verdict := transferVerdict(chunk); verdict != nil {
				_ = os.Remove(dest)
				return 0, verdict
			}
			break
		}
		n, err := file.Write(chunk.Data)
		if err != nil {
			_ = os.Remove(dest)
			return 0, fmt.Errorf("statelog: write %s: %w", dest, err)
		}
		written += int64(n)
		// THE CREDIT, returned before the next read rather than after
		// it: the donor is waiting on this to send the chunk after
		// next.
		if chunk.Reply != "" {
			if err := chunk.Respond(nil); err != nil {
				_ = os.Remove(dest)
				return 0, fmt.Errorf("statelog: keep the transfer flowing: %w", err)
			}
		}
	}
	if err := file.Sync(); err != nil {
		_ = os.Remove(dest)
		return 0, fmt.Errorf("statelog: flush %s: %w", dest, err)
	}
	closed = true
	if err := file.Close(); err != nil {
		_ = os.Remove(dest)
		return 0, fmt.Errorf("statelog: close %s: %w", dest, err)
	}
	return written, nil
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

// boundedWaitErr says whose end a bounded wait reached.
//
// EACH WAIT IT JUDGES IS BOUNDED TWICE — the ask's flush, the fetch's flush,
// the wait for a chunk: by the caller's context, and by a limit of this
// package's own (the offer window, the chunk wait) set as a child of it.
// Whichever ends first, the wait fails through the child, so its error cannot
// say whose end it was, and reading only that once blamed a donor for a
// caller's own deadline in a message naming a thirty-second wait the caller
// never gave it. So:
//
//   - The CALLER'S end is tested on the parent and wrapped: errors.Is against
//     context.Canceled or context.DeadlineExceeded is how a caller learns its
//     own context ended.
//   - The package's OWN bound is returned as own — named, and deliberately
//     NOT wrapping DeadlineExceeded, because in the chain that would claim
//     the caller's deadline had passed when the caller set none.
//   - Anything else — a closed connection, a dropped subscription — is the
//     transport's own failure, and keeps its %w.
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
