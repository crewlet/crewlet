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
	logger := d.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
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
func CollectOffers(ctx context.Context, nc *nats.Conn, req OfferRequest, window time.Duration) ([]Offer, error) {
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

	if err := nc.PublishRequest(SubjectOffer, inbox, body); err != nil {
		return nil, fmt.Errorf("statelog: ask for offers: %w", err)
	}
	if err := nc.Flush(); err != nil {
		return nil, fmt.Errorf("statelog: flush the offer request: %w", err)
	}

	deadline := time.Now().Add(window)
	var out []Offer
	for time.Now().Before(deadline) {
		msg, err := sub.NextMsg(time.Until(deadline))
		if err != nil {
			break
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
func FetchArtefact(ctx context.Context, nc *nats.Conn, offer Offer, dest string) (int64, error) {
	deliver := nats.NewInbox()
	sub, err := nc.SubscribeSync(deliver)
	if err != nil {
		return 0, fmt.Errorf("statelog: open the transfer inbox: %w", err)
	}
	defer func() { _ = sub.Unsubscribe() }()
	// The subscription's own buffer, sized for the credit window: the
	// donor is allowed a window of chunks in flight, so a smaller buffer
	// here drops exactly what the window was for.
	if err := sub.SetPendingLimits(SnapshotTransferWindow*4,
		SnapshotChunkBytes*SnapshotTransferWindow*4); err != nil {
		return 0, fmt.Errorf("statelog: size the transfer buffer: %w", err)
	}

	if err := nc.PublishRequest(offer.Fetch, nats.NewInbox(), []byte(deliver)); err != nil {
		return 0, fmt.Errorf("statelog: ask %s for the artefact: %w", offer.Fetch, err)
	}
	if err := nc.Flush(); err != nil {
		return 0, fmt.Errorf("statelog: flush the fetch: %w", err)
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
		chunk, err := nextTransferChunk(ctx, sub)
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

// nextTransferChunk waits for the next message, bounded.
func nextTransferChunk(ctx context.Context, sub *nats.Subscription) (*nats.Msg, error) {
	waitCtx, cancel := context.WithTimeout(ctx, TransferChunkWait)
	defer cancel()
	msg, err := sub.NextMsgWithContext(waitCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("statelog: the donor stopped sending "+
				"mid-transfer (no chunk for %s)", TransferChunkWait)
		}
		return nil, fmt.Errorf("statelog: receive a chunk: %w", err)
	}
	return msg, nil
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
