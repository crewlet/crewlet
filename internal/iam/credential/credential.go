// Package credential is what somebody proves themselves with, and the
// arithmetic behind each of them: a password, a second factor, a recovery
// code, and a machine's bearer token.
//
// # What is stored is a VERIFIER, never a secret
//
// Every value this package puts in front of the identity estate is something a
// presented secret is checked AGAINST and which cannot be presented to
// anything: an argon2id digest with its own parameters beside it, a SHA-256 of
// a machine token, a TOTP shared secret sealed under the fleet keyring and
// bound to its person and its credential.
// The estate is replicated to every node, snapshotted, backed up and donated
// to joining peers — so a value that could be replayed out of it would be a
// credential every operator with a backup holds.
//
// # The enumeration oracle, which is the failure this package is shaped by
//
// A sign-in endpoint is the one surface an unauthenticated stranger may
// address, and everything about how it answers is evidence. If an unknown
// login is refused faster than a known one, the endpoint is a ROSTER: an
// attacker learns who works here in as many requests as they care to make, and
// that list is worth more than any single password. Two mechanisms close it:
//
//   - THE THROTTLE IS KEYED ON WHAT WAS TYPED, NEVER ON WHAT IT RESOLVED TO,
//     and decides before anything is looked up. A throttle keyed on who the
//     subject turned out to be is one only real people can trigger, so its
//     delay itself becomes the oracle. [Throttle.Admit] takes the SOURCE and
//     the subject as the caller typed it, and a name nobody holds climbs the
//     curve exactly as a real one does.
//   - A SUBJECT THAT DOES NOT EXIST IS STILL VERIFIED AGAINST: what was
//     presented is checked against one dummy verifier at this build's cost
//     ([Hasher.Decoy]), so a miss is one argon2id derivation under the same
//     cap exactly as a hit is — and that derivation is what dominates how
//     long a sign-in takes.
//
// THE RESIDUAL IS STATED, NOT PADDED. What still differs between the arms is
// the directory read before the derivation — a name nobody holds is answered
// by an index a little sooner — and a real verifier written at another cost
// than this build's, which takes that cost's time until the next sign-in
// rewrites it. Every refusal used to be padded to one deadline measured from
// admission, and holding that deadline under load took a turn per address at
// the verify cap and a decoy whose hold was drawn from the node's recent
// derivations: a scheduler and a sampler to hide one indexed read.
//
// # A failure costs delay, never a lockout
//
// A hard refusal after N failures is a lockout an outsider can cause — keyed
// on a login, of that person; keyed on an address, of everybody behind it. So
// each failure doubles the wait before its key's next attempt, one second to
// thirty, and a correct credential after the wait always succeeds. The key is
// the (typed subject, source) PAIR, which catches a run at one account, and a
// success clears its own pair and nothing else, because clearing a whole
// source let anybody holding an account wipe the record of their guesses at
// somebody else's by signing in as themselves. throttle.go carries the whole
// argument.
//
// # And the source is bounded by what it costs, never refused
//
// A run across many names meets a fresh pair every time, so no curve slows it
// — and a curve on the source alone was a refusal anybody sharing the address
// held shut for everybody else. What bounds one address instead is COST: every
// name it tries is an argon2id derivation waiting for a slot of the verify cap
// ([VerifyCap]). A second factor is the one curve keyed on the PERSON
// ([Throttle.AdmitSecondFactor]), because only somebody holding the password
// can reach it, and without it they divide the pair's curve by every address
// they own.
//
// # Each node keeps its own curve
//
// Nothing here is shared across a fleet: a run a load balancer rotates across
// N nodes meets N curves, and that factor is a stated residual rather than a
// gap — throttle.go says what bounds it and what sharing it cost.
//
// # No new modules
//
// The TOTP implementation is RFC 6238 written out over crypto/hmac,
// crypto/sha1 and encoding/base32, which is forty lines of arithmetic against
// a dependency with its own release cadence and its own idea of what a
// tolerable clock skew is. argon2id is golang.org/x/crypto/argon2, which is
// promoted from indirect to direct here: a password hash is not something to
// hand-write, it is maintained by the same people who maintain the standard
// library's crypto, and there is no second implementation of it to disagree
// with.
package credential

import "errors"

// ErrWeak reports a password the company will not accept.
//
// THE ONLY REFUSAL IN THIS PACKAGE THAT SAYS WHAT IS WRONG, and the asymmetry
// is the difference between two audiences. A failed SIGN-IN is answered to a
// stranger, and "no such login", "wrong password", "wrong code" and "that code
// was already used" are four different facts whose difference tells an
// attacker as much as it tells the caller — the first of them is the company's
// roster. So every verification here answers a BOOL and the route composes one
// generic refusal from it; the sentinel for that refusal belongs where it is
// RETURNED rather than here, where nothing would produce it.
//
// This one is answered to somebody who has already proved who they are and is
// choosing a new secret. A generic refusal there is somebody typing variations
// until one sticks.
var ErrWeak = errors.New("credential: this password cannot be used")
