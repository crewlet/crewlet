package code_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
)

// seatBranchRun is the run id the table's run forms are cut from: a UUIDv7,
// its last twelve hex digits its random tail.
const seatBranchRun = "0190d8f0-1111-7000-8000-0123456789ab"

// digest12 is the suffix a respelled key takes: the first twelve hex digits of
// its SHA-256.
func digest12(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// THE SEAT BRANCH IS A BRANCH IN THE SEAT'S OWN NAMESPACE, one per work item
// and distinct per key: a key a ref takes as written is the branch's last
// component as written, case kept; a key that had to be respelled — or whose
// spelling already ends as a digest does — takes the digest of the key it was,
// so two keys that spell alike are two branches; one past the ref's room is cut
// before its digest to a ref of exactly 255 bytes; and a turn on no work item,
// or on one whose key spells to nothing, works on the run's own branch.
//
// Mutation: drop the digest of a respelled key → the two acme rows are one
// branch, red.
func TestTheSeatBranchIsABranchInTheSeatsNamespace(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("k", 300)
	for _, c := range []struct {
		name, handle, key, run, want string
	}{
		{"a tracker key as written", "swe", "ENG-412", seatBranchRun, "seats/swe/ENG-412"},
		{"case kept", "swe", "eng-412", seatBranchRun, "seats/swe/eng-412"},
		{"a key respelled takes its digest", "swe", "acme/api#88", seatBranchRun,
			"seats/swe/acme-api-88-" + digest12("acme/api#88")},
		{"the key it spells alike with takes none", "swe", "acme-api-88", seatBranchRun, "seats/swe/acme-api-88"},
		{"an unchanged key shaped like a digest takes one", "swe", "acme-api-88-0123456789ab", seatBranchRun,
			"seats/swe/acme-api-88-0123456789ab-" + digest12("acme-api-88-0123456789ab")},
		{"runs and ends of dashes are a change", "swe", "--a--b--", seatBranchRun,
			"seats/swe/a-b-" + digest12("--a--b--")},
		{"no key is the run's branch", "swe", "", seatBranchRun, "seats/swe/run-0123456789ab"},
		{"a key that spells to nothing is the run's branch", "swe", "###", seatBranchRun,
			"seats/swe/run-0123456789ab"},
		{"a key past the ref's room is cut before its digest", "swe", long, seatBranchRun,
			"seats/swe/" + long[:255-len("refs/heads/seats/swe/")-13] + "-" + digest12(long)},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := code.SeatBranch(c.handle, c.key, c.run)
			if err != nil {
				t.Fatalf("SeatBranch(%q, %q): %v", c.handle, c.key, err)
			}
			if got != c.want {
				t.Errorf("SeatBranch(%q, %q) = %q, want %q", c.handle, c.key, got, c.want)
			}
			if !gitref.ValidBranch(got) {
				t.Errorf("%q is not a branch git takes", got)
			}
			if !strings.HasPrefix("refs/heads/"+got, code.SeatNamespace(c.handle)) {
				t.Errorf("%q is outside the seat's namespace %s", got, code.SeatNamespace(c.handle))
			}
		})
	}
	if got, _ := code.SeatBranch("swe", long, seatBranchRun); len("refs/heads/"+got) != gitref.MaxRefNameBytes {
		t.Errorf("a 300-byte key's ref is %d bytes, want exactly %d", len("refs/heads/"+got),
			gitref.MaxRefNameBytes)
	}

	for _, c := range []struct{ name, handle, key, run string }{
		{"a handle past a seat's 64 bytes", strings.Repeat("h", 65), "ENG-1", seatBranchRun},
		{"a handle no seat has", "Not A Handle", "ENG-1", seatBranchRun},
		{"a run id that is no UUID, with no key", "swe", "", "run-1"},
	} {
		if _, err := code.SeatBranch(c.handle, c.key, c.run); !errors.Is(err, code.ErrInvalid) {
			t.Errorf("%s: SeatBranch answered %v, want ErrInvalid", c.name, err)
		} else if c.key != "" && !strings.Contains(err.Error(), c.handle) {
			t.Errorf("%s: the refusal does not name the handle: %v", c.name, err)
		}
	}
}

// DISTINCT KEYS ARE DISTINCT BRANCHES: 100 000 keys in the shapes trackers
// write them — PROJ-<n>, <owner>/<repo>#<n>, <group>/<proj>!<n>, their case
// variants and runs of punctuation — name 100 000 branches; and for every
// respelled key, the key that IS its spelling (the one a person could type to
// land on the same words) names another branch.
//
// Mutation: drop the digest-shape rule → a crafted key "x-<12 hex>" lands on
// the branch of the key whose digest those digits are, red.
func TestDistinctKeysAreDistinctBranches(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string, 100_000)
	note := func(key string) {
		branch, err := code.SeatBranch("swe", key, seatBranchRun)
		if err != nil {
			t.Fatalf("SeatBranch(%q): %v", key, err)
		}
		if other, taken := seen[branch]; taken && other != key {
			t.Fatalf("keys %q and %q are one branch, %q", other, key, branch)
		}
		seen[branch] = key
	}
	shapes := []func(n int) string{
		func(n int) string { return fmt.Sprintf("PROJ-%d", n) },
		func(n int) string { return fmt.Sprintf("proj-%d", n) },
		func(n int) string { return fmt.Sprintf("acme/api#%d", n) },
		func(n int) string { return fmt.Sprintf("acme-api-%d", n) },
		func(n int) string { return fmt.Sprintf("group/proj!%d", n) },
		func(n int) string { return fmt.Sprintf("Group/Proj!!%d", n) },
		func(n int) string { return fmt.Sprintf("a.b..c/%d", n) },
		func(n int) string { return fmt.Sprintf("a b  c %d", n) },
		func(n int) string { return fmt.Sprintf("--%d--", n) },
		func(n int) string { return fmt.Sprintf("x_%d", n) },
	}
	for i := 0; len(seen) < 100_000; i++ {
		key := shapes[i%len(shapes)](i / len(shapes))
		note(key)
		branch, _ := code.SeatBranch("swe", key, seatBranchRun)
		// THE CRAFTED KEY: the respelled branch's last component, typed as
		// a key of its own. It is shaped like a digest, so it must take one.
		if crafted := strings.TrimPrefix(branch, "seats/swe/"); crafted != key {
			note(crafted)
		}
	}
}

// FuzzSeatBranch holds the function's promises over any key: a branch git
// takes, inside the seat's namespace, and a pure function of its inputs.
func FuzzSeatBranch(f *testing.F) {
	for _, key := range []string{"ENG-412", "acme/api#88", "acme-api-88", "acme-api-88-0123456789ab", "", "###",
		strings.Repeat("k", 300), "a\x00b", "ü/é", "..", "@{", "a.lock", "-", "\n"} {
		f.Add(key)
	}
	f.Fuzz(func(t *testing.T, key string) {
		got, err := code.SeatBranch("swe", key, seatBranchRun)
		if err != nil {
			t.Fatalf("SeatBranch(%q): %v", key, err)
		}
		if !gitref.ValidBranch(got) || !strings.HasPrefix("refs/heads/"+got, code.SeatNamespace("swe")) {
			t.Fatalf("SeatBranch(%q) = %q, not a branch in the seat's namespace", key, got)
		}
		if again, _ := code.SeatBranch("swe", key, seatBranchRun); again != got {
			t.Fatalf("SeatBranch(%q) answered %q and then %q", key, got, again)
		}
	})
}
