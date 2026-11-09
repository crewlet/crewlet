// Package accessreview turns a change to an agent seat's access into one
// review turn for that seat (ADR-0111): the inbound source `access_review`,
// its parser and prompt, the seat's pending review record, and the open work a
// turn that tells a change lists.
//
// The node that measures a change records it as the seat's [Pending] review
// in the coordination store and publishes one [Request] on the inbound topic,
// the alarms' route (ADR-0095). The turn it wakes is told the change at its
// dispatch, as every turn is (ADR-0104), and the record is dropped when a turn
// that was told the change ends.
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

// Source is the inbound source a review is delivered under.
const Source = "access_review"

// EventType is the one event a review delivery carries.
const EventType = "access.changed"

// The metadata keys a review wake carries.
const (
	BaselineField = "baseline"
	DigestField   = "digest"
	ChangesField  = "changes"
	OpenField     = "open"
)

// Pending is one seat's access change waiting on its review, as the
// coordination store holds it.
type Pending struct {
	// Baseline is the told record the change is measured from, BaselineAt
	// its instant, and Digest the access it is measured to.
	Baseline   string    `json:"baseline"`
	BaselineAt time.Time `json:"baseline_at"`
	Digest     string    `json:"digest"`

	// ChangedAt is when the change was first measured: a change made,
	// reverted and made again is measured again, at a new instant.
	ChangedAt time.Time `json:"changed_at"`

	// ToldTurn is the turn first told the change, and ToldAt when; empty
	// until one is.
	ToldTurn string    `json:"told_turn,omitempty"`
	ToldAt   time.Time `json:"told_at,omitzero"`

	// Lines are the change in a person's words, at most [capability.MaxLines].
	Lines []string `json:"lines"`
}

// Same reports whether p is the change measured from baseline to digest.
func (p Pending) Same(baseline, digest string) bool {
	return p.Baseline == baseline && p.Digest == digest
}

// Stale reports whether a question asked at asked was raised under the access
// before this change: before any turn was told it.
func (p Pending) Stale(asked time.Time) bool {
	return p.ToldAt.IsZero() || asked.Before(p.ToldAt)
}

// EncodePending is p as the coordination store keeps it.
func EncodePending(p Pending) ([]byte, error) {
	p.Lines = boundedLines(p.Lines)
	return json.Marshal(p)
}

// DecodePending reads a record [EncodePending] wrote.
func DecodePending(raw []byte) (Pending, error) {
	var p Pending
	if err := json.Unmarshal(raw, &p); err != nil {
		return Pending{}, fmt.Errorf("accessreview: a pending review record does not read: %w", err)
	}
	return p, nil
}

// Request is one review the node holding a seat asks for: the delivery's body.
type Request struct {
	Handle     string    `json:"handle"`
	Baseline   string    `json:"baseline"`
	BaselineAt time.Time `json:"baseline_at"`
	Digest     string    `json:"digest"`
	ChangedAt  time.Time `json:"changed_at"`

	// Lines are the change in the seat's words, for the activity entry; the
	// turn is told the change at its dispatch, from the access log.
	Lines []string `json:"lines"`

	// Open is how many distinct open items the node counted ([Work.Open]).
	Open int `json:"open"`
}

// RequestFor is the review of seat handle's pending change.
func RequestFor(handle string, p Pending, lines []string, open int) Request {
	return Request{Handle: handle, Baseline: p.Baseline, BaselineAt: p.BaselineAt,
		Digest: p.Digest, ChangedAt: p.ChangedAt, Lines: lines, Open: open}
}

// ErrNotARequest is a delivery under [Source] that carries no review this
// build can read.
var ErrNotARequest = errors.New("accessreview: the delivery is not an access review")

// Encode is the inbound delivery one request is published as.
func Encode(r Request) (types.RawWebhook, error) {
	r.Lines = boundedLines(r.Lines)
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
	r.Lines = boundedLines(r.Lines)
	return r, nil
}

// check refuses a request no wake id can be derived from.
func check(r Request) error {
	switch {
	case strings.TrimSpace(r.Handle) == "":
		return fmt.Errorf("%w: it names no seat", ErrNotARequest)
	case r.Baseline == "" || r.BaselineAt.IsZero():
		return fmt.Errorf("%w: the review of %s names no told record to measure from",
			ErrNotARequest, r.Handle)
	case r.Digest == "" || r.ChangedAt.IsZero():
		return fmt.Errorf("%w: the review of %s names no measured change", ErrNotARequest, r.Handle)
	}
	return nil
}

// boundedLines holds lines to what a turn is told: at most
// [capability.MaxLines], each at most [capability.MaxLineBytes].
func boundedLines(in []string) []string {
	in = in[:min(len(in), capability.MaxLines)]
	out := make([]string, 0, len(in))
	for _, line := range in {
		out = append(out, textcut.Within(line, capability.MaxLineBytes))
	}
	return out
}

// WakeID is the deterministic id of the wake r produces: its seat, the told
// record and digest it is measured between, and when that change was first
// measured, so a republish collapses and a redo after a revert is new.
func WakeID(r Request) uuid.UUID {
	return uuid.NewSHA1(wakeNamespace, []byte(r.Handle+"\x00"+r.Baseline+"\x00"+r.Digest+
		"\x00"+r.ChangedAt.UTC().Format(time.RFC3339Nano)))
}

// wakeNamespace scopes review wake ids. Fixed for the life of the deployment.
var wakeNamespace = uuid.MustParse("2f0e8a6c-5b2d-4f7e-9c41-7d3a6e18b9f5")
