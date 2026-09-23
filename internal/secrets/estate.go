package secrets

import (
	"errors"
	"fmt"
	"strings"
)

// THE ENGINE'S OWN KEY MATERIAL, and why it shares a store with the operator's
// credentials without sharing a namespace.
//
// The company's secret store holds two kinds of thing. The OPERATOR's
// credentials — a provider's API key, a vendor token — are named by the
// environment-variable grammar, because a `${VAR}` in the company document is
// how they are used: listed, revealed, rotated and resolved into a provider or
// a child process. And the ENGINE's key material: a person's data key and a
// human seat's, whose deletion is what removing either does; an OIDC session's
// refresh token, a credential at somebody else's identity provider; the
// blind-index keys the identity directory and the org chart each match an
// address under; the key the org chart seals a literal credential with. Those
// live in the same store because it is the one place a delete reaches every
// node at once and nothing on the request path reads it back.
//
// # Why the two must not meet
//
// They used to share the environment-variable grammar, which made the engine's
// keys ordinary operator secrets. `/secrets` listed every person's key and every
// signed-in session's refresh token; a reveal handed out a person's live token
// at their identity provider, and a copy taken before a removal defeated the
// shred the removal is; a PUT or a DELETE shredded somebody with no removal on
// record, or replaced a blind-index key and orphaned every address in the
// directory; and every apply decrypted all of it into each node's `${VAR}`
// resolver, where `${IAM_SESSION_…_REFRESH}` in an `mcp_env` handed somebody
// else's credential to a child process.
//
// # A different SHAPE, so no reference grammar can reach it
//
// An engine-owned name is a PATH — `iam/person/<id>/dek` — and the reference
// grammar has no `/` in it, so no `${VAR}` in any document can name one, by the
// shape of the value rather than by a check a resolver has to remember to run.
// The operator surfaces refuse the prefix outright ([ErrReservedName]), and the
// store's operator view neither lists nor snapshots a reserved row; only the
// engine's own view ([CheckEstateName]) writes one.

// ErrReservedName reports an operator surface asked to address the engine's own
// key material.
//
// Its own sentinel because it is neither the caller's typo ([ErrInvalidName])
// nor an absent row ([ErrNotFound]): the name is well formed and may well
// exist, and the answer is that no operator gesture reaches it — removing a
// person is `crewlet iam remove`, removing a seat is the org chart's own
// removal, and a lost blind-index key is restored with the coordination store
// it lived in.
var ErrReservedName = errors.New(
	"secrets: that name is the engine's own key material, which no operator " +
		"surface reads, writes or resolves")

// estateOwners are the subsystems that keep key material in the store, each
// under its own first path segment: the identity estate (`iam/`) and the org
// chart (`chart/`).
//
// A CLOSED SET, so a name is reserved by its first segment rather than by
// whatever a caller happened to put a slash in. An owner joins it BEFORE it
// writes its first key: a key written under an owner nobody reserved is an
// ordinary operator row for as long as that lasts — listed, revealable,
// deletable by anybody holding the grant, and decrypted into every node's
// `${VAR}` snapshot — which is exactly the exposure the namespace exists to
// close.
var estateOwners = []string{"iam", "chart"}

// Reserved reports whether a name is in the engine's own namespace — the
// question every operator surface asks before it touches a row.
//
// BY PREFIX, and deliberately wider than [CheckEstateName]'s grammar: a
// malformed name under a reserved owner is still not the operator's to read,
// write or delete, and a surface that refused only well-formed ones would
// admit exactly the names nobody meant to write.
func Reserved(name string) bool {
	for _, owner := range estateOwners {
		if strings.HasPrefix(name, owner+"/") {
			return true
		}
	}
	return false
}

// CheckEstateName refuses a name the engine may not keep its own material
// under: a reserved owner, then one or more non-empty segments of lower-case
// letters, digits, `-`, `_` and `.`.
//
// THE SEGMENTS ARE NARROW because every one of them is an id this engine minted
// (a uuid, a lineage) or a fixed word, and a name nothing parses back must still
// never alias another — an empty segment would make `iam/person//dek` the one
// key every unidentified caller shared.
func CheckEstateName(name string) error {
	if !Reserved(name) {
		return fmt.Errorf("%w: %q is not under one of the engine's own owners %v",
			ErrInvalidName, name, estateOwners)
	}
	segments := strings.Split(name, "/")
	if len(segments) < 2 {
		return fmt.Errorf("%w: %q names an owner and nothing under it",
			ErrInvalidName, name)
	}
	for _, segment := range segments[1:] {
		if segment == "" {
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidName, name)
		}
		for _, r := range segment {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
				r == '-', r == '_', r == '.':
			default:
				return fmt.Errorf("%w: %q carries %q, and an engine-owned name "+
					"is lower-case letters, digits, '-', '_' and '.'",
					ErrInvalidName, name, r)
			}
		}
	}
	return nil
}

// EngineKeys summarises the engine's own key material in a store WITHOUT
// NAMING ANY OF IT: how many rows there are, and how many are sealed under each
// keyring key.
//
// IT EXISTS FOR THE ROTATION. A rekey re-seals these rows with everything else,
// and the operator retiring the old key needs to know that none is still
// sealed under it — dropping that key would make every person's name, every
// refresh token and the blind-index key unreadable at once. A count per key
// answers that without putting a person's id or a session's lineage on an
// operator's screen.
type EngineKeys struct {
	Total int            `json:"total"`
	ByKey map[string]int `json:"by_key,omitempty"`
}

// StaleUnder is how many engine rows are sealed under a key other than active.
func (k EngineKeys) StaleUnder(active string) int {
	stale := 0
	for key, n := range k.ByKey {
		if key != active {
			stale += n
		}
	}
	return stale
}
