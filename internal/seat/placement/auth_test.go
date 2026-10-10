package placement

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/secrets"
)

// The key table a node's API admits, as its presence carries it and as the
// `auth_diverged` alarm groups the fleet by it.

// tableOf is a node serving the API whose table is the given value, under
// keyring keys named by id with fixed material — so two calls naming one key
// are two nodes holding it.
func tableOf(t *testing.T, id, value string, keys ...string) NodeProfile {
	t.Helper()
	ring := secrets.Keyring{Keys: map[string][]byte{}}
	for _, k := range keys {
		ring.Keys[k] = bytes.Repeat([]byte(k[:1]), 32)
	}
	f, err := ring.Fingerprint("test", []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return NodeProfile{ID: id, Auth: AuthDigest{Serves: true, Keys: f}}
}

// roundTrip is a profile as a peer reads it back off the store, which hands
// every map back as map[string]any after a JSON round trip.
func roundTrip(t *testing.T, p NodeProfile) NodeProfile {
	t.Helper()
	raw, err := json.Marshal(p.Meta())
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	return FromMeta(p.ID, meta)
}

// A PEER READS BACK EXACTLY THE TABLE A NODE PUBLISHED, through the store's
// JSON — a node serving the API with no keyring included, which says it serves
// and holds nothing to compare — and a node serving no API publishes nothing.
func TestAPresenceRowCarriesTheTableItsNodeAdmits(t *testing.T) {
	t.Parallel()
	served := tableOf(t, "a", "the table", "k1", "k2")
	got := roundTrip(t, served)
	if !got.Auth.Serves || got.Auth.Against(served.Auth) != secrets.AgreementSame {
		t.Errorf("read back %+v, want the table it published", got.Auth)
	}
	if len(got.Auth.Keys) != 2 {
		t.Errorf("read back %d keys, want 2", len(got.Auth.Keys))
	}

	unkeyed := roundTrip(t, NodeProfile{ID: "b", Auth: AuthDigest{Serves: true}})
	if !unkeyed.Auth.Serves || len(unkeyed.Auth.Keys) != 0 {
		t.Errorf("a node serving with no keyring read back as %+v", unkeyed.Auth)
	}

	silent := NodeProfile{ID: "c"}
	if _, written := silent.Meta()[AuthDigestKey]; written {
		t.Error("a node serving no API published a table")
	}
	if roundTrip(t, silent).Auth.Serves {
		t.Error("a node serving no API read back as serving one")
	}
}

// A ROW THIS BUILD CANNOT READ SERVES AND COMPARES WITH NOBODY: present but
// malformed is neither an agreement nor a divergence, and an entry missing half
// of itself is dropped rather than compared.
func TestAMalformedTableComparesWithNobody(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]any{
		"not an object":       "abc",
		"an entry not object": map[string]any{"k1": "abc"},
		"an entry with no check": map[string]any{
			"k1": map[string]any{"digest": "abc"},
		},
	} {
		got := FromMeta("x", map[string]any{AuthDigestKey: raw}).Auth
		if !got.Serves {
			t.Errorf("%s: read as serving no API", name)
		}
		if ag := got.Against(tableOf(t, "y", "v", "k1").Auth); ag != secrets.AgreementIncomparable {
			t.Errorf("%s: compared %s, want incomparable", name, ag)
		}
	}
}

// THE ALARM'S INPUT NAMES THE GROUPS IN DISAGREEMENT AND NOTHING ELSE.
func TestDivergentAuthGroupsTheNodesThatDisagree(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		nodes []NodeProfile
		want  [][]string
	}{
		"every node holding one table": {
			nodes: []NodeProfile{tableOf(t, "a", "v1", "k1"), tableOf(t, "b", "v1", "k1"),
				tableOf(t, "c", "v1", "k1")},
		},
		"one node holding another": {
			nodes: []NodeProfile{tableOf(t, "c", "v1", "k1"), tableOf(t, "b", "v2", "k1"),
				tableOf(t, "a", "v1", "k1")},
			want: [][]string{{"a", "c"}, {"b"}},
		},
		"three tables": {
			nodes: []NodeProfile{tableOf(t, "a", "v1", "k1"), tableOf(t, "b", "v2", "k1"),
				tableOf(t, "c", "v3", "k1")},
			want: [][]string{{"a"}, {"b"}, {"c"}},
		},
		// A node with no keyring, and one whose keyring shares nothing with
		// the rest, are not shown to differ — and a group that differs from
		// nobody is left out of the groups named.
		"a node nothing can compare": {
			nodes: []NodeProfile{tableOf(t, "a", "v1", "k1"), tableOf(t, "b", "v2", "k1"),
				{ID: "c", Auth: AuthDigest{Serves: true}}, tableOf(t, "d", "v3", "z9")},
			want: [][]string{{"a"}, {"b"}},
		},
		"only nodes nothing can compare": {
			nodes: []NodeProfile{{ID: "a", Auth: AuthDigest{Serves: true}},
				{ID: "b", Auth: AuthDigest{Serves: true}}},
		},
		// A node that serves no API admits nobody by a table, whatever its
		// Tier A carries.
		"a node serving no API": {
			nodes: []NodeProfile{tableOf(t, "a", "v1", "k1"),
				{ID: "b", Auth: AuthDigest{Keys: tableOf(t, "b", "v2", "k1").Auth.Keys}}},
		},
		// Mid keyring rotation: a holds only k1, c only k2, and they hold
		// one table — joined through b, which holds both. Compared only
		// against a group's first member, c would be its own group.
		"one table across a keyring rotation": {
			nodes: []NodeProfile{tableOf(t, "a", "v1", "k1"), tableOf(t, "b", "v1", "k1", "k2"),
				tableOf(t, "c", "v1", "k2"), tableOf(t, "d", "v2", "k2")},
			want: [][]string{{"a", "b", "c"}, {"d"}},
		},
	} {
		got := DivergentAuth(tc.nodes)
		if !slices.EqualFunc(got, tc.want, slices.Equal[[]string]) {
			t.Errorf("%s: groups = %v, want %v", name, got, tc.want)
		}
	}
}
