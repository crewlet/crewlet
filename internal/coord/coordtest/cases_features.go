package coordtest

import (
	"errors"
	"reflect"

	"github.com/crewlet/crewlet/internal/coord"
)

// featureCases certify [coord.NodeStatus] across a real store round trip and
// the [coord.FeatureReader] built on it.
//
// In the suite rather than beside the reader because the reader's whole input
// is what a BACKEND hands back from a presence lease's Meta — and the two
// shipped backends hand it back in different Go types (the twin what was
// written, the KV a JSON round trip's []any and float64). A reader tested only
// against the twin would pass and then read every peer on the real store as
// honouring nothing.
var featureCases = []testCase{
	{"a_node_status_round_trips_through_presence", func(h *harness) {
		want := coord.NodeStatus{
			InFlight: 2, Posture: "serve",
			Features: []coord.Feature{coord.FeatureMCPStatus, "from_a_newer_build"},
			MCP: []coord.MCPServerStatus{
				{Server: "github", Shared: true, Started: 1, Tools: 12},
				{Server: "jira", Started: 1, Failed: 1, Tools: 9, Error: "401", ErrorSeat: "pm"},
			},
		}
		h.present("n1", "n1:a", want.Meta())
		for what, lease := range map[string]coord.Lease{
			"Get":      *h.mustHold(coord.NodeResource("n1"), "n1:a"),
			"ListLive": h.listLive(coord.ClassNode)[0],
		} {
			got, ok := coord.StatusFromMeta(lease.Meta)
			if !ok {
				h.t.Fatalf("%s: a published status read back as absent", what)
			}
			if !reflect.DeepEqual(got, want) {
				h.t.Fatalf("%s: status read back as %+v, want %+v", what, got, want)
			}
		}
	}},

	{"an_older_peers_status_reads_as_honouring_nothing", func(h *harness) {
		h.present("n1", "n1:a", map[string]any{"in_flight": 1, "draining": false})
		got, ok := coord.StatusFromMeta(h.listLive(coord.ClassNode)[0].Meta)
		if !ok {
			h.t.Fatal("an older peer's status read as absent")
		}
		if len(got.Features) != 0 {
			h.t.Fatalf("an older peer read as honouring %v", got.Features)
		}
	}},

	{"a_seat_is_answered_for_by_the_node_that_holds_it", func(h *harness) {
		h.present("n1", "n1:a", withFeatures(coord.FeatureMCPStatus))
		h.present("n2", "n2:a", withFeatures())
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL})
		h.claim(coord.SeatResource("pm"), coord.AcquireOptions{Owner: "n2:a", TTL: LongTTL})
		h.requireFeature("the upgraded holder", true, nil)(h.features().SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
		h.requireFeature("the older holder", false, nil)(h.features().SeatFeature(h.ctx, "pm", coord.FeatureMCPStatus))
	}},

	{"an_unheld_seat_is_answered_for_the_whole_fleet", func(h *harness) {
		// Whichever live node claims it next carries the gesture out.
		h.present("n1", "n1:a", withFeatures(coord.FeatureMCPStatus))
		h.requireFeature("an upgraded fleet", true, nil)(h.features().SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
		h.present("n2", "n2:a", withFeatures())
		h.requireFeature("a fleet with an older node", false, nil)(h.features().SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
	}},

	{"a_holder_with_no_presence_is_unknown_not_lacking", func(h *harness) {
		// Mid-drain: the presence lease is gone and the seat lease is not
		// yet. Nothing says what that process can do, and "try again" is
		// the honest answer — "your fleet is upgrading" is not.
		h.present("n1", "n1:a", withFeatures(coord.FeatureMCPStatus))
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "n2:gone", TTL: LongTTL})
		h.requireFeature("a holder with no presence", false, coord.ErrFeatureUnknown)(h.features().SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
	}},

	{"a_restarted_holder_is_not_answered_for_by_its_successor", func(h *harness) {
		// The seat lease still names the OLD incarnation; the node id's
		// presence now belongs to a new process whose build says nothing
		// about the one that holds the seat.
		h.present("n1", "n1:new", withFeatures(coord.FeatureMCPStatus))
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "n1:old", TTL: LongTTL})
		h.requireFeature("a holder whose node restarted", false, coord.ErrFeatureUnknown)(h.features().SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
	}},

	{"a_node_that_published_no_status_is_unknown", func(h *harness) {
		h.present("n1", "n1:a", withFeatures(coord.FeatureMCPStatus))
		h.present("n2", "n2:a", nil)
		h.requireFeature("a fleet with a silent node", false, coord.ErrFeatureUnknown)(h.features().AllLiveHave(h.ctx, coord.FeatureMCPStatus))
	}},

	{"one_node_lacking_decides_over_one_that_is_silent", func(h *harness) {
		h.present("n1", "n1:a", nil)
		h.present("n2", "n2:a", withFeatures())
		h.requireFeature("a silent node beside an older one", false, nil)(h.features().AllLiveHave(h.ctx, coord.FeatureMCPStatus))
	}},

	{"an_unreachable_store_is_an_error_not_a_no", func(h *harness) {
		// The store's own error, not ErrFeatureUnknown and not a false: a
		// blip must reach the caller as the outage it is.
		h.present("n1", "n1:a", withFeatures(coord.FeatureMCPStatus))
		h.claim(coord.SeatResource("ceo"), coord.AcquireOptions{Owner: "n1:a", TTL: LongTTL})
		store := NewFaulty(h.b)
		store.Break(nil)
		reader := coord.FeatureReader{Leases: store}
		h.requireFeature("a seat read on a broken store", false, coord.ErrUnavailable)(
			reader.SeatFeature(h.ctx, "ceo", coord.FeatureMCPStatus))
		h.requireFeature("a fleet read on a broken store", false, coord.ErrUnavailable)(
			reader.AllLiveHave(h.ctx, coord.FeatureMCPStatus))
	}},

	{"an_empty_fleet_is_unknown_not_vacuously_upgraded", func(h *harness) {
		h.requireFeature("no live node", false, coord.ErrFeatureUnknown)(h.features().AllLiveHave(h.ctx, coord.FeatureMCPStatus))
	}},
}

// present claims a node's presence lease carrying status as its heartbeat
// would, or no status at all when status is nil.
func (h *harness) present(nodeID, owner string, status map[string]any) {
	h.t.Helper()
	meta := map[string]any{"roles": []string{"seats"}}
	if status != nil {
		meta[coord.StatusKey] = status
	}
	h.claim(coord.NodeResource(nodeID), coord.AcquireOptions{
		Owner: owner, TTL: LongTTL, Preferred: nodeID, Ungated: true, Meta: meta,
	})
}

// withFeatures is the status an upgraded or an older node's heartbeat carries.
func withFeatures(features ...coord.Feature) map[string]any {
	return coord.NodeStatus{Features: features}.Meta()
}

func (h *harness) features() coord.FeatureReader { return coord.FeatureReader{Leases: h.b} }

// requireFeature checks one three-valued answer: the value, and whether the
// error is the one wanted (nil for none).
//
// Curried so a call site reads as the question followed by the read that
// answers it: Go passes a two-valued call only as a function's sole argument.
func (h *harness) requireFeature(what string, want bool, wantErr error) func(bool, error) {
	return func(got bool, err error) {
		h.t.Helper()
		switch {
		case wantErr == nil && err != nil:
			h.t.Fatalf("%s: unexpected error %v", what, err)
		case wantErr != nil && !errors.Is(err, wantErr):
			h.t.Fatalf("%s: error %v, want one wrapping %v", what, err, wantErr)
		case got != want:
			h.t.Fatalf("%s: answered %v, want %v", what, got, want)
		}
	}
}
