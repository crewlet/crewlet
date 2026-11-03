package config

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// THE CACHE'S DIRECTORY IS RELATIVE TO THE STORE, store.snapshot_dir's rule: a
// path resolved against the engine's working directory lands somewhere
// different in a container and in a shell.
func TestTheGitCacheDirIsRelativeToTheStore(t *testing.T) {
	t.Parallel()
	const store = "/var/lib/crewlet/crewlet.db"
	for _, tc := range []struct{ dir, want string }{
		{"", "/var/lib/crewlet/git"},
		{"cache", "/var/lib/crewlet/cache"},
		{"/var/cache/crewlet/../git", "/var/cache/git"},
		{"  ", "/var/lib/crewlet/git"},
	} {
		if got := (Git{CacheDir: tc.dir}).CacheDirFor(store); got != tc.want {
			t.Errorf("git.cache_dir %q resolves to %q, want %q", tc.dir, got, tc.want)
		}
	}
}

// THE CACHE'S BUDGET IS ITS SHARE OF THE ROOM IT CAN USE: an explicit budget
// whatever the volume, else half of the room on a volume the store, the
// snapshots or the broker share and seven eighths of one of its own — NEVER
// MORE, with no floor raising a small share past the room and no ceiling — and
// the room counts what the cache already holds, so a warm node's restart
// derives what its first boot did.
//
// Mutation: give a dedicated volume the shared half, and the dedicated rows go
// red; restore a floor above the share, and the small rooms' rows go red.
func TestTheCacheBudgetIsItsShareOfTheVolume(t *testing.T) {
	t.Parallel()
	const gib = int64(1) << 30
	for _, tc := range []struct {
		name      string
		explicit  int64
		room      int64
		dedicated bool
		want      int64
		derived   bool
	}{
		{"an explicit budget whatever the volume", 8 * gib, 100 * gib, true, 8 * gib, false},
		{"a shared volume's half", 0, 100 * gib, false, 50 * gib, true},
		{"a dedicated volume's seven eighths", 0, 100 * gib, true, 87*gib + gib/2, true},
		// NO FLOOR: the share of a small room is the share, never a budget the
		// volume does not have.
		{"a small shared room's half", 0, 2 * gib, false, gib, true},
		{"a small dedicated room's seven eighths", 0, 2 * gib, true, gib + 3*gib/4, true},
		{"a room the bounded tenants take whole", 0, 0, false, 0, true},
		{"a room the bounded tenants overcommit", 0, -3 * gib, true, 0, true},
		{"no ceiling on a large dedicated volume", 0, 2048 * gib, true, 1792 * gib, true},
		// THE RESTART, as arithmetic: 100 GiB free beside 25 GiB the cache
		// holds is 125 GiB the cache can use — never the 50 GiB half of the
		// free space alone would give it.
		{"a warm restart counts what the cache holds", 0, 125 * gib, false, 62*gib + gib/2, true},
		// EIGHTHS WITHOUT LOSS OR OVERFLOW: seven bytes' half rounds down, and
		// the largest room takes seven eighths of itself.
		{"a room of seven bytes", 0, 7, false, 3, true},
		{"the largest room", 0, math.MaxInt64, true, math.MaxInt64/8*7 + 6, true},
	} {
		got, derived := Git{CacheMaxBytes: tc.explicit}.CacheBudget(tc.room, tc.dedicated)
		if got != tc.want || derived != tc.derived {
			t.Errorf("%s: CacheBudget(%d, %t) = (%d, %t), want (%d, %t)", tc.name,
				tc.room, tc.dedicated, got, derived, tc.want, tc.derived)
		}
		if derived && tc.room > 0 && got > tc.room {
			t.Errorf("%s: derived %d from a room of %d", tc.name, got, tc.room)
		}
	}
}

// A CACHE DIRECTORY HOLDING THE STORE IS REFUSED AT VALIDATE: the cache
// removes what it finds under its tmp/, scratch/ and trash/ at every boot, so
// it is given a directory of its own before a node is started on one that
// holds the store, its snapshots or its replicated estate.
func TestAGitCacheDirMustNotHoldTheStore(t *testing.T) {
	t.Parallel()
	const store = "store:\n  path: /var/lib/crewlet/crewlet.db\n"
	for _, tc := range []struct {
		name, doc, says string
	}{
		{"the store's own directory", store + "git:\n  cache_dir: .\n", "the store"},
		{"the store's parent", store + "git:\n  cache_dir: ..\n", "the store"},
		{"the snapshots' directory", store + "git:\n  cache_dir: snapshots\n", "the snapshots"},
		{"an ancestor of an absolute snapshot directory",
			"store:\n  path: /var/lib/crewlet/crewlet.db\n  snapshot_dir: /srv/crewlet/snapshots\n" +
				"git:\n  cache_dir: /srv\n", "the snapshots"},
		{"the replicated estate's directory",
			"store:\n  path: /var/lib/crewlet/crewlet.db\n  replicated_path: /data/estate/estate.db\n" +
				"git:\n  cache_dir: /data\n", "the replicated estate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.doc, "git.cache_dir")
			if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "holds "+tc.says) {
				t.Fatalf("refused, but not as a directory holding %s: %v", tc.says, err)
			}
		})
	}
	for _, tc := range []struct{ name, doc string }{
		{"the default, beside the snapshots", store},
		{"a sibling of the snapshot directory", store + "git:\n  cache_dir: snapshots-git\n"},
		{"inside the store's directory", store + "git:\n  cache_dir: cache/git\n"},
		{"an absolute directory elsewhere", store + "git:\n  cache_dir: /srv/git-cache\n"},
		{"a directory sharing only a prefix of the store's name",
			"store:\n  path: /var/lib/crewlet/crewlet.db\ngit:\n  cache_dir: /var/lib/crew\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseRunnableBootstrap([]byte(tc.doc), EnvOnly()); err != nil {
				t.Fatalf("refused: %v", err)
			}
		})
	}
}

// EVERY GIT SETTING REFUSES A NEGATIVE, AND A VALUE PAST ITS BOUND, naming the
// field: running the default after being told -1 hides a configuration the
// operator believes is in effect.
func TestGitSettingsRefuseNegatives(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ doc, path string }{
		{"git:\n  cache_max_bytes: -1\n", "git.cache_max_bytes"},
		{"git:\n  cache_max_bytes: 1048576\n", "git.cache_max_bytes"},
		{"git:\n  cache_max_bytes: 140737488355328\n", "git.cache_max_bytes"},
		{"git:\n  max_processes: -1\n", "git.max_processes"},
		{"git:\n  max_processes: 1025\n", "git.max_processes"},
		{"git:\n  max_transfers: -1\n", "git.max_transfers"},
		{"git:\n  max_transfers: 65537\n", "git.max_transfers"},
		{"git:\n  max_checks: -1\n", "git.max_checks"},
		{"git:\n  max_checks: 1025\n", "git.max_checks"},
	} {
		t.Run(tc.path+"/"+strings.TrimSpace(strings.SplitN(tc.doc, ": ", 2)[1]), func(t *testing.T) {
			t.Parallel()
			if err := rejectsBootstrap(t, tc.doc, tc.path); !errors.Is(err, ErrOutOfRange) {
				t.Fatalf("refused, but not as out of range: %v", err)
			}
		})
	}
	ok := "git:\n  cache_max_bytes: 1073741824\n  max_processes: 1024\n  max_transfers: 65536\n  max_checks: 1024\n"
	if _, err := ParseRunnableBootstrap([]byte(ok), EnvOnly()); err != nil {
		t.Fatalf("the bounds themselves are refused: %v", err)
	}
}

// A NODE'S CONTAINER CHECKS ARE A QUARTER OF ITS CORES UNLESS IT SAYS: unset
// git.max_checks is one for every four cores and at least one, a value set is
// taken as written, and 0 — a setting of its own, never "unset" — runs none.
//
// Mutation: read 0 as unset (a plain int), and the node told to run no
// container check runs its quarter.
func TestMaxChecksIsAQuarterOfTheCoresUnlessSet(t *testing.T) {
	t.Parallel()
	parse := func(t *testing.T, doc string) Git {
		t.Helper()
		b, err := ParseRunnableBootstrap([]byte(doc), EnvOnly())
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		return b.Git
	}
	unset := parse(t, "git:\n  max_processes: 2\n")
	for cores, want := range map[int]int{1: 1, 2: 1, 4: 1, 7: 1, 8: 2, 16: 4, 64: 16} {
		if got := unset.MaxChecksFor(cores); got != want {
			t.Errorf("unset on %d cores: %d checks, want %d", cores, got, want)
		}
	}
	if got := parse(t, "git:\n  max_checks: 0\n").MaxChecksFor(64); got != 0 {
		t.Errorf("max_checks: 0 on 64 cores runs %d container checks, want none", got)
	}
	if got := parse(t, "git:\n  max_checks: 3\n").MaxChecksFor(64); got != 3 {
		t.Errorf("max_checks: 3 runs %d, want 3", got)
	}
	if parse(t, "git:\n  max_checks: 0\n").IsZero() {
		t.Error("a block setting max_checks: 0 reads as one that sets nothing, and drops out of its export")
	}
}

// A GIT BLOCK ON A NODE THAT SERVES NO GIT IS SAID: nothing reads it, and an
// operator who sized it believes this node serves code.
func TestAGitBlockOnANodeThatServesNoGitWarns(t *testing.T) {
	t.Parallel()
	gitWarnings := func(t *testing.T, doc string) []Warning {
		t.Helper()
		b, err := ParseRunnableBootstrap([]byte(doc), EnvOnly())
		if err != nil {
			t.Fatalf("refused: %v", err)
		}
		var out []Warning
		for _, w := range b.Warnings() {
			if w.Path == "git" {
				out = append(out, w)
			}
		}
		return out
	}
	block := "git:\n  cache_dir: /srv/git\n"
	for _, b := range []string{block, "git:\n  import_allow_private: true\n"} {
		if got := gitWarnings(t, "node:\n  roles: [data, ingress]\n"+b); len(got) != 1 ||
			!strings.Contains(got[0].Message, "nothing reads it") {
			t.Errorf("%q on a [data, ingress] node: warnings %+v, want one saying nothing reads it", b, got)
		}
	}
	for name, doc := range map[string]string{
		"undeclared roles, which are every role": block,
		"a node naming git":                      "node:\n  roles: [data, git, ingress]\n" + block,
		"no git block":                           "node:\n  roles: [data, ingress]\n",
	} {
		if got := gitWarnings(t, doc); len(got) != 0 {
			t.Errorf("%s: warned %+v", name, got)
		}
	}
}

// AN IMPORT OR A MIRROR DIALS ONLY THE PUBLIC INTERNET UNLESS ITS NODE SAYS
// OTHERWISE: git.import_allow_private is off where the block does not name
// it, on where it does, and a block naming nothing else is still a block — it
// survives the JSON a node's Tier A is exported as, rather than dropping out
// as an unset block does.
//
// Mutation: leave the field out of Git.IsZero, and the export loses it.
func TestImportAllowPrivateIsOffUnlessTheNodeSetsIt(t *testing.T) {
	t.Parallel()
	b, err := ParseRunnableBootstrap([]byte("git:\n  max_processes: 2\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if b.Git.ImportAllowPrivate {
		t.Error("git.import_allow_private is on where nothing sets it")
	}
	b, err = ParseRunnableBootstrap([]byte("git:\n  import_allow_private: true\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if !b.Git.ImportAllowPrivate {
		t.Fatal("git.import_allow_private: true is read as off")
	}
	encoded, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Git struct {
			ImportAllowPrivate bool `json:"import_allow_private"`
		} `json:"git"`
	}
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Git.ImportAllowPrivate {
		t.Errorf("a git block naming only import_allow_private is exported without it:\n%s", encoded)
	}
}
