package objstore

import (
	"fmt"
	"maps"
	"time"
)

// What a data node says about its part in the object store, on the lease that
// makes it a member.
//
// # Why a lease of its own
//
// Membership used to be read off the node's PRESENCE lease, and presence is
// the seat host's: a shutdown drain gives it back at its first step, while the
// node is still serving every chunk it holds, so a drain that outlasted the
// map's grace moved the node's whole share away and then back. And presence
// carried the weight the node was CONFIGURED with and nothing about whether
// its volume still worked, so a node whose objects directory had failed stayed
// placed on for ever. The `objects:{node}` lease (coord.ClassObjects) is
// claimed by the object store itself, renewed on its own loop, released only
// once its chunk server has stopped — and carries what this file encodes:
// the store's own account of its health, and what its passes last found.
//
// # Hand-encoded, like coord.NodeStatus
//
// A lease's Meta is a `map[string]any` a backend is free to round-trip through
// JSON — an int comes back a float64, a nested map a map[string]any — so the
// wire shape is [ObjectsMeta.Encode] and [ObjectsFromMeta], written by hand to
// accept either. Struct tags would be a second encoding nothing calls.
//
// # Absent is not zero
//
// Every sub-report is a pointer, and nil is "this member did not say": a node
// whose repair has not run yet has no pending count, which is a different fact
// from a node holding everything the map places on it, and a scrub that has
// not started has verified nothing rather than found nothing rotten. A reader
// renders the absence, and the maintainer reads an absent health as a store
// that has not reported itself failed — the reading that removes nobody.

// ObjectsMeta is one data node's objects lease, decoded.
type ObjectsMeta struct {
	// Weight is the share of the object store the node offers
	// (store.objects.weight, resolved), 1..placement.MaxWeight. A lease
	// with none readable offers nothing: see [ObjectsFromMeta].
	Weight int

	// Labels are the node's own node.labels, which the map's failure domain
	// is read from — carried here rather than read off presence so a
	// membership never depends on the seat host's lease being present.
	Labels map[string]string

	// Health is the store's own account of whether it can hold chunks.
	Health *ObjectsHealth

	// Repair is the node's LAST repair pass, completed or not; Scrub where
	// its scrub is in its cycle.
	Repair *ObjectsRepair
	Scrub  *ObjectsScrub

	// Strays is how many referenced copies the node holds beyond what the
	// map places on it, awaiting the confirmation that lets its collector
	// delete them. A node being emptied is done when this reaches zero.
	//
	// NIL UNTIL A COLLECTION THAT WALKED EVERY SLOT HAS RUN AT THE MAP
	// EPOCH THE NODE PLACES BY, never a count from any other pass: one that
	// stopped short never looked at most of the disk, and one from before
	// the map moved counted against a placement that no longer stands — a
	// member taken out at epoch N held no strays at all under N-1. That
	// collection is the one every node runs once the fleet has settled at
	// the epoch, so it starts within about half a minute of the last member
	// finishing its repair rather than on the hourly schedule.
	Strays *int
}

// ObjectsHealth is a store's health on the lease: the vocabulary is
// disk.HealthState's (`ok`, `nearfull`, `full`, `failed`), kept a string here
// because a peer's build may know a state this one does not, and an unknown
// state is a store that has not said it FAILED.
type ObjectsHealth struct {
	State       string
	Detail      string
	UsedPercent float64
}

// ObjectsRepair is a repair pass as a peer reads it: the map epoch it placed
// by, whether it reached every group the node holds, and what it counted.
type ObjectsRepair struct {
	Epoch       uint64
	Completed   bool
	Placed      int
	Held        int
	Pending     int
	Unreachable int
	Missing     int
	At          time.Time
}

// ObjectsScrub is where a node's scrub is: when its cycle began, how far
// through the slots it is (0..1), what it has read so far — intact, rotten,
// and unreadable (bytes the disk would not return) — and what stopped it last,
// empty while it runs.
//
// THE ERROR RIDES THE LEASE because a scrub that stopped is otherwise visible
// only as a Progress that no longer moves — which a reader can tell from a
// slow one, pacing a large disk over its week, only by watching it for hours,
// where the node itself knows at once.
type ObjectsScrub struct {
	CycleStarted time.Time
	Progress     float64
	Verified     int
	Rotten       int
	Unreadable   int
	Error        string
}

// The lease's keys, one per concern.
const (
	metaWeight = "weight"
	metaLabels = "labels"
	metaHealth = "health"
	metaRepair = "repair"
	metaScrub  = "scrub"
	metaStrays = "strays"
)

// Encode renders the lease's Meta. A nil sub-report is omitted, which a reader
// takes as not reported.
func (m ObjectsMeta) Encode() map[string]any {
	out := map[string]any{metaWeight: m.Weight}
	if len(m.Labels) > 0 {
		out[metaLabels] = maps.Clone(m.Labels)
	}
	if h := m.Health; h != nil {
		health := map[string]any{"state": h.State, "used_percent": h.UsedPercent}
		if h.Detail != "" {
			health["detail"] = h.Detail
		}
		out[metaHealth] = health
	}
	if r := m.Repair; r != nil {
		out[metaRepair] = map[string]any{
			"epoch":       r.Epoch,
			"completed":   r.Completed,
			"placed":      r.Placed,
			"held":        r.Held,
			"pending":     r.Pending,
			"unreachable": r.Unreachable,
			"missing":     r.Missing,
			"at":          wireTime(r.At),
		}
	}
	if s := m.Scrub; s != nil {
		scrub := map[string]any{
			"cycle_started": wireTime(s.CycleStarted),
			"progress":      s.Progress,
			"verified":      s.Verified,
			"rotten":        s.Rotten,
			"unreadable":    s.Unreadable,
		}
		if s.Error != "" {
			scrub["error"] = s.Error
		}
		out[metaScrub] = scrub
	}
	if m.Strays != nil {
		out[metaStrays] = *m.Strays
	}
	return out
}

// ObjectsFromMeta reads a peer's objects lease, reporting false when it offers
// no share at all — no weight, or one that is not a whole number from 1 up.
//
// NO SHARE RATHER THAN THE DEFAULT ONE, for the reason a node read as holding
// objects is dangerous where a node read as holding every seat role is not:
// writers send it chunks and count its answers toward a quorum. Every other
// field fails soft — an unreadable label is dropped, an unreadable sub-report
// is absent — because a peer's malformed report must not take a working member
// out of the map.
func ObjectsFromMeta(meta map[string]any) (ObjectsMeta, bool) {
	weight, ok := wholeFromMeta(meta[metaWeight])
	if !ok || weight < 1 {
		return ObjectsMeta{}, false
	}
	out := ObjectsMeta{Weight: weight, Labels: labelsFromMeta(meta[metaLabels])}
	if raw, ok := meta[metaHealth].(map[string]any); ok {
		out.Health = &ObjectsHealth{
			State:       stringFromMeta(raw["state"]),
			Detail:      stringFromMeta(raw["detail"]),
			UsedPercent: floatFromMeta(raw["used_percent"]),
		}
	}
	if raw, ok := meta[metaRepair].(map[string]any); ok {
		r := &ObjectsRepair{
			Epoch:       epochFromMeta(raw["epoch"]),
			Placed:      intFromMeta(raw["placed"]),
			Held:        intFromMeta(raw["held"]),
			Pending:     intFromMeta(raw["pending"]),
			Unreachable: intFromMeta(raw["unreachable"]),
			Missing:     intFromMeta(raw["missing"]),
			At:          timeFromMeta(raw["at"]),
		}
		r.Completed, _ = raw["completed"].(bool)
		out.Repair = r
	}
	if raw, ok := meta[metaScrub].(map[string]any); ok {
		out.Scrub = &ObjectsScrub{
			CycleStarted: timeFromMeta(raw["cycle_started"]),
			Progress:     floatFromMeta(raw["progress"]),
			Verified:     intFromMeta(raw["verified"]),
			Rotten:       intFromMeta(raw["rotten"]),
			Unreadable:   intFromMeta(raw["unreadable"]),
			Error:        stringFromMeta(raw["error"]),
		}
	}
	if n, ok := wholeFromMeta(meta[metaStrays]); ok && n >= 0 {
		out.Strays = &n
	}
	return out, true
}

// wireTime is an instant on the wire, empty for the zero one.
func wireTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func timeFromMeta(v any) time.Time {
	s, _ := v.(string)
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// wholeFromMeta accepts the int this build writes and the float64 a JSON round
// trip returns, and nothing else — a fraction is not a count.
func wholeFromMeta(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

func intFromMeta(v any) int {
	n, _ := wholeFromMeta(v)
	return n
}

func floatFromMeta(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	}
	return 0
}

func stringFromMeta(v any) string {
	s, _ := v.(string)
	return s
}

// epochFromMeta reads a map epoch back. A float64 after a JSON round trip holds
// it exactly: an epoch moves once per placement change, and 2^53 of them is not
// a number a fleet reaches.
func epochFromMeta(v any) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int:
		return uint64(max(n, 0))
	case float64:
		if n < 0 || n != float64(uint64(n)) {
			return 0
		}
		return uint64(n)
	}
	return 0
}

// labelsFromMeta coerces a non-string value rather than dropping the map, for
// placement's reason: a peer that wrote zone: 7 is describing a real node.
func labelsFromMeta(raw any) map[string]string {
	switch v := raw.(type) {
	case map[string]string:
		return maps.Clone(v)
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
				continue
			}
			out[k] = fmt.Sprint(val)
		}
		return out
	}
	return nil
}
