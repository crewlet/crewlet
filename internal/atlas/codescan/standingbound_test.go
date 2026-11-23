package codescan_test

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/codescan"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
)

// countedScans is the memory coordination store, which holds every code scan
// standing to coord.SmallValueBytes as the KV store does, recording what was
// written and counting the listings and the key reads.
type countedScans struct {
	*memory.Fleet
	mu      sync.Mutex
	written map[string]int
	lists   int
	gets    int
}

func newCountedScans() *countedScans {
	return &countedScans{Fleet: memory.NewFleet(), written: map[string]int{}}
}

func (s *countedScans) AtlasCodeScans(ctx context.Context, host string) (map[string][]byte, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Fleet.AtlasCodeScans(ctx, host)
}

func (s *countedScans) AtlasCodeScan(ctx context.Context, host, repo string) ([]byte, bool, error) {
	s.mu.Lock()
	s.gets++
	s.mu.Unlock()
	return s.Fleet.AtlasCodeScan(ctx, host, repo)
}

func (s *countedScans) PutAtlasCodeScan(ctx context.Context, host, repo string, value []byte) error {
	s.mu.Lock()
	s.written[coord.AtlasCodeScanKey(host, repo)] = len(value)
	s.mu.Unlock()
	return s.Fleet.PutAtlasCodeScan(ctx, host, repo, value)
}

func (s *countedScans) counts() (lists, gets int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lists, s.gets
}

// THE WIDEST STANDING OF EACH KIND FITS coord.SmallValueBytes: a forge
// repository's under a voice at an atlas key's 512 bytes with its stamp's
// digest, a Gitlet repository's under a name at its bound, and a forge's own,
// each with an error at its bound of text JSON escapes six bytes a byte and
// every count at an int64's 19 digits.
//
// Mutation: keep the forge's stamp as it was listed, and a long branch's
// name is kept whole, no digest bounding it.
func TestTheWidestCodeScanStandingFitsItsBound(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 23, 12, 34, 56, 123456789, time.UTC)
	most := math.MaxInt64
	fault := strings.Repeat("<", collect.MaxErrorLength)
	repo := func(id, name string) codescan.Status {
		return codescan.Status{Repo: id, Name: name, Tip: strings.Repeat("f", 64), Rules: most, TooLarge: true,
			LastAttemptAt: at, Completed: true, LastCompleteAt: at, Listed: most, Unlisted: most, Calls: most,
			Reads: most, Bytes: most, Pending: most, Oversize: most, Unparsed: most, Withheld: most, Truncated: most,
			Entries: most, Recorded: most, Purged: most, LastError: fault}
	}
	scans := newCountedScans()
	forges, err := codescan.NewForgeStore(scans)
	if err != nil {
		t.Fatal(err)
	}
	gitletStore, err := codescan.NewCoordStore(scans)
	if err != nil {
		t.Fatal(err)
	}

	// A FORGE REPOSITORY'S, its stamp the one a pass keeps for a listing
	// naming a branch of 255 escaped bytes.
	voice := strings.Repeat("v", atlas.MaxKeyBytes)
	api := &forgeRepo{key: forgeAPI, tip: tip1, stamp: "2026-11-23T12:34:56Z " + strings.Repeat("<", 255),
		files: map[string]string{}}
	rig := newForgeRig(t, api)
	rig.pass()
	_, kept := rig.standing()
	st := repo(voice, voice)
	st.Stamp = kept[forgeVoice].Stamp
	if len(st.Stamp) != 64 {
		t.Fatalf("a pass kept a stamp of %d bytes, want its 64-byte digest", len(st.Stamp))
	}
	if err := forges.Save(t.Context(), integration.KindGitHub, st); err != nil {
		t.Errorf("the widest forge repository's standing was refused: %v", err)
	}
	// A GITLET REPOSITORY'S, by its id under a name at its bound.
	if err := gitletStore.Save(t.Context(), repo(shopID, strings.Repeat("n", gitref.MaxRepoNameBytes))); err != nil {
		t.Errorf("the widest Gitlet repository's standing was refused: %v", err)
	}
	// A FORGE'S OWN.
	if err := forges.SaveForge(t.Context(), codescan.ForgeStatus{Kind: integration.KindGoogleAnalytics,
		LastAttemptAt: at, Completed: true, LastCompleteAt: at, Repositories: most, Whole: true, Mirrored: most,
		Refused: most, Unnamed: most, TooLarge: most, Waiting: most, Calls: most, Trees: most, Reads: most,
		Bytes: most, Released: most, LastError: fault}); err != nil {
		t.Errorf("the widest forge's standing was refused: %v", err)
	}
	// THE FIGURES encode STATES.
	for key, want := range map[string]int{
		coord.AtlasCodeScanKey("github", voice):                             13_763,
		coord.AtlasCodeScanKey(coord.AtlasCodeGitlet, shopID):               12_763,
		coord.AtlasCodeScanKey(string(integration.KindGoogleAnalytics), ""): 12_507,
	} {
		if got := scans.written[key]; got != want {
			t.Errorf("the widest standing under %.40s is %d bytes, want %d", key, got, want)
		}
	}
}

// A FORGE'S STAMP IS KEPT AS ITS DIGEST, and still answers whether a
// repository moved: a pass over an unchanged stamp asks the forge nothing, and
// a moved stamp is asked about.
//
// Mutation: compare the standing's stamp with the listing's own, and the
// unchanged stamp's pass asks for the tip again.
func TestAForgesStampIsKeptAsItsDigest(t *testing.T) {
	t.Parallel()
	api := apiRepo()
	api.stamp = "2026-11-23T12:34:56Z " + strings.Repeat("<", 255)
	r := newForgeRig(t, api)
	r.pass()
	r.forge.asked()
	if _, repos := r.standing(); len(repos[forgeVoice].Stamp) != 64 ||
		strings.Contains(repos[forgeVoice].Stamp, "<") {
		t.Fatalf("the standing keeps the stamp %q, want its digest", repos[forgeVoice].Stamp)
	}
	r.now = r.now.Add(time.Hour)
	r.pass()
	if tips, _, _ := r.forge.asked(); len(tips) != 0 {
		t.Errorf("an unchanged stamp's pass asked for tips %v, want none", tips)
	}
	api.stamp += "!"
	r.now = r.now.Add(time.Hour)
	r.pass()
	if tips, _, _ := r.forge.asked(); len(tips) != 1 {
		t.Errorf("a moved stamp's pass asked for tips %v, want its one", tips)
	}
}

// A REPOSITORY'S PASS READS ITS OWN STANDING BY ITS KEY, AND THE LISTING
// READS EVERY STANDING ONCE A PASS: Work lists no other repository's, and Due
// lists them on its first page alone, however many pages the listing has.
//
// Mutation: have Work list every standing, or Due list them on every page,
// and the listings are counted.
func TestAPassReadsItsOwnStandingAndTheListingReadsThemOnce(t *testing.T) {
	t.Parallel()
	scans := newCountedScans()
	other := shop()
	other.id, other.name = adminID, "admin"
	r := newRigOn(t, scans, shop(), other)
	r.code.page = 1
	due := r.due()
	if lists, gets := scans.counts(); len(due) != 2 || lists != 1 || gets != 0 {
		t.Fatalf("a listing of two pages owes %v after %d listings and %d key reads, want both after one listing",
			due, lists, gets)
	}
	for _, id := range due {
		if err := r.scanner.Work(t.Context(), id, statelog.Position{}); err != nil {
			t.Fatal(err)
		}
	}
	if lists, gets := scans.counts(); lists != 1 || gets != 2 {
		t.Errorf("two repositories' passes made %d listings and %d key reads, want none and one each",
			lists-1, gets)
	}
}
