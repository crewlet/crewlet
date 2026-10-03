package credential_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// THE COST PARAMETERS ARE PINNED BY VALUE, AND EACH ASSERTION NAMES WHAT THE
// VALUE BUYS.
//
// A cost that drifts DOWN is the one change in this package that is completely
// invisible: every test still passes, every login still works, and the only
// thing that moved is how long an offline attack against a stolen database
// takes. Nothing else would notice, so this does.
//
// Changing them here is changing them for the next build, and a change may
// only RAISE a parameter: a verifier is rewritten only at a cost that lowers
// none of its own ([TestAVerifierIsRewrittenOnlyUpNeverDown]), so a new cost
// that trades one parameter down for another up reaches nobody enrolled before
// it.
func TestTheCostParametersAreTheOnesThatShip(t *testing.T) {
	t.Parallel()
	if credential.Memory != 64*1024 {
		t.Errorf("the memory cost is %d KiB, want 65536 — memory is the one "+
			"parameter a GPU cannot buy its way around, because a card with "+
			"thousands of cores has nothing like thousands of times the "+
			"memory bandwidth", credential.Memory)
	}
	if credential.Time != 3 {
		t.Errorf("the time cost is %d, want 3 — it is the pairing OWASP "+
			"states for 64 MiB, and it trades linearly against login latency "+
			"where memory trades superlinearly against an attacker",
			credential.Time)
	}
	if credential.Threads != 1 {
		t.Errorf("the parallelism is %d, want 1. Parallelism makes ONE "+
			"verification finish sooner and does nothing against an attacker "+
			"who is already running every core they own on different "+
			"candidates — what it costs is the ability to serve concurrent "+
			"sign-ins at all", credential.Threads)
	}
	if credential.KeyLen != 32 || credential.SaltLen != 16 {
		t.Errorf("the digest is %d bytes and the salt %d, want 32 and 16",
			credential.KeyLen, credential.SaltLen)
	}
	if iam.MinPasswordChars != 12 {
		t.Errorf("the password floor is %d characters, want 12",
			iam.MinPasswordChars)
	}
}

// THE VERIFY CAP IS DERIVED FROM THE MACHINE, NOT CONFIGURED.
//
// Each verification holds the memory cost for its duration, so an uncapped
// endpoint is 64 MiB per concurrent unauthenticated request — a hundred in
// flight is 6.4 GiB, which is an out-of-memory kill rather than a slow login.
func TestTheVerifyCapIsDerivedAndNeverZero(t *testing.T) {
	t.Parallel()
	want := runtime.NumCPU() / int(credential.Threads)
	if want < 1 {
		want = 1
	}
	if got := credential.VerifyCap(); got != want {
		t.Errorf("VerifyCap() = %d, want %d", got, want)
	}
	if credential.VerifyCap() < 1 {
		t.Error("a cap of zero is a channel nothing can be sent on, which is " +
			"every sign-in blocking for ever")
	}
}

// testParams are a cost a test can afford. The SHIPPED cost is asserted above;
// exercising the arithmetic at 64 MiB per case would make this suite minutes
// long for a property that does not depend on the numbers.
func testParams() credential.Params {
	return credential.Params{Memory: 8 * 1024, Time: 1, Threads: 1, KeyLen: 32}
}

// A PASSWORD ROUND-TRIPS, AND A WRONG ONE DOES NOT.
func TestAPasswordVerifiesAndAWrongOneDoesNot(t *testing.T) {
	t.Parallel()
	h := credential.NewHasher(testParams(), 2)
	verifier, err := h.Hash(t.Context(), "a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, rehash, err := h.Verify(t.Context(), verifier, "a-long-enough-password"); !ok ||
		rehash || err != nil {
		t.Errorf("the password verified %v and asked for a rehash %v under the "+
			"cost it was written at (%v)", ok, rehash, err)
	}
	if ok, _, _ := h.Verify(t.Context(), verifier, "a-long-enough-passwore"); ok {
		t.Error("a password one character out verified")
	}
	// The verifier is a PHC string, which is what lets an operator take
	// their digests to another implementation and bring them back.
	if !strings.HasPrefix(verifier, "$argon2id$v=19$m=8192,t=1,p=1$") {
		t.Errorf("the verifier is %q, which is not the PHC form every other "+
			"argon2 implementation reads", verifier)
	}
}

// RAISING THE COST REHASHES ON THE NEXT SUCCESSFUL LOGIN.
//
// THE ONLY INSTANT A STRONGER DIGEST CAN BE COMPUTED is the one where somebody
// presents their password, because the plaintext is not stored. Without the
// parameters in the verifier there is no way to tell a digest written at the
// old cost from one written at the new, and a cost raise would apply to
// nobody who already had an account.
func TestRaisingTheCostAsksForARehashOnTheNextLogin(t *testing.T) {
	t.Parallel()
	weak := credential.NewHasher(testParams(), 2)
	verifier, err := weak.Hash(t.Context(), "a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	raised := testParams()
	raised.Time = 2
	strong := credential.NewHasher(raised, 2)

	ok, rehash, err := strong.Verify(t.Context(), verifier, "a-long-enough-password")
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatal("a verifier written at the old cost stopped verifying when " +
			"the cost was raised, which is every person in the company " +
			"locked out by a configuration change")
	}
	if !rehash {
		t.Error("the verifier was not reported stale, so the raised cost " +
			"would apply to nobody who already had an account")
	}
	// And a verifier written at the CURRENT cost is not reported stale,
	// or every login would rewrite a row for nothing.
	fresh, err := strong.Hash(t.Context(), "a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, rehash, _ := strong.Verify(t.Context(), fresh, "a-long-enough-password"); rehash {
		t.Error("a verifier at the current cost was reported stale")
	}
}

// A VERIFIER IS REWRITTEN ONLY UP, NEVER DOWN.
//
// The cost moves only with a build, so it moves during a ROLLING UPGRADE: two
// builds share one identity estate, and a person signs in on whichever node
// the balancer picks. Reported stale whenever its parameters merely DIFFERED,
// a verifier the newer build had written at its raised cost was read as stale
// by a node still on the older build, which rewrote it at its own weaker cost
// — and an upgraded node then rewrote it back up, so every person's verifier
// flapped for the whole rollout and spent part of it at the cost being
// retired. Nothing else would ever notice: every sign-in still works.
//
// So stale is WEAKER — below this hasher's cost in some parameter an attacker
// pays for and above it in none — and every other verifier verifies and is
// left alone. Parallelism is not such a parameter. The weaker rows are the
// controls: they are what a cost raise must still reach. Mutation: compare the
// parameters with `!=` again and every row that is higher somewhere goes red.
func TestAVerifierIsRewrittenOnlyUpNeverDown(t *testing.T) {
	t.Parallel()
	// A COST WITH ROOM BELOW IT IN EVERY PARAMETER, and cheap enough to
	// hash ten times: what is asserted is the order, not the numbers.
	current := credential.Params{Memory: 64, Time: 2, Threads: 1, KeyLen: 32}
	with := func(change func(*credential.Params)) credential.Params {
		p := current
		change(&p)
		return p
	}
	for _, c := range []struct {
		name   string
		stored credential.Params
		stale  bool
	}{
		{"the same cost", current, false},

		// WEAKER: a cost raise reaches these.
		{"less memory", with(func(p *credential.Params) { p.Memory /= 2 }), true},
		{"fewer passes", with(func(p *credential.Params) { p.Time-- }), true},
		{"a shorter digest", with(func(p *credential.Params) { p.KeyLen = 16 }), true},

		// STRONGER: a newer build's, read by an older one mid-rollout.
		{"more memory", with(func(p *credential.Params) { p.Memory *= 2 }), false},
		{"more passes", with(func(p *credential.Params) { p.Time++ }), false},
		{"a longer digest", with(func(p *credential.Params) { p.KeyLen = 64 }), false},

		// NEITHER: a rewrite would lower one parameter to raise another.
		{"more memory and fewer passes", with(func(p *credential.Params) {
			p.Memory *= 2
			p.Time--
		}), false},
		{"less memory and a longer digest", with(func(p *credential.Params) {
			p.Memory /= 2
			p.KeyLen = 64
		}), false},

		// NOT A COST: parallelism changes how soon one verification
		// finishes and nothing an attacker pays.
		{"other parallelism only", with(func(p *credential.Params) { p.Threads = 2 }), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			verifier, err := credential.NewHasher(c.stored, 1).Hash(t.Context(), "a-long-enough-password")
			if err != nil {
				t.Fatalf("hash: %v", err)
			}
			ok, stale, err := credential.NewHasher(current, 1).Verify(t.Context(),
				verifier, "a-long-enough-password")
			if err != nil {
				t.Fatalf("verify: %v", err)
			}
			if !ok {
				t.Fatal("a verifier at another cost stopped verifying — it " +
					"must verify at its OWN parameters, whatever this build's are")
			}
			if stale != c.stale {
				t.Errorf("a verifier at %+v through a hasher at %+v was "+
					"reported stale %v, want %v", c.stored, current, stale, c.stale)
			}
		})
	}
}

// A CORRUPTED VERIFIER IS A REFUSAL, NOT A DISTINGUISHABLE ERROR.
//
// A row somebody mangled must not be tellable, from outside, from a wrong
// password: the difference is the one signal an attacker probing a database
// they have partial write access to would want.
func TestAnUnreadableVerifierRefusesLikeAWrongPassword(t *testing.T) {
	t.Parallel()
	h := credential.NewHasher(testParams(), 2)
	for name, verifier := range map[string]string{
		"empty":            "",
		"not phc":          "deadbeef",
		"wrong algorithm":  "$argon2i$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$ZGlnZXN0",
		"wrong version":    "$argon2id$v=16$m=8192,t=1,p=1$c2FsdHNhbHQ$ZGlnZXN0",
		"no parameters":    "$argon2id$v=19$$c2FsdHNhbHQ$ZGlnZXN0",
		"bad base64":       "$argon2id$v=19$m=8192,t=1,p=1$!!!!$ZGlnZXN0",
		"empty digest":     "$argon2id$v=19$m=8192,t=1,p=1$c2FsdHNhbHQ$",
		"zero parallelism": "$argon2id$v=19$m=8192,t=1,p=0$c2FsdHNhbHQ$ZGlnZXN0",
		// argon2 PANICS at zero passes rather than refusing them, so a
		// verifier stating it must be unreadable before it is derived.
		"zero passes": "$argon2id$v=19$m=8192,t=0,p=1$c2FsdHNhbHQ$ZGlnZXN0",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if ok, rehash, err := h.Verify(t.Context(), verifier,
				"a-long-enough-password"); ok || rehash || err != nil {
				t.Errorf("%q verified %v / rehash %v (%v)", verifier, ok, rehash, err)
			}
		})
	}
}
