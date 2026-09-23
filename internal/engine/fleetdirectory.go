package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DIRECTORY A NODE THAT RUNS NO IDENTITY DOMAIN READS: the fleet's.
//
// # Why a seats-only node needs one at all
//
// A seats-only satellite does not apply the identity log, so its copy of the
// directory is empty — and it still runs everything the party registry feeds.
// It consumes inbound deliveries (the inbound group is fleet-wide, so the node
// that wins a delivery is whichever node asked first), it runs seats whose
// turns render a roster and resolve colleagues, and its transports mention
// people by the accounts the registry holds. Built from the chart alone, its
// registry went on attributing a suspended person's Slack messages to their
// seat for every delivery it happened to consume, and its agents went on
// addressing them, while every ingress node had withdrawn them within one
// apply. A suspension's promise held on some nodes of a fleet and not others,
// which is the same as not holding.
//
// Reading its own empty copy as "nobody holds any seat" is the other wrong
// answer — it is right only when the company genuinely has nobody bound — so
// the node ASKS: every node that runs the identity domain serves who holds each
// seat, from its own replicated rows, over the broker's ephemeral scatter
// ([queue.EventQueue.Ask]) — a query, not an event, so nothing is retained and
// nothing is audited per read. The satellite takes the answer from the
// most-caught-up node that replied, which is the reading closest to the log.
//
// # Three answers, and the third is not a guess
//
//   - A node running the domain answers: that is the reading.
//   - Nobody answers, and the identity LOG has never carried a record: the
//     company has bound nobody to anything, so the empty reading is a FACT
//     rather than a fallback — every fleet provisions every domain's stream,
//     and a log with no record in it is a directory with no row in it anywhere.
//     This is what an all-seats fleet reads, which is a deployment with no
//     sign-in surface and therefore nobody to suspend.
//   - Nobody answers and the log HAS records: some node ran the domain and
//     none is answering now. That is UNKNOWN, and the registry treats it as
//     every unreadable directory is treated — the last reading carried
//     forward, or [notify.Unread] if there has never been one.
//
// # How fast a suspension reaches it
//
// There is no committed hook here — the node applies nothing on this log — so
// the reading is taken on the directory trigger's periodic net,
// [DirectoryRefresh], and on every published company. A suspension therefore
// reaches a satellite within one refresh interval rather than within one
// apply, which is the stall threshold the alarm table already uses.

// holdersProtocol is the directory scatter's payload version.
//
// ONE, and it moves only for a reshape: a new field needs no bump, because an
// older peer ignores it. A peer that does not speak the asker's version
// answers nothing, which the asker counts as a node that did not reply.
const holdersProtocol = 1

// fleetDirectoryBudget bounds one scatter.
//
// [statelog.ReadBudget], the two seconds a linearizable read is promised, and
// for the same composition: a broker round trip plus one local read on the
// answering node — here an index range over the seat bindings and the removal
// tombstones. A scatter that has not heard back inside it is taken with
// whatever did arrive, and one that heard nothing is the unknown arm, which the
// next [DirectoryRefresh] retries.
const fleetDirectoryBudget = statelog.ReadBudget

// holdersRequest is one scattered question. It carries nothing but the
// version: the question is always "who holds every seat".
type holdersRequest struct {
	Version int `json:"version"`
}

// holdersReply is one node's answer.
type holdersReply struct {
	Version int    `json:"version"`
	Node    string `json:"node"`

	// At is the answering node's identity checkpoint, PACKED
	// ([statelog.Position.Packed]), so two answers compare across a
	// reanchor: the generation is in the high bits. It is read BEFORE the
	// rows, so it is a floor under what the answer saw.
	At int64 `json:"at"`

	Holders []holderWire `json:"holders"`
}

// holderWire is one binding on the wire — the FACT, never the verdict, and
// never the person: which seat the binding names, by the seat's IDENTITY (the
// handle it was created under, ADR-0020), and at what stage. Who holds it stays
// on the nodes that hold the directory.
type holderWire struct {
	Seat    string `json:"seat"`
	Stage   string `json:"stage,omitempty"`
	Removed bool   `json:"removed,omitempty"`
}

// holdersSource is the identity estate as the scatter's answerer reads it.
type holdersSource interface {
	SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error)
	At() statelog.Position
}

// serveHolders makes this node one of the fleet's directory answerers.
//
// Every node that runs the identity domain serves, whether or not a satellite
// ever asks: a scatter has no roster, and the node that happens to be up is
// the one a satellite needs.
func serveHolders(ctx context.Context, q queue.EventQueue, node string,
	src holdersSource) (queue.Unsubscribe, error) {

	if q == nil || src == nil {
		return nil, errors.New("engine: serve the directory with no broker or " +
			"no directory")
	}
	return q.Serve(ctx, topics.IamHolders, func(ctx context.Context, raw []byte) ([]byte, error) {
		var req holdersRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, fmt.Errorf("engine: decode a directory request: %w", err)
		}
		if req.Version != holdersProtocol {
			return nil, fmt.Errorf("engine: directory request at version %d, "+
				"this build speaks %d", req.Version, holdersProtocol)
		}
		at := src.At()
		holders, err := src.SeatHolders(ctx)
		if err != nil {
			// ANSWERS NOTHING, which the asker reads as a node that did
			// not reply — never as an empty directory.
			log.WarnContext(ctx, "directory_answer_failed", "error", err)
			return nil, err
		}
		reply := holdersReply{
			Version: holdersProtocol, Node: node, At: at.Packed(),
			Holders: make([]holderWire, 0, len(holders)),
		}
		for _, h := range holders {
			reply.Holders = append(reply.Holders, holderWire{
				Seat: h.Seat, Stage: string(h.Stage), Removed: h.Removed,
			})
		}
		return json.Marshal(reply)
	})
}

// errNoDirectoryAnswer is the unknown arm of a satellite's reading.
var errNoDirectoryAnswer = errors.New("engine: no node running the identity " +
	"domain answered, and its log has records, so this node cannot say who " +
	"holds which seat")

// fleetDirectory is [notify.Directory] for a node that runs no identity domain.
type fleetDirectory struct {
	// ask is the broker's scatter.
	ask queue.EventQueue

	// answerers counts the live nodes that run the identity domain, off the
	// presence leases — how many replies are worth waiting for.
	answerers func(ctx context.Context) (int, error)

	// quiet reports whether the identity log has never carried a record.
	quiet func(ctx context.Context) (bool, error)
}

var _ notify.Directory = (*fleetDirectory)(nil)

// SeatHolders asks the fleet, and answers with the most-caught-up reply.
func (d *fleetDirectory) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	n, err := d.answerers(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: count the nodes that hold the directory: %w", err)
	}
	if n > 0 {
		best, answered, err := d.scatter(ctx, n)
		if err != nil {
			return nil, err
		}
		if answered {
			return best, nil
		}
	}
	quiet, err := d.quiet(ctx)
	if err != nil {
		return nil, fmt.Errorf("engine: read the identity log's state: %w", err)
	}
	if quiet {
		// A FACT, not a fallback — see the notes above.
		return nil, nil
	}
	return nil, errNoDirectoryAnswer
}

// scatter asks up to want answerers and keeps the reply read at the highest
// position.
func (d *fleetDirectory) scatter(ctx context.Context, want int) (
	[]notify.Holder, bool, error) {

	if d.ask == nil {
		return nil, false, errors.New("engine: no broker to ask the fleet's " +
			"directory through")
	}
	request, err := json.Marshal(holdersRequest{Version: holdersProtocol})
	if err != nil {
		return nil, false, fmt.Errorf("engine: encode a directory request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, fleetDirectoryBudget)
	defer cancel()
	replies, err := d.ask.Ask(ctx, topics.IamHolders, request, want)
	if err != nil {
		return nil, false, fmt.Errorf("engine: ask the fleet's directory: %w", err)
	}
	var best *holdersReply
	for _, raw := range replies {
		var reply holdersReply
		if err := json.Unmarshal(raw, &reply); err != nil ||
			reply.Version != holdersProtocol {
			// A REPLY THIS BUILD CANNOT READ IS A MISSING ONE.
			continue
		}
		if best == nil || reply.At > best.At ||
			(reply.At == best.At && reply.Node < best.Node) {
			// THE MOST-CAUGHT-UP NODE, and the lowest node id between
			// two at one position, so two readings of the same fleet
			// pick the same answer rather than whichever arrived first.
			best = &reply
		}
	}
	if best == nil {
		return nil, false, nil
	}
	out := make([]notify.Holder, 0, len(best.Holders))
	for _, h := range best.Holders {
		out = append(out, notify.Holder{
			Seat: h.Seat, Stage: iam.Stage(h.Stage), Removed: h.Removed,
		})
	}
	return out, true, nil
}

// directoryAnswerers counts the live nodes whose roles run the identity
// domain.
//
// OFF THE PRESENCE LEASES, through the same predicate a node applies to its own
// roles — the retention report's own derivation, so the two can never
// disagree about who runs the domain.
func directoryAnswerers(ctx context.Context, leases coord.Backend) (int, error) {
	if leases == nil {
		return 0, nil
	}
	live, err := leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return 0, err
	}
	name := iamdomain.Domain{}.Name()
	n := 0
	for _, lease := range live {
		profile, ok := placement.FromLease(lease)
		if ok && participationOf(profile.Roles).Runs(name) {
			n++
		}
	}
	return n, nil
}
