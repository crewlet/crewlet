package credential

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// THE PASSWORD PARAMETERS, and what each one buys.
//
// Asserted BY VALUE in the suite: a cost that drifts down is invisible, and only
// an offline attack on a stolen database gets faster.
//
// A CHANGE MAY ONLY RAISE THEM, each on its own. A verifier is rewritten only
// where this build's cost is above its in some parameter and below in none
// ([Hasher.Verify]): a rolling upgrade runs two builds on one estate, and a
// rewrite in either direction flaps verifiers between them. A change lowering
// one parameter to raise another reaches no existing verifier.
const (
	// Memory is argon2id's memory cost in KiB: 64 MiB.
	//
	// THE PARAMETER THAT ACTUALLY DEFENDS, because it is the one a GPU
	// cannot buy its way around — a card with thousands of cores has
	// nothing like thousands of times the memory bandwidth, so raising
	// this costs an attacker far more than it costs the engine. 64 MiB is
	// the OWASP-recommended floor for t=3, and it is what bounds
	// [VerifyCap]: a node running N verifications at once needs N × this,
	// so an unbounded endpoint would be a memory exhaustion an
	// unauthenticated caller can trigger.
	Memory uint32 = 64 * 1024

	// Time is the number of passes: 3, which is the pairing OWASP states
	// for 64 MiB. Raising it trades linearly against login latency and
	// buys linearly against an attacker, where memory buys superlinearly
	// — so memory goes up first and this follows only when memory cannot.
	Time uint32 = 3

	// Threads is argon2id's parallelism: ONE.
	//
	// NOT the machine's core count, and this is the parameter most often
	// set wrong. Parallelism makes ONE verification finish sooner by
	// using more cores; it does nothing against an attacker, who is
	// already running every core they own on different candidates. What
	// it costs is the engine's ability to serve concurrent sign-ins: at
	// p = NumCPU one login saturates the machine. One thread per
	// verification, [VerifyCap] of them at once, is the arrangement where
	// the cost is spent on the attacker rather than on latency.
	Threads uint8 = 1

	// KeyLen is the digest length in bytes, and SaltLen the salt's. 32 and
	// 16 are argon2's own recommendations; neither is a security knob
	// anybody should be turning. Typed for what each is handed to: argon2
	// takes the digest length as a uint32, and the salt's sizes a slice.
	KeyLen  uint32 = 32
	SaltLen int    = 16
)

// VerifyCap is how many argon2id verifications this process runs at once.
//
// DERIVED, NOT CONFIGURED: max(1, NumCPU / Threads). Each verification holds
// [Memory], so the cap stops an unauthenticated caller turning sign-in into
// 64 MiB per concurrent request (6.4 GiB at a hundred in flight: an OOM kill).
// Above the cap requests QUEUE: a slow sign-in under attack, not a dead node.
//
// It divides by [Threads] because that is what one verification occupies;
// stating the division keeps the two in step if the parameter moves.
func VerifyCap() int {
	if n := runtime.NumCPU() / int(Threads); n > 1 {
		return n
	}
	return 1
}

// Hasher hashes and verifies passwords under one set of parameters.
//
// A TYPE RATHER THAN PACKAGE FUNCTIONS, because the concurrency cap has to be
// SHARED to mean anything: two hashers each admitting NumCPU verifications
// admit twice the memory the cap was computed to bound. The composition root
// builds ONE and hands it to everything that verifies a password — and the
// type is what makes that a visible dependency rather than a package-level
// variable nobody can see.
//
// SAFE FOR CONCURRENT USE.
type Hasher struct {
	params Params

	// slots is the cap: a derivation holds one of its [VerifyCap] places
	// for as long as it runs. A PLAIN SEMAPHORE, first come first served,
	// whose one rule beyond the count is that a waiter whose request went
	// away leaves ([Hasher.take]). It was a set of per-address lanes
	// served in turn, each address holding one slot at most, which bought
	// fairness between addresses under a flood and a timing equalisation
	// that only a refusal pad needed — a pad every failed sign-in was once
	// held to, and which is gone — at the price of a scheduler of its own.
	// Under a flood every arm of a sign-in queues here alike, which is what
	// keeps the queue from saying which names exist.
	slots chan struct{}

	// dummySalt and dummyDigest are the verifier a subject with nothing to
	// check is verified against ([Hasher.Decoy]): at this hasher's own cost,
	// over a salt and a digest drawn once for the life of the process. HELD
	// AS BYTES, never as a PHC string the decoy decodes, because the decoy
	// is also what [Hasher.Verify] answers an unreadable verifier with — a
	// decoy that went back through Verify recursed without end, until the
	// stack overflowed and took the process with it, for a hasher whose own
	// dummy did not decode.
	dummySalt, dummyDigest []byte

	// work is the derivation. A hasher's own suite replaces it, to decide
	// from outside when a derivation ends and to see what it was asked to
	// derive; nothing else ever does.
	work func(password string, salt []byte, params Params) []byte
}

// Params are one hasher's cost settings.
//
// THEY EXIST SO A TEST CAN RUN AT A COST A TEST CAN AFFORD, and for nothing
// else: [Default] is what ships, the suite asserts its values, and every
// other set is a test's. A config field here would be an operator's chance to
// turn the cost down by accident, with nothing that looks wrong afterwards.
type Params struct {
	Memory  uint32
	Time    uint32
	Threads uint8
	KeyLen  uint32
}

// Default is the shipped cost.
func Default() Params {
	return Params{Memory: Memory, Time: Time, Threads: Threads, KeyLen: KeyLen}
}

// NewHasher builds one. A zero Params takes [Default]; a zero cap takes
// [VerifyCap].
//
// IT PANICS on a cost argon2 cannot derive at ([Params.derivable]): zero
// passes, zero lanes or an empty digest. [Params] is a test's and [Default]
// is derivable, so such a hasher is a programming error — and built, every
// password it hashed or verified would panic inside argon2 with a derivation
// slot held, and every verifier it wrote would be one [decode] refuses.
func NewHasher(params Params, cap int) *Hasher {
	if params == (Params{}) {
		params = Default()
	}
	if !params.derivable() {
		panic(fmt.Sprintf("credential: NewHasher given %+v, a cost argon2 "+
			"cannot derive at — it needs at least one pass (Time), one lane "+
			"(Threads) and one byte of digest (KeyLen)", params))
	}
	if cap <= 0 {
		cap = VerifyCap()
	}
	return &Hasher{params: params, slots: make(chan struct{}, cap), work: argon2id,
		// THE DUMMY IS DRAWN, NEVER DERIVED: a verification against it
		// costs exactly what one against a real verifier at this cost
		// does, and what it is compared with is never read — so a digest
		// of random bytes is as good as a digest of anything, and the
		// process pays nothing to make it.
		dummySalt: random(SaltLen), dummyDigest: random(int(params.KeyLen))}
}

// random is n bytes of crypto/rand.
func random(n int) []byte {
	out := make([]byte, n)
	// crypto/rand does not fail: it aborts the process rather than return
	// an error, so there is nothing here to handle.
	_, _ = rand.Read(out)
	return out
}

// Params is this hasher's cost, for a caller that has to report it.
func (h *Hasher) Params() Params { return h.params }

// Hash produces a verifier for a password, under the cap ([VerifyCap]).
//
// IT DOES NOT CHECK STRENGTH. [CheckStrength] is a separate call because the
// two answer different people: a strength failure is told to somebody choosing
// a password and must say what is wrong, while a hash is also produced for an
// imported credential and for a test. Folding them would make every caller
// handle a refusal it cannot act on.
//
// The error is ctx's when the request went away before a slot freed, and
// nothing was derived.
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	salt := random(SaltLen)
	digest, err := h.derive(ctx, password, salt, h.params)
	if err != nil {
		return "", err
	}
	return h.encode(salt, digest), nil
}

// Verify reports whether a password matches a verifier, and whether the
// verifier should be REWRITTEN at this hasher's current cost.
//
// THE PARAMETERS ARE IN THE STORED STRING because the plaintext is not: a
// stronger digest exists only when somebody presents their password. An
// older-cost verifier verifies under its OWN parameters and is reported stale,
// WEAKER NEVER MERELY DIFFERENT ([Params.weakerThan]), else mid-rollout builds
// rewrite each other's verifiers. AN UNPARSEABLE VERIFIER IS A REFUSAL, NOT AN
// ERROR PATH: verified as a decoy is ([Hasher.Decoy]), it must look like a wrong
// password. UNDER THE CAP AS A HASH ([VerifyCap]); the error is ctx's.
func (h *Hasher) Verify(ctx context.Context, verifier, password string) (
	ok bool, rehash bool, err error) {

	params, salt, want, err := decode(verifier)
	if err != nil {
		return false, false, h.Decoy(ctx, password)
	}
	got, err := h.derive(ctx, password, salt, params)
	if err != nil {
		return false, false, err
	}
	// CONSTANT TIME. A byte-by-byte compare over a digest leaks it one
	// byte at a time to anybody who can time the endpoint — and this
	// endpoint is deliberately reachable with no other credential.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false, nil
	}
	return true, params.weakerThan(h.params), nil
}

// Decoy verifies what was presented for a subject with no verifier to check
// (nobody by that name, a person who may not act, one with no password) against
// ONE DUMMY VERIFIER at this hasher's own cost, and discards the answer.
//
// A miss without a derivation would answer in microseconds where a hit pays an
// argon2id verification: the roster, off a stopwatch. So it does what
// [Hasher.Verify] does past the decode (one derivation at the same cost under
// the same cap, the same constant-time compare) against a salt and digest drawn
// once per process. It skips the decode, as Verify answers an unreadable
// verifier with a decoy. NOT EQUALISED: the directory read before it, and a
// REAL verifier at another cost until [Hasher.Rehash]. THE RESULT IS DISCARDED
// BY CONSTRUCTION: a readable answer would be a second oracle.
func (h *Hasher) Decoy(ctx context.Context, presented string) error {
	got, err := h.derive(ctx, presented, h.dummySalt, h.params)
	if err != nil {
		return err
	}
	// THE COMPARE A VERIFICATION MAKES, and its answer thrown away: the
	// compare is part of what a hit costs, and nothing may branch on it.
	_ = subtle.ConstantTimeCompare(got, h.dummyDigest)
	return nil
}

// weakerThan reports whether p is a WEAKER cost than q: below it in at least
// one parameter an attacker pays for, and above it in none.
//
// A PARTIAL ORDER, deliberately, not a product or other single number: lowering
// ANY parameter is a downgrade in it, and judging it paid for elsewhere is what
// argon2's trade-offs make unreliable. Antisymmetry makes it safe mid-rollout:
// no two builds each find the other's verifier weaker.
//
// [Params.Threads] IS NOT A COST and decides nothing: parallelism changes how
// soon one verification finishes, not what an attacker pays (see [Threads]),
// and builds disagreeing about it would rewrite verifiers on every sign-in.
func (p Params) weakerThan(q Params) bool {
	if p.Memory > q.Memory || p.Time > q.Time || p.KeyLen > q.KeyLen {
		return false
	}
	return p.Memory < q.Memory || p.Time < q.Time || p.KeyLen < q.KeyLen
}

// Rehash is [Hash] but only if a slot of the cap is free NOW; else it answers
// [ErrSaturated], deriving nothing.
//
// It is for the ONE hash nobody waits on: the rewrite follows a sign-in that has
// already answered. So it never QUEUES for the cap, full exactly under the load
// it exists for: it would take a slot ahead of later sign-ins. A miss is
// harmless; the next sign-in asks again.
//
// NO CONTEXT: a slot is taken at once or not at all, and runs to the end (argon2
// is not interruptible). NEVER AHEAD OF A WAITER: a freed slot goes straight to
// a blocked sender as it is received, so one is free here only while nobody is
// queued.
func (h *Hasher) Rehash(password string) (string, error) {
	select {
	case h.slots <- struct{}{}:
	default:
		return "", ErrSaturated
	}
	defer func() { <-h.slots }()
	salt := random(SaltLen)
	return h.encode(salt, h.work(password, salt, h.params)), nil
}

// ErrSaturated reports a [Hasher.Rehash] that found every slot of the
// concurrency cap taken and derived nothing.
var ErrSaturated = errors.New("credential: every derivation slot is taken, " +
	"and a rewrite nobody is waiting on does not queue for one")

// derive runs the cost at params under the cap: this hasher's own cost for a
// new verifier, the stored verifier's for a verification. With
// [Hasher.Rehash] it is the only caller of [Hasher.work], so no derivation
// this package runs can skip the cap.
//
// IT WAITS FOR A SLOT FOR AS LONG AS ITS REQUEST DOES: a verifying sign-in is
// queued, not refused (a refusal would read as a wrong password), but a request
// that went away gives up its place.
//
// THE SLOT IS TAKEN AROUND THE DERIVATION AND NOTHING ELSE: held across a store
// read, one slow database would turn the password cost into a queue, taking a
// node down under the load the cap exists for.
func (h *Hasher) derive(ctx context.Context, password string, salt []byte,
	params Params) ([]byte, error) {

	release, err := h.take(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	return h.work(password, salt, params), nil
}

// take waits for a slot of the cap, answering what gives it back — or ctx's
// error, with nothing held, when the request went away first. A request that
// is already gone takes nothing, even where a slot is free.
func (h *Hasher) take(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// HoldSlot takes a slot of h's cap and answers what gives it back, for a suite
// outside this package that has to fill the cap.
//
// EXPORTED FOR A TEST AND SAYING SO: that every arm of a sign-in route — a
// name nobody holds included — spends a derivation under the cap is a property
// of the route's wiring, and the only way to see it from outside is to hold
// every slot and watch each arm's request wait for one.
func HoldSlot(ctx context.Context, h *Hasher) (func(), error) { return h.take(ctx) }

// argon2id is the one derivation, and it is called only with a slot of the
// cap held — through [Hasher.derive] or [Hasher.Rehash].
func argon2id(password string, salt []byte, params Params) []byte {
	return argon2.IDKey([]byte(password), salt, params.Time, params.Memory,
		params.Threads, params.KeyLen)
}

// encode writes the PHC string this engine stores.
//
// THE PHC FORMAT rather than a shape of this project's own, because it is what
// every other implementation of argon2 reads and writes: an operator moving
// off this engine can take their verifiers with them, and one moving onto it
// can bring them. Writing a private format would make a migration a password
// reset for everybody.
func (h *Hasher) encode(salt, digest []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, h.params.Memory, h.params.Time, h.params.Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest))
}

// errVerifier reports a stored verifier this build cannot read.
var errVerifier = errors.New("credential: unreadable verifier")

// decode reads a PHC string back, including the parameters it was written
// under — which is the half that makes a cost raise possible at all.
func decode(verifier string) (Params, []byte, []byte, error) {
	parts := strings.Split(verifier, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, digest
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errVerifier
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil ||
		version != argon2.Version {
		return Params{}, nil, nil, errVerifier
	}
	var params Params
	var threads int
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d",
		&params.Memory, &params.Time, &threads); err != nil ||
		threads < 0 || threads > 255 {
		return Params{}, nil, nil, errVerifier
	}
	params.Threads = uint8(threads)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errVerifier
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, errVerifier
	}
	params.KeyLen = uint32(len(digest))
	if !params.derivable() {
		return Params{}, nil, nil, errVerifier
	}
	return params, salt, digest, nil
}

// derivable reports whether p is a cost argon2 can derive at: at least one
// pass, one lane and one byte of digest.
//
// argon2 PANICS below the first two rather than refusing them, and a digest of
// nothing is no verifier at all. So a stored verifier outside it is unreadable
// ([decode]) and answers as a wrong password does, rather than taking a
// derivation slot and crashing the request that presented it — which is what a
// verifier stating zero passes did, since only the lanes and the digest were
// checked — and a hasher outside it is never built ([NewHasher]).
func (p Params) derivable() bool {
	return p.Time >= 1 && p.Threads >= 1 && p.KeyLen >= 1
}
