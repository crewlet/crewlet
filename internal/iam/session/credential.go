package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// A SESSION THAT STANDS FOR A CONFIGURED SECRET IS BOUND TO ITS VALUE.
//
// `POST /auth/token` exchanges a Tier A bearer for a session, and that
// session's subject is the token's LOGIN — `token:<id>` — from which the guard
// re-composes it on every request, out of the entry this node holds under that
// id NOW. Keyed on the id alone, the session answered to the NAME of the
// credential and not to the credential: an operator who answered a leak by
// putting a new value under the same id left every session exchanged from the
// leaked value working for the rest of its hour — which is exactly the hour a
// leak is answered in, and the one thing rotating the value was for.
//
// So the bearer carries a BINDING: a MAC over the value the session was
// exchanged with, checked on every request against the value this node holds
// for that id — through [Identity.Credential], which the directory a bearer is
// validated against fills from the configuration. A different value is a
// session that is over, on every node, whether or not it has applied anything,
// because the bearer and the configuration are both already on hand.
//
// # Why a MAC under the signing key, and never a hash
//
// A cookie is the value most likely to end up somewhere nobody meant it to,
// and a Tier A value is only as strong as whoever wrote it — the floor is a
// length, not an entropy. An unkeyed digest of it in the cookie would be an
// offline guessing oracle for the deployment's break-glass credential, handed
// to anybody who reads a browser profile. Keyed under a key derived from the
// one that signs the bearer, the binding says nothing to anybody who does not
// already hold the keyring — and whoever holds that can mint sessions anyway.
//
// A RE-ISSUE RECOMPUTES IT under the key the new bearer is signed with, which
// is what keeps a keyring rotation draining exchanged sessions onto the new
// key like every other: the value it is recomputed from is the one the node
// just checked the old binding against.

// Credential is a configured secret a session stands for — a Tier A token's
// value — held so a bearer can be bound to it and checked against it.
//
// THREE STATES, and the zero value is the first: NONE, a person's session,
// which answers to rows rather than to a configured value; a VALUE
// ([CredentialOf]); and WITHDRAWN ([Withdrawn]) — a subject that is a
// configured credential with nothing configured under its name any more,
// because its entry was removed or renamed. Withdrawn is not none: a session
// exchanged from a token that no longer exists is over, and one answering to
// none would be served as though it were a person's.
//
// ITS VALUE NEVER PRINTS: [Credential.String] and [Credential.GoString] are
// redacted, so a log line or an error that formats an [Identity] carries the
// fact that a credential is there and never the credential.
type Credential struct {
	secret     string
	configured bool
}

// CredentialOf wraps a configured secret.
func CredentialOf(secret string) Credential {
	return Credential{secret: secret, configured: true}
}

// Withdrawn is the credential of a subject that is a configured credential
// with nothing configured under its name any more.
func Withdrawn() Credential { return Credential{configured: true} }

// IsZero reports that there is no credential: a person's session, which
// answers to rows rather than to a configured value.
func (c Credential) IsZero() bool { return !c.configured }

// String is the redacted form every formatting verb but %#v prints.
func (c Credential) String() string {
	switch {
	case c.IsZero():
		return "none"
	case c.secret == "":
		return "withdrawn"
	}
	return "[redacted]"
}

// GoString is the redacted form %#v prints.
func (c Credential) GoString() string { return "session.Credential{" + c.String() + "}" }

// credentialDomain separates a binding's key from the key it is derived from,
// which also signs every bearer: a binding is never a signature over a payload
// and a signature is never a binding.
const credentialDomain = "crewlet/iam/session/credential-binding/v1"

// bindingUnder is the binding a bearer signed under key carries for cred.
func bindingUnder(key []byte, cred Credential) string {
	sub := hmac.New(sha256.New, key)
	sub.Write([]byte(credentialDomain))
	mac := hmac.New(sha256.New, sub.Sum(nil))
	mac.Write([]byte(cred.secret))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// bound reports whether a bearer stands for the credential its subject answers
// to now, and why not when it does not.
//
// BOTH DIRECTIONS REFUSE. A bearer carrying a binding whose subject answers to
// no configured value, and one carrying none whose subject does, are each a
// session nothing can vouch for: the first names a credential this node does
// not hold under that name, and the second was minted without the value it
// stands for — so neither is served, and neither is guessed about.
func (s *Signer) bound(b Bearer, cred Credential) (bool, string) {
	switch {
	case b.Binding == "" && cred.IsZero():
		return true, ""
	case cred.IsZero():
		return false, "the bearer is bound to a configured credential and " +
			"its subject answers to none"
	case cred.secret == "":
		return false, "the configured credential this session was exchanged " +
			"from is no longer configured"
	case b.Binding == "":
		return false, "the subject answers to a configured credential and " +
			"the bearer was minted without one"
	}
	key, held := s.keys[b.KeyTag]
	if !held {
		// Unreachable from a parsed bearer, whose tag verified; refused
		// rather than assumed, for the reason every unknown tag is.
		return false, "the bearer's key is not one this node holds"
	}
	if !hmac.Equal([]byte(b.Binding), []byte(bindingUnder(key, cred))) {
		return false, "the configured credential this session was exchanged " +
			"from has a different value now"
	}
	return true, ""
}
