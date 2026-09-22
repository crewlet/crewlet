package credential_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
)

// LENGTH IS THE ONLY RULE, AND IT IS THE ONE THAT DOES THE WORK.
//
// No required digit, no required symbol, no forbidden repeat. Composition
// rules shrink the set people actually choose from, are enumerable by an
// attacker who knows the rule, and push people to write the result down.
func TestTwelveCharactersIsTheOnlyRule(t *testing.T) {
	t.Parallel()
	for name, password := range map[string]string{
		"all lower case, no digits":  "thepurplecarrotwalks",
		"a passphrase with spaces":   "four words with spaces",
		"repeated characters inside": "aardvarrrrrkkkkky",
		"no symbols at all":          "twelvelettersx",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := credential.CheckStrength(password); err != nil {
				t.Errorf("%q was refused: %v — the only rule is length, and a "+
					"composition rule shrinks what people choose from",
					password, err)
			}
		})
	}
	if err := credential.CheckStrength("elevenchars"); !errors.Is(err, credential.ErrWeak) {
		t.Errorf("an eleven-character password was accepted: %v", err)
	}
	// AND THE REFUSAL SAYS WHAT IS WRONG, unlike every other refusal in
	// this package: it is answered to somebody who has already proved who
	// they are and is choosing a new secret.
	err := credential.CheckStrength("short")
	if err == nil || !strings.Contains(err.Error(), "12") {
		t.Errorf("the refusal %v does not name the minimum, so somebody "+
			"choosing a password cannot tell what would satisfy it", err)
	}
}

// THE FLOOR IS IN CHARACTERS, NOT BYTES.
//
// A byte count quietly passes a twelve-byte value that is four characters of
// UTF-8 — which is the same failure the shared-token floor names, one field
// over.
func TestTheFloorCountsCharactersAndNotBytes(t *testing.T) {
	t.Parallel()
	// Four characters, sixteen bytes.
	short := strings.Repeat("😀", 4)
	if len(short) < iam.MinPasswordChars {
		t.Fatalf("this case is not testing anything: %q is %d bytes", short, len(short))
	}
	if err := credential.CheckStrength(short); !errors.Is(err, credential.ErrWeak) {
		t.Errorf("%q (%d bytes, 4 characters) was accepted: %v", short,
			len(short), err)
	}
}

// THE BLOCKLIST CATCHES A FAMILY, NOT A SPELLING.
//
// This is what makes a list of hundreds worth one of thousands: an entry is
// compared in folded form, so every substitution somebody makes to satisfy a
// composition rule lands on the same string.
func TestOneBlocklistEntryCatchesEverySpellingOfItsFamily(t *testing.T) {
	t.Parallel()
	for _, password := range []string{
		"password1234", "Password1234", "P@ssw0rd1234", "p4$$w0rd1234",
		"  password1234  ", "PASSWORD1234",
	} {
		if err := credential.CheckStrength(password); !errors.Is(err, credential.ErrWeak) {
			t.Errorf("%q was accepted — the fold is what lets one entry stand "+
				"for every spelling of its family", password)
		}
	}
	// And the families themselves.
	for name, password := range map[string]string{
		"a keyboard walk":   "1qaz2wsx3edc",
		"a repeated block":  "123123123123",
		"a run of digits":   "123456789012",
		"a famous phrase":   "correcthorsebatterystaple",
		"a word doubled up": "passwordpassword",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := credential.CheckStrength(password); !errors.Is(err, credential.ErrWeak) {
				t.Errorf("%q was accepted", password)
			}
		})
	}
}

// AND THE FOLD DOES NOT REFUSE PASSWORDS THAT ARE MERELY LONG.
//
// The net is deliberately wide, and wide is only free while it stays on the
// far side of anything anybody would actually choose. A fold that collapsed
// too much would start refusing good passwords, which is the failure an
// operator experiences as "it will not take anything I type".
func TestTheFoldDoesNotRefuseAnOrdinaryPassword(t *testing.T) {
	t.Parallel()
	for _, password := range []string{
		"correct-horse-staples", "my-first-bicycle-1987", "Tr0ub4dor&3xkcd!!",
		"the rain in spain falls", "quartz-ledger-inbound-9",
	} {
		if err := credential.CheckStrength(password); err != nil {
			t.Errorf("%q was refused: %v", password, err)
		}
	}
}

// EVERY ENTRY IN THE FILE IS A LINE THAT CAN ACTUALLY MATCH SOMETHING.
//
// Three ways a line can look like it is doing something and not be, and every
// one of them is invisible in a data file nothing reads back:
//
//   - BELOW THE FLOOR: the length rule refuses the string first, so the entry
//     can never be reached;
//   - NOT LOWER CASE: the fold lower-cases before comparing, so the capitals
//     are decoration;
//   - A FOLD DUPLICATE: two lines that fold to one key, which is what an entry
//     written in leetspeak always is — `p4ssw0rd1234` is `password1234`
//     wearing a different face, and adds nothing.
//
// A digit RUN is not a fold duplicate of anything and stays written in digits:
// the fold maps digits to letters, so `123123123123` folds to a string nobody
// types, and that is fine — what matters is that the entry and the candidate
// fold the SAME WAY, not that the fold is readable.
func TestEveryBlocklistEntryCanActuallyBeReached(t *testing.T) {
	t.Parallel()
	entries := credential.BlocklistEntries()
	if len(entries) < 200 {
		t.Fatalf("the blocklist holds %d entries, which is not the file this "+
			"test is reading", len(entries))
	}
	seen := map[string]string{}
	for _, entry := range entries {
		if n := len([]rune(entry)); n < iam.MinPasswordChars {
			t.Errorf("%q is %d characters, below the floor — the length rule "+
				"refuses it first, so this line can never match", entry, n)
		}
		if entry != strings.ToLower(entry) {
			t.Errorf("%q is not lower case; the fold lower-cases before "+
				"comparing, so the capitals are decoration", entry)
		}
		folded := credential.Fold(entry)
		if first, dup := seen[folded]; dup {
			t.Errorf("%q and %q both fold to %q, so one of them adds nothing "+
				"— which is what a leetspeak spelling of an entry already in "+
				"the file always is", first, entry, folded)
		}
		seen[folded] = entry
	}
	// AND THE FILE IS REACHED THROUGH THE EMBED rather than through a
	// path this test resolves for itself: a suite reading the file
	// directly would go on passing after the embed directive stopped
	// pointing at it, which is how a gate comes to certify nothing.
	if !credential.Blocked("password1234") {
		t.Error("the embedded list does not refuse the entry it most " +
			"obviously holds, so nothing this case walked is what the " +
			"engine actually screens against")
	}
}
