package placement

import (
	"cmp"
	"slices"

	"github.com/crewlet/crewlet/internal/secrets"
)

// AuthDigest is which keys a node's API admits and what each reaches, as its
// peers can compare it without learning any of them.
//
// # Why it rides the presence profile
//
// Tier A is per node, and the API's key table is Tier A's: every node serving
// the API judges a request by its own `api.auth`. With roles that stopped being
// bookkeeping — a key that is a member's on one node and an admin's on the next
// is a way to read every agent's transcript through whichever node a load
// balancer happens to pick. Nothing else in the fleet can see two nodes'
// tables side by side, so each node publishes a keyed fingerprint of its own
// ([secrets.Fingerprint]) where its peers already read what it is: the lease
// that says it is LIVE. That is also what makes a node that has gone stop
// counting — its presence lapses at the lease TTL, the same moment placement
// stops dividing seats by it — with no clock of the comparison's own.
//
// A FACT ABOUT THE PROCESS, like [BrokerKind]: Tier A changes at a restart, and
// the profile is re-sent on every renew, so a node restarted with another table
// corrects what its peers believe within a heartbeat.
//
// # Serving is part of the fact
//
// Only a node that serves the API — the ingress role, on a bound api.port —
// admits anybody by its table, and only such a node publishes one. A worker
// whose Tier A carries no keys, or stale ones, is not a divergence: nothing
// reaches the company through it, and keeping admin keys off nodes that need
// none is a posture an operator should be free to choose.
type AuthDigest struct {
	// Serves says the node serves the keyed API. A profile that does not is
	// compared with nothing.
	Serves bool

	// Keys is the table's fingerprint under every key of the node's keyring.
	// Empty on a node that serves the API with no keyring: it holds nothing
	// a fingerprint could be taken under, so it compares with nobody — which
	// is the documented cost of running without one, never an agreement.
	Keys secrets.Fingerprint
}

// AuthDigestKey is where a presence row carries [NodeProfile.Auth]: present
// exactly when the node serves the API, as an object keyed by keyring key id,
// each entry a `check` and a `digest` — empty where the node has no keyring.
//
// A build that changes what the digest COVERS publishes it under another key:
// a changed input compared against this one reads as two different tables,
// and a rolling upgrade would raise the alarm on every node it passed.
const AuthDigestKey = "auth_digest"

// Against reports how this node's table stands against another's — see
// [secrets.Fingerprint.Against]. A node that serves no API has no table to
// compare, so either side not serving is incomparable.
func (a AuthDigest) Against(other AuthDigest) secrets.Agreement {
	if !a.Serves || !other.Serves {
		return secrets.AgreementIncomparable
	}
	return a.Keys.Against(other.Keys)
}

// meta is the digest in the shape a presence row carries it.
func (a AuthDigest) meta() map[string]any {
	keys := make(map[string]any, len(a.Keys))
	for id, d := range a.Keys {
		keys[id] = map[string]any{"check": d.Check, "digest": d.Digest}
	}
	return keys
}

// authFromMeta reads a peer's digest back off its presence row.
//
// THE SAFE READING OF A ROW THIS BUILD CANNOT READ is a node that serves the
// API and compares with nobody: present but malformed says the node serves,
// and claiming either agreement or a divergence from bytes nothing could
// decode would be a guess. An entry missing either half is dropped rather than
// compared, for the same reason.
func authFromMeta(raw any) AuthDigest {
	if raw == nil {
		return AuthDigest{}
	}
	out := AuthDigest{Serves: true}
	entries, ok := raw.(map[string]any)
	if !ok {
		return out
	}
	for id, v := range entries {
		entry, ok := v.(map[string]any)
		if !ok {
			continue
		}
		check, _ := entry["check"].(string)
		digest, _ := entry["digest"].(string)
		if id == "" || check == "" || digest == "" {
			continue
		}
		if out.Keys == nil {
			out.Keys = secrets.Fingerprint{}
		}
		out.Keys[id] = secrets.KeyedDigest{Check: check, Digest: digest}
	}
	return out
}

// DivergentAuth is the nodes that serve the API grouped by the key table they
// hold, when two of the groups definitively disagree — and nil when no two do.
//
// THE `auth_diverged` ALARM'S INPUT, and the population is the caller's: the
// live nodes, which is what makes a node that has gone stop counting once its
// presence lapses. Each group is the nodes a shared key says hold one table,
// in id order, and the groups are ordered by their first node.
//
// ONLY A DEFINITE DIVERGENCE IS ONE. A node with no keyring, or two whose
// keyrings share no key, cannot be compared — that is not an agreement, and the
// fleet view says so row by row, but neither is it a table shown to differ: a
// fleet that runs without a keyring has chosen the one posture under which no
// comparison is possible, and an alarm that could never clear on it would be
// one an operator learns to ignore. A group that differs from no other group is
// left out, so the groups named are exactly the ones in disagreement.
func DivergentAuth(nodes []NodeProfile) [][]string {
	serving := make([]NodeProfile, 0, len(nodes))
	for _, n := range nodes {
		if n.Auth.Serves && len(n.Auth.Keys) > 0 {
			serving = append(serving, n)
		}
	}
	slices.SortFunc(serving, func(a, b NodeProfile) int { return cmp.Compare(a.ID, b.ID) })

	// EVERY PAIR, ONCE. A fleet is tens of nodes, and comparing only against
	// a group's first member would miss what comparability does not carry:
	// two nodes can hold one table and share no key between them — one
	// mid-rotation onto a key the other has already dropped — and be joined
	// only through a third that shares a key with each.
	n := len(serving)
	group := make([]int, n)
	for i := range group {
		group[i] = i
	}
	root := func(i int) int {
		for group[i] != i {
			i = group[i]
		}
		return i
	}
	agreement := make([][]secrets.Agreement, n)
	for i := range serving {
		agreement[i] = make([]secrets.Agreement, n)
		for j := range i {
			a := serving[i].Auth.Against(serving[j].Auth)
			agreement[i][j] = a
			if a == secrets.AgreementSame {
				group[root(i)] = root(j)
			}
		}
	}
	differs := map[int]bool{}
	for i := range serving {
		for j := range i {
			if agreement[i][j] == secrets.AgreementDifferent && root(i) != root(j) {
				differs[root(i)], differs[root(j)] = true, true
			}
		}
	}

	members := map[int][]string{}
	var order []int
	for i, node := range serving {
		r := root(i)
		if !differs[r] {
			continue
		}
		if _, seen := members[r]; !seen {
			order = append(order, r)
		}
		members[r] = append(members[r], node.ID)
	}
	out := make([][]string, 0, len(order))
	for _, r := range order {
		out = append(out, members[r])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
