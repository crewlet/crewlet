package credential_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/credential"
)

// A SET IS MINTED WHOLE, SHOWN ONCE, AND EACH CODE SPENT ONCE.
func TestARecoverySetIsTenDistinctSingleUseCodes(t *testing.T) {
	t.Parallel()
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(codes) != credential.RecoveryCodeCount ||
		len(verifiers) != credential.RecoveryCodeCount {
		t.Fatalf("minted %d codes and %d verifiers, want %d of each",
			len(codes), len(verifiers), credential.RecoveryCodeCount)
	}
	seen := map[string]bool{}
	for i, code := range codes {
		if seen[code] {
			t.Errorf("code %d repeats an earlier one", i)
		}
		seen[code] = true
		// THE CODE IS NOT THE VERIFIER, which is the whole reason the
		// two are returned together: a caller that stored the wrong one
		// would be storing a live credential in a replicated table.
		if verifiers[i] == code {
			t.Fatal("the verifier IS the code")
		}
		if strings.Contains(verifiers[i], strings.ReplaceAll(code, "-", "")) {
			t.Fatal("the verifier contains the code")
		}
		if at := credential.SpendRecoveryCode(verifiers, code); at != i {
			t.Errorf("code %d matched verifier %d", i, at)
		}
	}
	if at := credential.SpendRecoveryCode(verifiers, "AAAA-BBBB-CCCC-DDDD-EEEE-FF"); at != -1 {
		t.Errorf("a code nobody minted matched verifier %d", at)
	}
}

// A CODE IS ACCEPTED IN THE FORM SOMEBODY READS IT OFF PAPER.
//
// The grouping dashes are there so a twenty-six character string can be typed
// without losing your place; refusing the code back without them, or in lower
// case, or with a trailing space from a paste, is a lockout produced by
// formatting — on the credential whose entire purpose is being the way back in.
func TestARecoveryCodeIsAcceptedHoweverItIsTypedBack(t *testing.T) {
	t.Parallel()
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	code := codes[3]
	for name, typed := range map[string]string{
		"as given":       code,
		"lower case":     strings.ToLower(code),
		"without dashes": strings.ReplaceAll(code, "-", ""),
		"with spaces":    strings.ReplaceAll(code, "-", " "),
		"pasted padded":  "  " + code + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if at := credential.SpendRecoveryCode(verifiers, typed); at != 3 {
				t.Errorf("%q matched verifier %d, want 3", typed, at)
			}
		})
	}
}

// A SPENT CODE IS THE CALLER'S TO REMOVE, AND REMOVING IT IS WHAT SPENDS IT.
//
// Nothing here mutates the set: a spend that happened in memory would have
// happened on one node, and the other nodes would go on accepting the code.
func TestSpendingIsTheCallersAndTheCodeThenStopsMatching(t *testing.T) {
	t.Parallel()
	codes, verifiers, err := credential.NewRecoveryCodes()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	at := credential.SpendRecoveryCode(verifiers, codes[0])
	if at != 0 {
		t.Fatalf("the first code matched %d", at)
	}
	// The function did NOT change the set — the caller publishes the
	// record that does.
	if credential.SpendRecoveryCode(verifiers, codes[0]) != 0 {
		t.Error("the set changed under the caller, so the spend happened in " +
			"memory on one node")
	}
	remaining := append(append([]string{}, verifiers[:at]...), verifiers[at+1:]...)
	if again := credential.SpendRecoveryCode(remaining, codes[0]); again != -1 {
		t.Errorf("a spent code still matched verifier %d", again)
	}
	if other := credential.SpendRecoveryCode(remaining, codes[1]); other != 0 {
		t.Errorf("the next code matched %d after one was removed", other)
	}
}
