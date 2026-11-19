package secrets

import (
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// THE ENGINE'S OWN KEY MATERIAL shares the operator's store (every node must
// derive the blind-index key the directory matches an address under) without
// sharing a namespace. Operator credentials are named by the environment-variable
// grammar; under it a PUT or DELETE would replace the blind-index key and orphan
// every address, and an `mcp_env` reference could hand it to a child process.
// An engine-owned name is a PATH (`iam/blind-index-key`) and the reference
// grammar has no `/`, so no `${VAR}` can name one, by shape rather than by a
// check. Operator surfaces refuse the prefix ([ErrReservedName]) and neither
// list nor snapshot a reserved row; only the engine's view ([CheckEstateName])
// writes one.

// ErrReservedName reports an operator surface asked to address the engine's own
// key material.
//
// Its own sentinel because it is neither the caller's typo ([ErrInvalidName])
// nor an absent row ([ErrNotFound]): the name is well formed and may well
// exist, and the answer is that no operator gesture reaches it — a lost
// blind-index key is restored with the coordination store it lived in.
var ErrReservedName = errors.New(
	"that name is reserved for the engine's own keys")

// estateOwners are the subsystems that keep key material in the store, each
// under its own first path segment: the identity estate (`iam/`), and the MCP
// servers' OAuth grants (`mcp/`), each a refresh token the engine renews a
// server's access with and no person reads back (internal/mcp/oauth).
//
// A CLOSED SET, so a name is reserved by its first segment rather than by
// whatever a caller happened to put a slash in. An owner joins it BEFORE it
// writes its first key: a key written under an owner nobody reserved is an
// ordinary operator row for as long as that lasts — listed, revealable,
// deletable by anybody holding the grant, and decrypted into every node's
// `${VAR}` snapshot — which is exactly the exposure the namespace exists to
// close.
var estateOwners = []string{"iam", "mcp"}

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
// THE SEGMENTS ARE NARROW because every one of them is a fixed word or an id
// this engine minted, and a name nothing parses back must still never alias
// another — an empty segment is an id somebody forgot to fill in, and the name
// built around it would be one key every such caller shared.
func CheckEstateName(name string) error {
	if !Reserved(name) {
		return fmt.Errorf("%w: %q is not under one of the engine's own owners, %s",
			ErrInvalidName, name, words.And(estateOwners))
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
// sealed under it — dropping that key would make the blind-index key
// unreadable, and every sign-in by address with it. A count per key answers
// that without naming the engine's rows on an operator's screen.
type EngineKeys struct {
	Total int            `json:"total"`
	ByKey map[string]int `json:"by_key,omitempty"`
}

// StaleUnder is how many engine rows are sealed under a key other than active.
func (k EngineKeys) StaleUnder(active string) int {
	return KeyCounts(k.ByKey).Outside(active)
}
