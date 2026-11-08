// Package accessreview turns a change to an agent seat's access into one
// review turn for that seat (ADR-0111): the inbound source `access_review`, its
// parser and prompt, the wake's derived id, and the open work the seat is
// asked to review.
//
// # Why a turn at all
//
// ADR-0104 tells a seat its access on every turn and the change once, at its
// next turn. A seat whose open work waits on a person (a question it asked, a
// call held for approval) has no next turn until somebody answers, so the
// person answers a question worded for access the seat no longer has, and the
// seat learns it at the worst moment: an approved call it cannot make. So the
// node holding the seat asks for one turn once a revision changed the seat's
// access and the seat has open work, and that turn reviews the work and
// nothing else.
//
// # The record a wake derives from
//
// ADR-0012 and ADR-0095. The node holding the seat compares the seat's
// snapshot ([capability.Take]) with the one it was last told (the access log)
// after a quiet window, and publishes one [Request] on the inbound topic, the
// alarms' route: the delivery IS the durable record, and the notify spine's
// fleet-wide consumer group derives the wake on whichever node takes it. What
// the request states is derived from durable state (the company document, the
// seat access ledger and the access log), so a publish this node did not live
// to make is made again by the seat's next holder, which checks on admission.
//
// # Once each
//
// A request names the told record its change is measured from and the digest
// it is measured to, and its wake's id is derived from those and the seat
// ([WakeID]). A republish, or the same request from the seat's next holder,
// routes to the same id, and the inbox's same-id dedupe and the fleet
// completion ledger collapse it. A revert followed by the same change again is
// measured from a newer told record, and is a review of its own.
//
// # The turn tells the change, as every turn does
//
// The review turn's "What changed since your last turn" and its list of open
// work are rendered at its dispatch ([Work.Section]) and its told record is
// written at its launch, so its next turn is not told the change again. A turn
// that tells a change for any other reason carries the same list and the same
// ask, so whichever turn tells the change first asks for the review, and a
// review wake that finds nothing left to tell is acked without a turn.
package accessreview

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/textcut"
)

// Source is the inbound source a review is delivered under: the envelope's
// source, this parser's and its prompt's.
const Source = "access_review"

// EventType is the one event a review delivery carries.
const EventType = "access.changed"

// The metadata keys a review wake carries. Constants, because the prompt reads
// back what the parser wrote.
const (
	// BaselineField is the turn whose told record the change is measured
	// from.
	BaselineField = "baseline"
	// DigestField is the seat's snapshot digest the review is for.
	DigestField = "digest"
	// ChangesField is how many change lines the request carries.
	ChangesField = "changes"
	// OpenField is how many open items the node counted.
	OpenField = "open"
)

// Request is one review the node holding a seat asks for: the delivery's body.
type Request struct {
	// Handle is the seat to review.
	Handle string `json:"handle"`

	// Baseline is the turn whose told record the change is measured from,
	// and Digest the snapshot digest it is measured to.
	Baseline string `json:"baseline"`
	Digest   string `json:"digest"`

	// Lines are the change lines as the node measured them, at most
	// [capability.MaxLines], each at most [capability.MaxLineBytes]: the
	// activity entry's account. The turn is told the change at its
	// dispatch, from the access log, which is what it acts on.
	Lines []string `json:"lines"`

	// Open is how many open items the node counted for the seat.
	Open int `json:"open"`

	// At is when the node measured the change.
	At time.Time `json:"at"`
}

// ErrNotARequest is a delivery under [Source] that carries no review this build
// can read: a parse failure, which the spine records and acks.
var ErrNotARequest = errors.New("accessreview: the delivery is not an access review")

// Encode is the inbound delivery one request is published as: its JSON as the
// body and its exact bytes beside it, read back by [Decode].
func Encode(r Request) (types.RawWebhook, error) {
	r = bounded(r)
	if err := check(r); err != nil {
		return types.RawWebhook{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return types.RawWebhook{}, fmt.Errorf("accessreview: encode the review of %s: %w", r.Handle, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return types.RawWebhook{}, fmt.Errorf("accessreview: encode the review of %s: %w", r.Handle, err)
	}
	return types.RawWebhook{Body: body, BodyRaw: raw}, nil
}

// Decode is the request a delivery carries, the exact bytes first.
func Decode(w types.RawWebhook) (Request, error) {
	raw := w.BodyRaw
	if len(bytes.TrimSpace(raw)) == 0 {
		var err error
		if raw, err = json.Marshal(w.Body); err != nil {
			return Request{}, fmt.Errorf("%w: %w", ErrNotARequest, err)
		}
	}
	var r Request
	if err := json.Unmarshal(raw, &r); err != nil {
		return Request{}, fmt.Errorf("%w: %w", ErrNotARequest, err)
	}
	if err := check(r); err != nil {
		return Request{}, err
	}
	return bounded(r), nil
}

// check refuses a request no wake can be derived from: one naming no seat, or
// no told record or digest, could not be told apart from another review of
// the same seat.
func check(r Request) error {
	switch {
	case strings.TrimSpace(r.Handle) == "":
		return fmt.Errorf("%w: it names no seat", ErrNotARequest)
	case r.Baseline == "":
		return fmt.Errorf("%w: the review of %s names no told record to measure from",
			ErrNotARequest, r.Handle)
	case r.Digest == "":
		return fmt.Errorf("%w: the review of %s names no snapshot to measure to",
			ErrNotARequest, r.Handle)
	}
	return nil
}

// bounded holds a request's lines to what a turn is told: at most
// [capability.MaxLines], each at most [capability.MaxLineBytes].
func bounded(r Request) Request {
	if len(r.Lines) > capability.MaxLines {
		r.Lines = r.Lines[:capability.MaxLines]
	}
	lines := make([]string, 0, len(r.Lines))
	for _, line := range r.Lines {
		lines = append(lines, textcut.Within(line, capability.MaxLineBytes))
	}
	r.Lines = lines
	return r
}

// WakeID is the deterministic id of the wake request r produces.
//
// DERIVED from the seat, the told record the change is measured from and the
// digest it is measured to, which is what makes a republish, or the same
// request from the seat's next holder, recognisable: the inbox's same-id
// dedupe and the fleet completion ledger key on it. The told record is part
// of it so that a change made, reverted and made again is reviewed again: the
// second time it is measured from a newer record.
func WakeID(r Request) uuid.UUID {
	return uuid.NewSHA1(wakeNamespace, []byte(r.Handle+"\x00"+r.Baseline+"\x00"+r.Digest))
}

// wakeNamespace scopes review wake ids. Fixed for the life of the deployment:
// a new one would make every republish a fresh wake.
var wakeNamespace = uuid.MustParse("2f0e8a6c-5b2d-4f7e-9c41-7d3a6e18b9f5")
