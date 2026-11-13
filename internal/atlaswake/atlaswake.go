// Package atlaswake wakes the agent seat that owns an atlas entry when the
// entry drifts, at most once a UTC day (ADR-0143): what counts as drift
// ([Drift]), who owns an entry ([Owned]), the record that bounds the wakes
// ([Record], [Decide]), and the inbound source `atlas_drift` with its parser
// and prompt.
//
// # What drifts
//
// Three changes, a closed set ([Change]): a value a person set that a newer
// report differs from, two sources that disagree with nobody to settle it,
// and the entry's health turning to alerting. Each open one is an [Item],
// named by a digest, so the record never holds a value.
//
// # Who is woken
//
// The seat the entry's row names as its owner, where the chart holds it as
// an agent: never a person, never a unit, never an owner only suggested. The
// drift is read off the atlas as that seat reads it, so a wake never points
// at what its seat is not shown.
//
// # Once a day, and once a change
//
// The duty (internal/engine) keeps one [Record] per seat and entry in the
// coordination store, written only at the revision it was read at: the UTC
// day of the last wake and the items the seat has been told. A wake is asked
// for only where an item is open that the seat was not told, and only where
// the record's day is not today. The record is claimed BEFORE the publish
// and confirmed after it, so a publish that fails is made again under the
// same wake id and never a second time that day.
//
// # A wake is a pointer
//
// The delivery carries the entry's id and kind and the change classes, all
// closed words or ids ([Request]): no name, no value and nothing a vendor,
// an agent or a person wrote. The seat reads the entry itself, with the
// atlas tools, at the trust its writers earn.
package atlaswake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Source is the inbound source a drift wake is delivered under.
const Source = "atlas_drift"

// EventType is the one event a drift delivery carries.
const EventType = "atlas.drifted"

// Change is one class of drift on an entry.
type Change string

// The classes, in the order a wake names them.
const (
	// ChangeAlerting is the entry's health turning to alerting.
	ChangeAlerting Change = "alerting"
	// ChangeContested is a value a person set or confirmed that a newer
	// report differs from ([atlas.StandingContested]).
	ChangeContested Change = "contested"
	// ChangeDisputed is two sources that disagree with no person to settle
	// it ([atlas.StandingDisputed]).
	ChangeDisputed Change = "disputed"
)

// Changes is every class, in that order.
var Changes = []Change{ChangeAlerting, ChangeContested, ChangeDisputed}

// Valid reports whether c is a member of the closed set.
func (c Change) Valid() bool { return slices.Contains(Changes, c) }

// Item is one open drift on an entry: its class, and a digest that names it
// among the entry's so a record can say the seat was told it.
type Item struct {
	Change Change
	Mark   string
}

// mark digests what an item stands on. Sixteen hex digits: a record compares
// marks of one entry only, where 64 bits never collide by chance.
func mark(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// Drift is every open drift on e, in a stable order. A retired entry has none.
func Drift(e atlas.Entry) []Item {
	if e.State == atlas.StateRetired {
		return nil
	}
	var out []Item
	if e.Health() == atlas.HealthAlerting {
		out = append(out, Item{Change: ChangeAlerting, Mark: mark(string(ChangeAlerting))})
	}
	add := func(standing atlas.Standing, parts ...string) {
		switch standing {
		case atlas.StandingContested:
			out = append(out, Item{Change: ChangeContested, Mark: mark(parts...)})
		case atlas.StandingDisputed:
			out = append(out, Item{Change: ChangeDisputed, Mark: mark(parts...)})
		case atlas.StandingSettled:
		}
	}
	for _, f := range e.Facts {
		add(f.Standing, "fact", string(f.Fact), f.Of.Key, f.Value.Text)
	}
	for _, ed := range e.Relations {
		add(ed.Standing, "relation", string(ed.Relation), ed.To.String(), string(ed.External), ed.Name)
	}
	slices.SortFunc(out, func(a, b Item) int {
		if c := slices.Index(Changes, a.Change) - slices.Index(Changes, b.Change); c != 0 {
			return c
		}
		return strings.Compare(a.Mark, b.Mark)
	})
	return slices.Compact(out)
}

// Owned is the entries of v each agent seat answers for, by handle: the
// owner an entry's row shows ([atlas.Summary.Owner]), where it is a seat and
// agent says the chart holds that handle as an agent. A unit, a person's
// seat, a handle the chart no longer holds and an owner only suggested own
// nothing here, and a retired entry is nobody's.
func Owned(v atlas.View, agent func(handle string) bool) map[string][]uuid.UUID {
	out := map[string][]uuid.UUID{}
	if agent == nil {
		return out
	}
	rows := v.Summaries(false)
	for _, e := range v.Entries {
		if e.State == atlas.StateRetired {
			continue
		}
		owner := rows.Of(e).Owner
		if owner == nil || owner.Kind != atlas.ExternalSeat || !agent(owner.Name) {
			continue
		}
		out[owner.Name] = append(out[owner.Name], e.ID)
	}
	return out
}

// MaxTold bounds the marks a record keeps. Sixty-four: an entry with more
// open disagreements than that is told the rest again another day, and the
// record stays a few kilobytes of a coordination key.
const MaxTold = 64

// DayLayout is how a record and a request spell a UTC day.
const DayLayout = "2006-01-02"

// Day is the UTC day of at, as a record spells it.
func Day(at time.Time) string { return at.UTC().Format(DayLayout) }

// Record is what the coordination store keeps for one seat and one entry.
type Record struct {
	// Day is the UTC day of the seat's last wake about the entry, and At the
	// instant it was claimed: the wake's own instant on every copy.
	Day string    `json:"day"`
	At  time.Time `json:"at"`
	// Published is false between the claim and the publish it is for.
	Published bool `json:"published"`
	// Kind and Changes are what the claimed wake says, so a publish made
	// again says the same.
	Kind    atlas.Kind `json:"kind"`
	Changes []Change   `json:"changes,omitempty"`
	// Pending are the marks the claimed wake tells, and Told the marks the
	// seat has been told and that are still open.
	Pending []string `json:"pending,omitempty"`
	Told    []string `json:"told,omitempty"`
}

// EncodeRecord is r as the coordination store keeps it.
func EncodeRecord(r Record) ([]byte, error) {
	r.Told = r.Told[:min(len(r.Told), MaxTold)]
	r.Pending = r.Pending[:min(len(r.Pending), MaxTold)]
	return json.Marshal(r)
}

// DecodeRecord reads a record [EncodeRecord] wrote.
func DecodeRecord(raw []byte) (Record, error) {
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return Record{}, fmt.Errorf("atlaswake: a wake record does not read: %w", err)
	}
	return r, nil
}

// Step is what the duty does next for one seat and one entry.
type Step string

// The steps.
const (
	// StepNone leaves the record as it is.
	StepNone Step = ""
	// StepWake claims today's wake (writing Next), publishes it and confirms.
	StepWake Step = "wake"
	// StepPublish publishes again a wake claimed today whose publish was
	// never confirmed, and confirms it.
	StepPublish Step = "publish"
	// StepKeep writes Next: marks that are no longer open are forgotten.
	StepKeep Step = "keep"
	// StepDrop deletes the record: nothing is open and its day has passed.
	StepDrop Step = "drop"
)

// Decision is one step and the record it writes.
type Decision struct {
	Step Step
	Next Record
}

// Decide is the step for the record rec (found says one exists) of an entry
// of kind on which open is open, at now. whole says the read open comes from
// was a whole one: a partial read forgets nothing, since what it does not
// show may still be open.
//
// THE BOUND IS HERE: a wake is claimed only where rec's day is not today.
func Decide(rec Record, found bool, kind atlas.Kind, open []Item, whole bool, now time.Time) Decision {
	today := Day(now)
	if found && rec.Day == today && !rec.Published {
		return Decision{Step: StepPublish, Next: rec}
	}
	if found && !rec.Published {
		// A CLAIM WHOSE DAY PASSED UNPUBLISHED told nobody anything.
		rec.Pending = nil
	}
	openMarks := make([]string, 0, len(open))
	for _, it := range open {
		openMarks = append(openMarks, it.Mark)
	}
	told := rec.Told
	if whole {
		told = slices.DeleteFunc(slices.Clone(rec.Told), func(m string) bool { return !slices.Contains(openMarks, m) })
	}
	var fresh []Item
	for _, it := range open {
		if !slices.Contains(told, it.Mark) {
			fresh = append(fresh, it)
		}
	}
	fresh = fresh[:min(len(fresh), MaxTold)]
	if len(fresh) > 0 && (!found || rec.Day != today) {
		next := Record{Day: today, At: now.UTC(), Kind: kind, Told: told}
		for _, it := range fresh {
			next.Pending = append(next.Pending, it.Mark)
			if !slices.Contains(next.Changes, it.Change) {
				next.Changes = append(next.Changes, it.Change)
			}
		}
		return Decision{Step: StepWake, Next: next}
	}
	switch {
	case !found:
		return Decision{}
	case whole && len(open) == 0 && rec.Day != today:
		return Decision{Step: StepDrop}
	case len(told) != len(rec.Told):
		rec.Told = told
		return Decision{Step: StepKeep, Next: rec}
	}
	return Decision{}
}

// Confirmed is rec once its wake is published: what it told is told.
func Confirmed(rec Record) Record {
	for _, m := range rec.Pending {
		if !slices.Contains(rec.Told, m) {
			rec.Told = append(rec.Told, m)
		}
	}
	// THE NEWEST MARKS STAY where the bound cuts: an old mark forgotten is
	// told again another day, a new one forgotten would be told tomorrow.
	if over := len(rec.Told) - MaxTold; over > 0 {
		rec.Told = rec.Told[over:]
	}
	rec.Pending, rec.Published = nil, true
	return rec
}

// Request is one wake the duty asks for: the delivery's body. Every field is
// an id or a closed word.
type Request struct {
	Handle  string     `json:"handle"`
	Entry   uuid.UUID  `json:"entry"`
	Kind    atlas.Kind `json:"kind"`
	Changes []Change   `json:"changes"`
	Day     string     `json:"day"`
	At      time.Time  `json:"at"`
}

// RequestFor is the wake rec claims for seat handle on entry.
func RequestFor(handle string, entry uuid.UUID, rec Record) Request {
	return Request{Handle: handle, Entry: entry, Kind: rec.Kind, Changes: rec.Changes, Day: rec.Day, At: rec.At}
}

// ErrNotARequest is a delivery under [Source] that carries no wake this
// build can read.
var ErrNotARequest = errors.New("atlaswake: the delivery is not an atlas drift wake")

// check refuses a request that says anything outside the closed words.
func check(r Request) error {
	switch {
	case strings.TrimSpace(r.Handle) == "":
		return fmt.Errorf("%w: it names no seat", ErrNotARequest)
	case r.Entry == uuid.Nil || !r.Kind.Valid():
		return fmt.Errorf("%w: the wake of %s names no entry of a kind the atlas holds", ErrNotARequest, r.Handle)
	case len(r.Changes) == 0 || len(r.Changes) > len(Changes) ||
		slices.ContainsFunc(r.Changes, func(c Change) bool { return !c.Valid() }):
		return fmt.Errorf("%w: the wake of %s names no change of the closed set", ErrNotARequest, r.Handle)
	case r.At.IsZero():
		return fmt.Errorf("%w: the wake of %s carries no instant", ErrNotARequest, r.Handle)
	}
	if _, err := time.Parse(DayLayout, r.Day); err != nil {
		return fmt.Errorf("%w: the wake of %s names no day", ErrNotARequest, r.Handle)
	}
	return nil
}

// Encode is the inbound delivery one request is published as.
func Encode(r Request) (types.RawWebhook, error) {
	if err := check(r); err != nil {
		return types.RawWebhook{}, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return types.RawWebhook{}, fmt.Errorf("atlaswake: encode the wake of %s: %w", r.Handle, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return types.RawWebhook{}, fmt.Errorf("atlaswake: encode the wake of %s: %w", r.Handle, err)
	}
	// FIRST-PARTY, as every engine producer's delivery is, so a node of a
	// build that has no parser for this source hands it to a peer that has
	// rather than acknowledge it away (types.RawWebhook.FirstParty).
	return types.RawWebhook{Body: body, BodyRaw: raw, FirstParty: true}, nil
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
	return r, nil
}

// WakeID is the deterministic id of the wake r produces: its seat, its entry
// and its UTC day, so a publish made again collapses onto the first and no
// second wake of the day can be told from it.
func WakeID(r Request) uuid.UUID {
	return uuid.NewSHA1(wakeNamespace, []byte(r.Handle+"\x00"+r.Entry.String()+"\x00"+r.Day))
}

// wakeNamespace scopes drift wake ids. Fixed for the life of the deployment.
var wakeNamespace = uuid.MustParse("5c1d7e3a-94b6-4f2c-8a0e-3b6f1d2c7e49")
