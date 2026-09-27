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
	"sync/atomic"
	"time"

	"golang.org/x/crypto/argon2"
)

// THE PASSWORD PARAMETERS, and what each one buys.
//
// They are stated as constants and asserted BY VALUE in the suite, because a
// cost that drifts down is invisible: every test still passes, every login
// still works, and the only thing that changed is how long an offline attack
// against a stolen database takes.
//
// A CHANGE MAY ONLY RAISE THEM, each on its own. A stored verifier is
// rewritten at this build's cost only where that cost is above the verifier's
// in some parameter and below it in none ([Hasher.Verify]), because a rolling
// upgrade runs two builds against one identity estate and a rewrite in either
// direction flaps every person's verifier between them — half of each sign-in
// at the weaker cost. So a change that lowers one parameter to raise another
// is one no existing verifier ever reaches: everybody keeps the cost they were
// enrolled at until they choose a new password.
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
// [Memory] for its duration, so the cap is what stops an unauthenticated
// caller turning a sign-in endpoint into 64 MiB per concurrent request — at a
// hundred in flight that is 6.4 GiB, which is an out-of-memory kill rather
// than a slow login. Above the cap requests QUEUE, which is the correct
// failure: a slow sign-in under attack, rather than a dead node — and they
// queue by SOURCE, one slot per source at a time, served in turn, so the
// address sending the flood is the address that waits for it (turns.go).
//
// It divides by [Threads] because that is what one verification actually
// occupies. With Threads at 1 the two are the same number, and stating the
// division rather than the result is what keeps them in step if the parameter
// ever moves.
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
	turns  *turns

	// took is how long a derivation takes here, in nanoseconds: a moving
	// average over every one this hasher has run, or zero until one has.
	// It is what a decoy holds its turn for ([Hasher.Decoy]).
	took atomic.Int64

	// work is the derivation, and hold how a decoy spends its turn. A
	// hasher's own suite replaces them, to decide from outside when a
	// derivation ends and to see what a decoy was asked to hold; nothing
	// else ever does.
	work func(password string, salt []byte, params Params) []byte
	hold func(ctx context.Context, d time.Duration)
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
func NewHasher(params Params, cap int) *Hasher {
	if params == (Params{}) {
		params = Default()
	}
	if cap <= 0 {
		cap = VerifyCap()
	}
	return &Hasher{params: params, turns: newTurns(cap),
		work: argon2id, hold: sleepUntil}
}

// Params is this hasher's cost, for a caller that has to report it.
func (h *Hasher) Params() Params { return h.params }

// Hash produces a verifier for a password, in source's turn — the address the
// request that chose it came from.
//
// IT DOES NOT CHECK STRENGTH. [CheckStrength] is a separate call because the
// two answer different people: a strength failure is told to somebody choosing
// a password and must say what is wrong, while a hash is also produced for an
// imported credential and for a test. Folding them would make every caller
// handle a refusal it cannot act on.
//
// The error is ctx's when the request went away before its turn came, and
// nothing was derived.
func (h *Hasher) Hash(ctx context.Context, source, password string) (string, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("credential: read a salt: %w", err)
	}
	digest, err := h.derive(ctx, source, password, salt, h.params)
	if err != nil {
		return "", err
	}
	return h.encode(salt, digest), nil
}

// Verify reports whether a password matches a verifier, and whether the
// verifier should be REWRITTEN at this hasher's current cost.
//
// THE REHASH FLAG IS WHY THE PARAMETERS ARE IN THE STORED STRING. Raising the
// cost is otherwise a migration nobody can perform — the plaintext is not
// stored, so the only instant at which a stronger digest can be computed is
// the one where somebody presents their password. A verifier written under a
// weaker cost verifies under its OWN parameters and is reported stale, and the
// caller rewrites it in the record that records the successful login.
//
// STALE MEANS WEAKER, NEVER MERELY DIFFERENT ([Params.weakerThan]). The only
// place the cost is chosen is a build's [Default], so the only way it moves is
// a new build — and a new build arrives as a rolling upgrade, two builds
// sharing one identity estate. Reported whenever the parameters differed, a
// node still on the older build read every verifier the newer one had written
// as stale and rewrote it at its own, weaker cost, while a sign-in on an
// upgraded node rewrote it back up: every person's verifier flapped for the
// whole rollout and spent part of it at the cost the upgrade was retiring,
// which is the silent downgrade [Memory] and [Time] are pinned against. A
// verifier at a cost that is higher in any parameter verifies and is left
// where it is.
//
// AN UNPARSEABLE VERIFIER IS A REFUSAL AND NOT AN ERROR PATH THE CALLER
// BRANCHES ON: a row somebody corrupted must not be distinguishable, from
// outside, from a wrong password — so it spends the turn a decoy would, and
// answers no.
//
// UNDER THE SAME CAP AS A HASH ([VerifyCap]), in source's turn. A
// verification holds the stored verifier's memory cost for as long as it
// runs, and it is the half an UNAUTHENTICATED caller reaches — every sign-in,
// every step-up, every retry of a held redemption — so it is the half the cap
// exists for. It used to call argon2 directly: only setting a password
// queued, and the sign-in endpoint ran one 64 MiB derivation per concurrent
// request, however many arrived.
//
// The error is ctx's when the request went away before its turn came: nothing
// was derived and nothing was decided, and the caller answers as for any
// attempt that reached no verdict.
func (h *Hasher) Verify(ctx context.Context, source, verifier, password string) (
	ok bool, rehash bool, err error) {

	params, salt, want, err := decode(verifier)
	if err != nil {
		return false, false, h.Decoy(ctx, source, password)
	}
	got, err := h.derive(ctx, source, password, salt, params)
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

// Decoy spends the turn a verification would have, in source's lane, for a
// subject that has no verifier to check — nobody by that name, a person who
// may not act, a person with no password.
//
// # It takes a turn, and holds its slot as long as a derivation takes
//
// The sign-in pad makes the two arms answer at one deadline, and that holds
// only while a verification finishes inside it. A decoy that queued for
// nothing let one address separate the arms below the pad by itself: fire a
// handful of attempts at once, and the real names queue behind each other in
// the address's lane and answer late while the decoys answer on time — the
// roster, read off the order the answers came back in. So a decoy waits for
// its turn in the same lane and holds a slot of the cap for [Hasher]'s
// measured derivation time, and an address's answers come back in the same
// rhythm whichever of its names exist.
//
// # It derives nothing, once it knows how long deriving takes
//
// Holding the slot costs no memory and no CPU. Until this hasher has run a
// derivation there is no measure to hold for, so that decoy derives once, at
// this hasher's own cost, and the measure is taken from it. What that costs a
// stranger with no real name to try is one derivation per process, and past it
// no more than a real name costs: one slot, in their own turn.
//
// ITS RESULT IS DISCARDED BY CONSTRUCTION — it returns nothing a caller could
// branch on, because a decoy whose answer could be read would be a second
// oracle. What it produces is TIME. The error is ctx's when the request went
// away before its turn came.
func (h *Hasher) Decoy(ctx context.Context, source, presented string) error {
	release, err := h.turns.take(ctx, sourceKeyOf(source))
	if err != nil {
		return err
	}
	defer release()
	if took := time.Duration(h.took.Load()); took > 0 {
		h.hold(ctx, took)
		return nil
	}
	salt := make([]byte, SaltLen)
	// crypto/rand does not fail: it aborts the process rather than return
	// an error, so there is nothing here to handle.
	_, _ = rand.Read(salt)
	h.timed(presented, salt, h.params)
	return nil
}

// weakerThan reports whether p is a WEAKER cost than q: below it in at least
// one parameter an attacker pays for, and above it in none.
//
// A PARTIAL ORDER, deliberately, and not a product or any other single number
// of cost: a rewrite that lowers ANY parameter is a downgrade in that
// parameter, and a cost model deciding it was paid for elsewhere is exactly
// the judgement argon2's own trade-offs make unreliable — OWASP's settings of
// equal defence are not one memory-time product, because one pass buys less
// per KiB than three. A verifier whose cost is neither weaker nor stronger
// than a build's is left where it is by that build — and the order being
// antisymmetric is what makes the rule safe mid-rollout: no two builds can
// each find the other's verifier weaker, so nothing flaps.
//
// [Params.Threads] IS NOT A COST and decides nothing: parallelism changes how
// soon one verification finishes, not what an attacker pays (see [Threads]),
// so a verifier that differs only in it is neither weaker nor stronger, and
// two builds disagreeing about it would otherwise rewrite each person's on
// every sign-in for nothing.
func (p Params) weakerThan(q Params) bool {
	if p.Memory > q.Memory || p.Time > q.Time || p.KeyLen > q.KeyLen {
		return false
	}
	return p.Memory < q.Memory || p.Time < q.Time || p.KeyLen < q.KeyLen
}

// Rehash produces a verifier for a password at this hasher's cost, as [Hash]
// does — but only if a slot of the concurrency cap is free NOW, and answers
// [ErrSaturated] without deriving anything when none is.
//
// # It exists for the ONE hash nobody is waiting on
//
// Raising the cost is carried out by the sign-in that presents the password
// ([Hasher.Verify]'s rehash flag), after that sign-in has answered: the
// rewrite is an opportunity, and nobody's request waits on it. So it never
// QUEUES for the cap, which is full exactly when the endpoint is under the
// load the cap exists for — a rewrite waiting in that queue would take a slot
// ahead of the sign-ins arriving behind it, and turn an attack on somebody
// else into a slow sign-in for everybody. A miss is harmless: the verifier
// stays verifiable at its own cost, and the next sign-in asks again.
//
// NO CONTEXT, because there is nothing to wait for: a slot is taken at once or
// not at all, and a slot once taken runs to the end — argon2 is not
// interruptible, and abandoning a derivation already paid for would spend the
// cost and keep nothing.
//
// IN NOBODY'S LANE: the sign-in that asked for it has already had its turn and
// answered, so it takes a slot only while no source is waiting for one.
func (h *Hasher) Rehash(password string) (string, error) {
	release, ok := h.turns.tryTake()
	if !ok {
		return "", ErrSaturated
	}
	defer release()
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("credential: read a salt: %w", err)
	}
	return h.encode(salt, h.timed(password, salt, h.params)), nil
}

// ErrSaturated reports a [Hasher.Rehash] that found every slot of the
// concurrency cap taken and derived nothing.
var ErrSaturated = errors.New("credential: every derivation slot is taken, " +
	"and a rewrite nobody is waiting on does not queue for one")

// derive runs the cost at params in source's turn: this hasher's own cost for
// a new verifier, the stored verifier's for a verification. With
// [Hasher.Rehash] and [Hasher.Decoy] it is the only caller of [Hasher.timed],
// so no derivation this package runs can skip the cap.
//
// IT WAITS FOR ITS TURN FOR AS LONG AS ITS REQUEST DOES: a sign-in VERIFYING
// is the caller the cap queues rather than refuses, and one refused would
// answer as a wrong password — but a request that went away gives up its
// place, and nothing is derived for nobody.
func (h *Hasher) derive(ctx context.Context, source, password string, salt []byte,
	params Params) ([]byte, error) {

	// THE TURN IS TAKEN AROUND THE DERIVATION AND NOTHING ELSE. Holding it
	// across a store read as well would make one slow database turn the
	// password cost into a queue, which is the shape that takes a node
	// down under exactly the load the cap exists for.
	release, err := h.turns.take(ctx, sourceKeyOf(source))
	if err != nil {
		return nil, err
	}
	defer release()
	return h.timed(password, salt, params), nil
}

// timed runs one derivation and folds how long it took into [Hasher]'s
// measure: an eighth of the way towards each new one, so the measure follows
// the load the node is under without one slow derivation moving it far.
// Called only with a slot of the cap held.
func (h *Hasher) timed(password string, salt []byte, params Params) []byte {
	start := time.Now()
	digest := h.work(password, salt, params)
	took := int64(time.Since(start))
	for {
		was := h.took.Load()
		next := took
		if was > 0 {
			next = was + (took-was)/8
		}
		if h.took.CompareAndSwap(was, next) {
			return digest
		}
	}
}

// argon2id is the one derivation, and it is called only with a slot of the
// cap held — through [Hasher.timed].
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
		threads <= 0 || threads > 255 {
		return Params{}, nil, nil, errVerifier
	}
	params.Threads = uint8(threads)
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errVerifier
	}
	digest, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(digest) == 0 {
		return Params{}, nil, nil, errVerifier
	}
	params.KeyLen = uint32(len(digest))
	return params, salt, digest, nil
}
