package credential_test

import (
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// THE COST PARAMETERS ARE PINNED BY VALUE, AND EACH ASSERTION NAMES WHAT THE
// VALUE BUYS.
//
// A cost that drifts DOWN is the one change in this package that is completely
// invisible: every test still passes, every login still works, and the only
// thing that moved is how long an offline attack against a stolen database
// takes. Nothing else would notice, so this does.
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
	if credential.MinPasswordChars != 12 {
		t.Errorf("the password floor is %d characters, want 12",
			credential.MinPasswordChars)
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
	verifier, err := h.Hash("a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if ok, rehash := h.Verify(verifier, "a-long-enough-password"); !ok || rehash {
		t.Errorf("the password verified %v and asked for a rehash %v under the "+
			"cost it was written at", ok, rehash)
	}
	if ok, _ := h.Verify(verifier, "a-long-enough-passwore"); ok {
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
	verifier, err := weak.Hash("a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	raised := testParams()
	raised.Time = 2
	strong := credential.NewHasher(raised, 2)

	ok, rehash := strong.Verify(verifier, "a-long-enough-password")
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
	fresh, err := strong.Hash("a-long-enough-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, rehash := strong.Verify(fresh, "a-long-enough-password"); rehash {
		t.Error("a verifier at the current cost was reported stale")
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
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if ok, rehash := h.Verify(verifier, "a-long-enough-password"); ok || rehash {
				t.Errorf("%q verified %v / rehash %v", verifier, ok, rehash)
			}
		})
	}
}
