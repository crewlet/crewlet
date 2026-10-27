// Package alarmwake turns one node's alarm transition into a wake for every
// seat whose role carries the health grant: the notify spine's parser and
// prompt for the inbound source `alarms`.
//
// # The record a wake derives from
//
// ADR-0065. A node's alarm tracker (internal/statelog's Tracker) hands every
// transition it makes — an alarm instance fired, cleared, or found firing by
// the tracker's first evaluation — to the engine's outbox, which publishes it
// on the inbound topic as a delivery of this source, retried until the broker
// takes it ([Encode] is that delivery's one shape, [Decode] its one reading).
// The published delivery IS the durable record ADR-0012 asks a wake to be
// derived from: nothing commits beside it, so no publish trails a commit, and
// the notify spine's fleet-wide consumer group derives the wakes on whichever
// node takes the delivery.
//
// # Who is woken
//
// Every AGENT seat whose role carries `health_tools` (authz.SeatGrantHealth,
// read off the epoch's chart by orgchart.GrantedSeats), which is exactly who
// the health tools admit: a seat is told the fleet's health changed where it
// may read the fleet's health, and nowhere else. Through internal/routing, as
// every first-party source routes: one reason ([ReasonHealthGrant]), no actor
// — an alarm has no author — and a seat the company no longer employs dropped
// by the roster the delivery is routed against.
//
// # Once each
//
// A transition carries its own id, derived by the tracker that made it from
// the node, the alarm, the log it stands on, the change and the instant the
// instance was raised (statelog.TransitionID), and each wake's id is derived
// from that id and the seat ([WakeID]). So a republish — the outbox retrying a
// publish the broker took but whose answer was lost, or a delivery handed to
// a second node — routes to the same ids, and the inbox's same-id dedupe and
// the fleet completion ledger collapse it: each granted seat is woken once per
// transition. A random id would make every copy a fresh wake.
//
// # A wake is a pointer
//
// The delivery says what changed and when; what is firing NOW is
// `fleet_health`'s to say, from every node's own heartbeat. So the prompt sends
// its seat to look before it acts: a later transition may already have
// cleared the alarm, and a clear lost with its node (the outbox is the node's
// memory) is read there too — the restarted node's first evaluation reports
// what is still firing, as `firing_at_boot`.
package alarmwake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/routing"
	"github.com/crewlet/crewlet/internal/statelog"
)

var log = logging.Get("alarmwake")

// Source is the inbound source an alarm transition is delivered under: the
// envelope's source, this parser's and its prompt's.
const Source = "alarms"

// The metadata keys an alarm wake carries. Constants, because the prompt reads
// back what the parser wrote, and a key spelled two ways is a field that is
// always empty on one side.
const (
	// NodeField is the node whose tracker made the transition.
	NodeField = "node"
	// AlarmField is the alarm's kind.
	AlarmField = "alarm"
	// DomainField is the log a per-log alarm stands on; absent for a
	// node-wide one.
	DomainField = "domain"
	// ChangeField is what the alarm did (statelog.TransitionChange).
	ChangeField = "change"
	// SinceField is when the instance was raised, RFC 3339.
	SinceField = "since"
	// AtField is when the transition was observed, RFC 3339.
	AtField = "at"
	// TransitionField is the transition's own id.
	TransitionField = "transition"
)

// Reason is why a seat is told of a transition.
//
// A NAMED TYPE for routing.Builder's sake, which takes a domain's own closed
// set: today it has one member, because one fact reaches a seat — its role
// carries the health grant.
type Reason string

// ReasonHealthGrant is a seat told because its role carries `health_tools`.
const ReasonHealthGrant Reason = "health_grant"

// Valid reports whether r is a reason this package routes by.
func (r Reason) Valid() bool { return r == ReasonHealthGrant }

// ErrNotATransition is a delivery under [Source] that carries no transition
// this build can read: a parse failure, which the spine records and acks.
var ErrNotATransition = errors.New("alarmwake: the delivery is not an alarm transition")

// Encode is the inbound delivery one transition is published as: the
// transition's JSON as the body, its exact bytes beside it — the shape every
// delivery on the inbound topic has, read back by [Decode].
func Encode(t statelog.AlarmTransition) (types.RawWebhook, error) {
	if err := check(t); err != nil {
		return types.RawWebhook{}, err
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return types.RawWebhook{}, fmt.Errorf("alarmwake: encode transition %s: %w", t.ID, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return types.RawWebhook{}, fmt.Errorf("alarmwake: encode transition %s: %w", t.ID, err)
	}
	return types.RawWebhook{Body: body, BodyRaw: raw}, nil
}

// Decode is the transition a delivery carries.
//
// THE EXACT BYTES FIRST, as every reader of the inbound edge prefers them, and
// the parsed body where a delivery carries none. A transition that names no
// id, no node, no alarm or no change is [ErrNotATransition]: one that cannot
// be told apart from its own republish, or says nothing a seat could look up,
// wakes nobody rather than somebody about nothing. A change this build does
// not know is read all the same — a newer node's word for what its alarm did,
// which the prompt repeats rather than drops (the older peer's half of an
// additive change).
func Decode(w types.RawWebhook) (statelog.AlarmTransition, error) {
	raw := w.BodyRaw
	if len(bytes.TrimSpace(raw)) == 0 {
		var err error
		if raw, err = json.Marshal(w.Body); err != nil {
			return statelog.AlarmTransition{}, fmt.Errorf("%w: %w", ErrNotATransition, err)
		}
	}
	var t statelog.AlarmTransition
	if err := json.Unmarshal(raw, &t); err != nil {
		return statelog.AlarmTransition{}, fmt.Errorf("%w: %w", ErrNotATransition, err)
	}
	if err := check(t); err != nil {
		return statelog.AlarmTransition{}, err
	}
	return t, nil
}

// check refuses a transition no wake can be derived from.
func check(t statelog.AlarmTransition) error {
	switch {
	case t.ID == "":
		return fmt.Errorf("%w: it carries no id, so a republish could not be told apart from it",
			ErrNotATransition)
	case t.Node == "":
		return fmt.Errorf("%w: transition %s names no node", ErrNotATransition, t.ID)
	case t.Kind == "":
		return fmt.Errorf("%w: transition %s names no alarm", ErrNotATransition, t.ID)
	case t.Change == "":
		return fmt.Errorf("%w: transition %s names no change", ErrNotATransition, t.ID)
	}
	return nil
}

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// Granted is the agent seats whose role carries the health grant, in
	// the chart's order: who every transition is routed to.
	Granted []string
}

// Parser turns one alarm transition into a wake for each granted seat.
//
// PER EPOCH: the granted seats are the chart's, which is what an apply
// changes, so the engine builds one for every epoch and swaps it in, as it
// does every first-party parser.
type Parser struct {
	granted []string
}

var _ notify.Parser = (*Parser)(nil)

// NewParser builds the parser.
func NewParser(opts ParserOptions) *Parser {
	return &Parser{granted: append([]string(nil), opts.Granted...)}
}

// Source implements [notify.Parser].
func (*Parser) Source() string { return Source }

// Parse implements [notify.Parser]: one wake for every granted seat the
// company still employs, each with the id [WakeID] derives and the instant the
// transition was observed, so every copy of one delivery routes identically.
func (p *Parser) Parse(_ context.Context, w types.RawWebhook, reg *notify.Registry) ([]notify.Routed, error) {
	t, err := Decode(w)
	if err != nil {
		return nil, err
	}
	var b routing.Builder[Reason]
	b.AddAll(p.granted, ReasonHealthGrant, false)
	routed := routing.Route(b.Candidates(), employs(reg), "", nil)
	if len(routed) == 0 {
		// THE ORDINARY OUTCOME for a company that granted nobody: the
		// alarm is said by the node's log line, gauge, health count and
		// heartbeat all the same, and no seat asked to be told.
		log.Debug("alarm_transition_reaches_nobody", "transition", t.ID, "alarm", string(t.Kind),
			"node", t.Node, "change", string(t.Change))
		return nil, nil
	}
	out := make([]notify.Routed, 0, len(routed))
	for _, c := range routed {
		out = append(out, notify.Routed{
			Inbound: inbound(t),
			To:      notify.Recipient{Handle: c.Handle},
			WakeID:  WakeID(t, c.Handle),
			WakeAt:  t.At,
		})
	}
	return out, nil
}

// employs is whether the registry still holds a handle; nil admits everybody,
// routing.Route's own reading of a deployment with no roster loaded yet.
func employs(reg *notify.Registry) func(string) bool {
	if reg == nil {
		return nil
	}
	return func(handle string) bool {
		_, ok := reg.ByHandle(handle)
		return ok
	}
}

// inbound is one recipient's copy, built per recipient so no two copies share
// a metadata map the spine stamps.
func inbound(t statelog.AlarmTransition) notify.Inbound {
	meta := map[string]string{
		NodeField:       t.Node,
		AlarmField:      string(t.Kind),
		ChangeField:     string(t.Change),
		TransitionField: t.ID,
	}
	if t.Domain != "" {
		meta[DomainField] = t.Domain
	}
	if !t.Since.IsZero() {
		meta[SinceField] = t.Since.UTC().Format(time.RFC3339)
	}
	if !t.At.IsZero() {
		meta[AtField] = t.At.UTC().Format(time.RFC3339)
	}
	return notify.Inbound{
		Source:    Source,
		EventType: "alarm." + string(t.Change),
		Subject:   subject(t),
		Body:      t.Detail,
		Metadata:  meta,
	}
}

// subject is the one line an operator sees in the feed: the alarm, where it
// stands and what it did.
func subject(t statelog.AlarmTransition) string {
	where := t.Node
	if t.Domain != "" {
		where = t.Node + ", " + t.Domain + " log"
	}
	switch t.Change {
	case statelog.TransitionFired:
		return fmt.Sprintf("%s fired (%s)", t.Kind, where)
	case statelog.TransitionCleared:
		return fmt.Sprintf("%s cleared (%s)", t.Kind, where)
	case statelog.TransitionFiringAtBoot:
		return fmt.Sprintf("%s firing at start (%s)", t.Kind, where)
	default:
		return fmt.Sprintf("%s %s (%s)", t.Kind, t.Change, where)
	}
}

// WakeID is the deterministic id of the wake transition t produces for the
// seat handle.
//
// DERIVED, which is what makes a republished transition recognisable at all:
// the inbox's same-id dedupe and the fleet completion ledger both key on it,
// and with random ids every copy wakes the seat again with neither layer able
// to see the pair. PER SEAT, because one transition wakes every granted seat,
// and one id for all of them would deliver the first and deduplicate the rest.
func WakeID(t statelog.AlarmTransition, handle string) uuid.UUID {
	return uuid.NewSHA1(wakeNamespace, []byte(t.ID+"\x00"+handle))
}

// wakeNamespace scopes alarm wake ids. Fixed for the life of the deployment:
// a new one would make every republish a fresh wake.
var wakeNamespace = uuid.MustParse("68313812-6259-4959-954b-80e340e125e9")
