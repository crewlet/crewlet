package jetstream

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// RouteKeyDomain, LeafKeyDomain and ClientKeyDomain are the derivation domains
// of the fleet's three listener credentials. Each key of secrets.keys derives
// an unrelated secret under each ([runtoken.DeriveKey]), so a route password
// captured on the wire is worth nothing on a leaf or a client listener, and
// none of them is a run token's, a session's or a state-log record's key.
//
// The `crewlet.<surface>.v1` convention every other consumer of the keyring
// names its domain by; a format that changes takes a new version, so a node on
// the old one is refused by name rather than admitted on a stale derivation.
const (
	RouteKeyDomain  = "crewlet.broker.route.v1"
	LeafKeyDomain   = "crewlet.broker.leaf.v1"
	ClientKeyDomain = "crewlet.broker.client.v1"
)

// FleetAuth is what this node's broker presents and accepts on its fleet
// listeners: one credential per key of the Tier A keyring under each
// listener's domain.
//
// EVERY KEY IS ACCEPTED AND THE ACTIVE ONE PRESENTED, the rule every consumer
// of the keyring follows (internal/runtoken's package doc), so the keyring's
// rotation runbook moves the broker with no step of its own: a node that has
// added a key accepts a peer still presenting the old one, and a node that has
// flipped its active key presents a credential every peer that added it holds.
//
// The zero value is a broker given no keyring — a test's solo member, an
// external client — and a broker that opens or dials a fleet listener refuses
// it ([ErrNoFleetKeys]). Every node the engine starts carries a keyring (Tier
// A requires one on every node), so the engine never builds the zero value.
//
// IT HOLDS SECRETS AND PRINTS NONE: [FleetAuth.LogValue] and
// [FleetAuth.String] name key ids, so a [Config] logged or printed whole
// carries no credential.
type FleetAuth struct {
	routes      map[string]routeKey // route username (runtoken.KeyTag) → credential
	routeActive string              // the route username this node presents
	leaves      map[string]leafKey  // leaf public nkey → credential
	leafActive  string              // the leaf public nkey this node presents
	operators   map[string]string   // operator public nkey → the key id it derives from
	activeID    string              // the configured id every presented credential derives from
	ids         []string            // every configured id, in the keyring's order
}

// routeKey is one key's route credential: the configured id it derives from,
// and the password, base64url of the 32 derived bytes.
type routeKey struct{ id, password string }

// leafKey is one key's leaf credential: the configured id it derives from,
// and the nkeys-encoded user seed of the 32 derived bytes, which the link
// presents when this key is the active one.
type leafKey struct{ id, seed string }

// ErrNoFleetKeys is a broker that opens or dials a fleet listener with no
// keyring to authenticate it with.
var ErrNoFleetKeys = errors.New("jetstream: this broker opens or joins a fleet " +
	"listener, and every one is authenticated with a key derived from " +
	"secrets.keys — none was given")

// FleetAuthFrom derives a node's fleet credentials from its keyring —
// config.Secrets.TokenMaterial(), the one builder of a [runtoken.Material].
//
// A Material with no keys is the zero FleetAuth. One whose active id names
// none of its keys cannot present anything, and is refused wrapping
// [runtoken.ErrNoKeyring], as [runtoken.New] refuses it.
//
// # One derivation, the keyring's own
//
// [runtoken.DeriveKey] derives each key on its own, bound to the domain and
// the configured id, and [runtoken.KeyTag] names it without disclosing the id.
// internal/iam/session already uses both for a credential of its own shape. A
// second derivation here would be a second spelling of the rule one rotation
// runbook governs; and the material arrives resolved (Tier A expands its whole
// document before decoding it), so two nodes that spell one key differently
// derive the same credentials.
func FleetAuthFrom(m runtoken.Material) (FleetAuth, error) {
	if len(m.Keys) == 0 {
		return FleetAuth{}, nil
	}
	if !m.Usable() {
		return FleetAuth{}, fmt.Errorf("%w: secrets.active_key_id %q names none of "+
			"secrets.keys, so the fleet's broker has no key to present",
			runtoken.ErrNoKeyring, m.ActiveID)
	}
	a := FleetAuth{
		routes:    make(map[string]routeKey, len(m.Keys)),
		leaves:    make(map[string]leafKey, len(m.Keys)),
		operators: make(map[string]string, len(m.Keys)),
		activeID:  m.ActiveID,
	}
	for _, key := range m.Keys {
		username := runtoken.KeyTag(RouteKeyDomain, key.ID)
		a.routes[username] = routeKey{id: key.ID, password: base64.RawURLEncoding.
			EncodeToString(runtoken.DeriveKey(RouteKeyDomain, key.ID, key.Material))}
		leafSeed, leafPublic, err := derivedNkey(LeafKeyDomain, key)
		if err != nil {
			return FleetAuth{}, err
		}
		a.leaves[leafPublic] = leafKey{id: key.ID, seed: leafSeed}
		_, operatorPublic, err := derivedNkey(ClientKeyDomain, key)
		if err != nil {
			return FleetAuth{}, err
		}
		a.operators[operatorPublic] = key.ID
		a.ids = append(a.ids, key.ID)
		if key.ID == m.ActiveID {
			a.routeActive, a.leafActive = username, leafPublic
		}
	}
	return a, nil
}

// OperatorCredential is the active key's operator nkey: the seed `crewlet
// fleet broker credential` writes for the nats tool, its public key, and the
// configured id it derives from. A Material that cannot sign is
// [ErrNoFleetKeys].
//
// THE OPERATOR PRESENTS IT AND NO NODE DOES: every member accepts every key's
// operator nkey on its loopback client listener, and nothing in the engine
// dials that listener ([Server.ClientListener]).
func OperatorCredential(m runtoken.Material) (seed []byte, public, id string, err error) {
	if !m.Usable() {
		return nil, "", "", fmt.Errorf("%w: secrets.keys holds no key "+
			"secrets.active_key_id names", ErrNoFleetKeys)
	}
	for _, key := range m.Keys {
		if key.ID != m.ActiveID {
			continue
		}
		encoded, derived, deriveErr := derivedNkey(ClientKeyDomain, key)
		if deriveErr != nil {
			return nil, "", "", deriveErr
		}
		return []byte(encoded), derived, key.ID, nil
	}
	return nil, "", "", ErrNoFleetKeys
}

// derivedNkey is one key's nkey user under domain: its encoded seed and its
// public key, from the 32 bytes [runtoken.DeriveKey] derives.
//
// AN NKEY, never a password, wherever the listener lets one be presented:
// what crosses the network is a public key and a signature over the
// connection's own nonce, never the secret — so a leaf link and an operator's
// connection give nothing away to whatever answers at the address they dial.
func derivedNkey(domain string, key runtoken.KeyMaterial) (seed, public string, err error) {
	kp, err := nkeys.FromRawSeed(nkeys.PrefixByteUser,
		runtoken.DeriveKey(domain, key.ID, key.Material))
	if err != nil {
		return "", "", fmt.Errorf("jetstream: derive key %q's %s nkey: %w", key.ID, domain, err)
	}
	raw, err := kp.Seed()
	if err != nil {
		return "", "", fmt.Errorf("jetstream: encode key %q's %s nkey: %w", key.ID, domain, err)
	}
	public, err = kp.PublicKey()
	if err != nil {
		return "", "", fmt.Errorf("jetstream: read key %q's %s nkey: %w", key.ID, domain, err)
	}
	return string(raw), public, nil
}

// IsZero reports a FleetAuth derived from no key.
func (a FleetAuth) IsZero() bool { return len(a.ids) == 0 }

// routeUser is the route credential this node presents: the username and the
// password every route it dials, explicit or gossiped, carries.
func (a FleetAuth) routeUser() (username, password string) {
	return a.routeActive, a.routes[a.routeActive].password
}

// leafSeed is the leaf credential this node's link presents.
func (a FleetAuth) leafSeed() string { return a.leaves[a.leafActive].seed }

// LogValue names the key id every credential this node presents derives from
// and every key id it accepts, never a secret — so a [Config] or a log line
// carrying it carries none.
func (a FleetAuth) LogValue() slog.Value {
	if a.IsZero() {
		return slog.StringValue("no keys")
	}
	return slog.GroupValue(
		slog.String("presents", a.activeID),
		slog.String("accepts", strings.Join(a.ids, ",")))
}

// String renders [FleetAuth.LogValue]'s ids, for its reason: `%v` and `%+v` of
// a [Config] would otherwise print every derived secret.
func (a FleetAuth) String() string {
	if a.IsZero() {
		return "no keys"
	}
	return fmt.Sprintf("presents %s, accepts %s", a.activeID, strings.Join(a.ids, ","))
}

// GoString is [FleetAuth.String], for `%#v`'s sake: the one verb String does
// not cover would print the derived secrets too.
func (a FleetAuth) GoString() string { return a.String() }

// acceptedIDs is every key id this node accepts, sorted — the `accepts` of its
// broker_keys line.
func (a FleetAuth) acceptedIDs() []string {
	ids := slices.Clone(a.ids)
	slices.Sort(ids)
	return ids
}
