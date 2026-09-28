package topics_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY NAME THE PARTITION GRAMMAR PRODUCES IS PINNED.
//
// These strings are durable the moment a fleet creates one of the streams:
// the broker holds the stream under the name, the positions register and the
// applier's checkpoint name it, and a build that spelled it differently would
// create a second, empty stream beside the first and read a company with
// nothing in it. So a change here is a change a test has to be edited for.
func TestThePartitionGrammarsNamesArePinned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		layout                   int
		space                    string
		index                    int
		domain                   string
		stream, prefix, wildcard string
	}{
		{1, "tracker", 7, "tracker",
			"CREWLET_L1_TRACKER_007_TRACKER", "crewlet.l1.tracker.007.tracker", "crewlet.l1.tracker.007.tracker.>"},
		{1, "tracker", 7, "vectors",
			"CREWLET_L1_TRACKER_007_VECTORS", "crewlet.l1.tracker.007.vectors", "crewlet.l1.tracker.007.vectors.>"},
		{1, "pages", 3, "pages",
			"CREWLET_L1_PAGES_003_PAGES", "crewlet.l1.pages.003.pages", "crewlet.l1.pages.003.pages.>"},
		{1, "company", 0, "tracker",
			"CREWLET_L1_COMPANY_000_TRACKER", "crewlet.l1.company.000.tracker", "crewlet.l1.company.000.tracker.>"},
		{12, "tracker", 255, "vectors",
			"CREWLET_L12_TRACKER_255_VECTORS", "crewlet.l12.tracker.255.vectors", "crewlet.l12.tracker.255.vectors.>"},
		{2, "tracker", topics.MaxPartitionIndex, "tracker",
			"CREWLET_L2_TRACKER_999_TRACKER", "crewlet.l2.tracker.999.tracker", "crewlet.l2.tracker.999.tracker.>"},
	} {
		if got := topics.PartitionLogStream(tc.layout, tc.space, tc.index, tc.domain); got != tc.stream {
			t.Errorf("stream of (%d, %s, %d, %s) is %q, pinned as %q",
				tc.layout, tc.space, tc.index, tc.domain, got, tc.stream)
		}
		if got := topics.PartitionLogPrefix(tc.layout, tc.space, tc.index, tc.domain); got != tc.prefix {
			t.Errorf("prefix of (%d, %s, %d, %s) is %q, pinned as %q",
				tc.layout, tc.space, tc.index, tc.domain, got, tc.prefix)
		}
		if got := topics.PartitionLogWildcard(tc.layout, tc.space, tc.index, tc.domain); got != tc.wildcard {
			t.Errorf("wildcard of (%d, %s, %d, %s) is %q, pinned as %q",
				tc.layout, tc.space, tc.index, tc.domain, got, tc.wildcard)
		}
	}
}

// LAYOUT 0 ANSWERS TODAY'S NAMES, AND FROM TODAY'S CONSTANTS.
//
// Every build between the vocabulary landing and the first partitioned
// layout runs layout 0 on a live fleet, whose broker holds these three
// streams. Answered from the constants rather than a second spelling, and
// held against literal strings too, so neither a changed constant nor a
// changed table can move a stream a running fleet holds.
func TestLayoutZeroNamesTheThreeLogsAsTheyHaveAlwaysBeenNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		domain                   string
		stream, prefix, wildcard string
		literal                  string
	}{
		{"tracker", topics.TrackerLogStream, topics.TrackerLogPrefix, topics.TrackerLogWildcard, "CREWLET_TRACKER_LOG"},
		{"vectors", topics.TrackerVectorsStream, topics.TrackerVectorsPrefix, topics.TrackerVectorsWildcard, "CREWLET_TRACKER_VECTORS"},
		{"pages", topics.PagesLogStream, topics.PagesLogPrefix, topics.PagesLogWildcard, "CREWLET_PAGES_LOG"},
	} {
		if got := topics.PartitionLogStream(0, "estate", 0, tc.domain); got != tc.stream || got != tc.literal {
			t.Errorf("layout 0's %s stream is %q, want %q (%q)", tc.domain, got, tc.stream, tc.literal)
		}
		if got := topics.PartitionLogPrefix(0, "estate", 0, tc.domain); got != tc.prefix {
			t.Errorf("layout 0's %s prefix is %q, want %q", tc.domain, got, tc.prefix)
		}
		if got := topics.PartitionLogWildcard(0, "estate", 0, tc.domain); got != tc.wildcard {
			t.Errorf("layout 0's %s wildcard is %q, want %q", tc.domain, got, tc.wildcard)
		}
	}
}

// LAYOUT 0 NAMES NOTHING BUT THOSE THREE, AND ONLY IN estate.000.
//
// Answering a partitioned-looking request with today's stream would put a
// second history behind a name that already has one: a tracker.007 log
// silently writing into the whole estate's tracker log.
func TestLayoutZeroNamesNoOtherLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		space  string
		index  int
		domain string
	}{
		{"another partition of estate", "estate", 1, "tracker"},
		{"a partitioned space", "tracker", 0, "tracker"},
		{"a partitioned space's partition", "tracker", 7, "vectors"},
		{"a domain layout 0 never had", "estate", 0, "turns"},
		{"no domain at all", "estate", 0, ""},
		{"no space at all", "", 0, "tracker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for what, got := range map[string]string{
				"stream":   topics.PartitionLogStream(0, tc.space, tc.index, tc.domain),
				"prefix":   topics.PartitionLogPrefix(0, tc.space, tc.index, tc.domain),
				"wildcard": topics.PartitionLogWildcard(0, tc.space, tc.index, tc.domain),
			} {
				if got != "" {
					t.Errorf("layout 0 named a %s %q for (%s, %d, %s)",
						what, got, tc.space, tc.index, tc.domain)
				}
			}
		})
	}
}

// THE GRAMMAR REFUSES EVERY REQUEST IT COULD NOT NAME INJECTIVELY.
//
// Each case is one of the reasons in the file's doc: a part that is a subject
// wildcard or separator, an underscore that would let two requests share a
// stream name, a capital that upper-casing would merge, an index the three
// digits cannot hold. The empty string is the answer, never a name built
// around the defect.
func TestThePartitionGrammarRefusesWhatItCannotNameInjectively(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		layout int
		space  string
		index  int
		domain string
	}{
		{"a negative layout", -1, "tracker", 7, "tracker"},
		{"a space with an underscore", 1, "track_er", 7, "tracker"},
		{"a domain with an underscore", 1, "tracker", 7, "b_007_c"},
		{"a capital in the space", 1, "Tracker", 7, "tracker"},
		{"a capital in the domain", 1, "tracker", 7, "Vectors"},
		{"a dot in the space", 1, "tra.cker", 7, "tracker"},
		{"a dot in the domain", 1, "tracker", 7, "a.b"},
		{"a wildcard in the domain", 1, "tracker", 7, "*"},
		{"a tail wildcard in the space", 1, ">", 7, "tracker"},
		{"whitespace in the domain", 1, "tracker", 7, "a b"},
		{"a hyphen in the space", 1, "track-er", 7, "tracker"},
		{"a leading digit", 1, "7tracker", 7, "tracker"},
		{"an empty space", 1, "", 7, "tracker"},
		{"an empty domain", 1, "tracker", 7, ""},
		{"a negative index", 1, "tracker", -1, "tracker"},
		{"an index past three digits", 1, "tracker", topics.MaxPartitionIndex + 1, "tracker"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for what, got := range map[string]string{
				"stream":   topics.PartitionLogStream(tc.layout, tc.space, tc.index, tc.domain),
				"prefix":   topics.PartitionLogPrefix(tc.layout, tc.space, tc.index, tc.domain),
				"wildcard": topics.PartitionLogWildcard(tc.layout, tc.space, tc.index, tc.domain),
			} {
				if got != "" {
					t.Errorf("built a %s %q from (%d, %q, %d, %q)",
						what, got, tc.layout, tc.space, tc.index, tc.domain)
				}
			}
		})
	}
}

// NO TWO LOGS SHARE A STREAM, AND NO TWO SUBJECT SPACES OVERLAP.
//
// Every log the fleet provisions lives on one broker. Two requests naming one
// stream would put two domains' histories in one sequence space; one subject
// space inside another would let the wider stream's wildcard take the
// narrower one's records, silently, because both decode as bytes. The file's
// doc argues this from the grammar's shape; this is the argument checked over
// a product of the cases where it would most plausibly fail — layouts 1 and
// 11, indices sharing digits, spaces and domains that are the same words, and
// layout 0's names beside them.
func TestNoTwoPartitionLogsShareAStreamOrASubjectSpace(t *testing.T) {
	t.Parallel()
	type request struct {
		layout        int
		space, domain string
		index         int
	}
	var requests []request
	for _, layout := range []int{1, 2, 11, 111} {
		for _, space := range []string{"tracker", "pages", "company", "estate", "track", "trackerpages"} {
			for _, index := range []int{0, 1, 7, 10, 11, 70, 100, 111, topics.MaxPartitionIndex} {
				for _, domain := range []string{"tracker", "vectors", "pages", "pagesvectors"} {
					requests = append(requests, request{layout, space, domain, index})
				}
			}
		}
	}
	for _, domain := range []string{"tracker", "vectors", "pages"} {
		requests = append(requests, request{0, "estate", domain, 0})
	}

	streams := map[string]request{}
	spaces := map[string]request{}
	for _, r := range requests {
		stream := topics.PartitionLogStream(r.layout, r.space, r.index, r.domain)
		prefix := topics.PartitionLogPrefix(r.layout, r.space, r.index, r.domain)
		if stream == "" || prefix == "" {
			t.Fatalf("%+v was refused; every request in this sample is nameable", r)
		}
		if prior, dup := streams[stream]; dup {
			t.Errorf("%+v and %+v both name the stream %q", prior, r, stream)
		}
		streams[stream] = r
		spaces[prefix+"."] = r
	}
	for a, ra := range spaces {
		for b, rb := range spaces {
			if a != b && strings.HasPrefix(a, b) {
				t.Errorf("%+v's subject space %q lies inside %+v's %q", ra, a, rb, b)
			}
		}
	}
	if len(streams) != len(requests) {
		t.Fatalf("%d requests named %d streams", len(requests), len(streams))
	}
}

// NO PARTITIONED LOG'S SUBJECTS OVERLAP ANOTHER NAMESPACE IN THIS PACKAGE.
//
// The partitioned logs share the crewlet.* space with the inboxes, the
// events, the control plane and everything else a new constant here may add,
// so the namespaces are read from this package's own constants rather than
// listed: a namespace added tomorrow is checked by adding it and nothing else.
// The layout-0 names are among those constants and are excluded by value,
// since they ARE partition logs' subjects — of layout 0.
func TestNoPartitionLogOverlapsAnotherNamespace(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	constants := constStrings(t, filepath.Join(root, "internal", "queue", "topics"))

	layoutZero := map[string]bool{}
	for _, domain := range []string{"tracker", "vectors", "pages"} {
		layoutZero[topics.PartitionLogPrefix(0, "estate", 0, domain)] = true
		layoutZero[topics.PartitionLogWildcard(0, "estate", 0, domain)] = true
	}

	var namespaces []string
	for name, value := range constants {
		if strings.HasSuffix(name, "Root") || layoutZero[value] || !strings.HasPrefix(value, "crewlet.") {
			continue
		}
		namespaces = append(namespaces, strings.TrimRight(value, ".>*"))
	}
	if len(namespaces) < 5 {
		t.Fatalf("read only %d crewlet.* namespaces from the package's constants — the "+
			"derivation has stopped seeing the grammar and this test would pass having "+
			"compared nothing", len(namespaces))
	}

	for _, layout := range []int{1, 2, 10} {
		for _, space := range []string{"tracker", "pages", "company"} {
			for _, domain := range []string{"tracker", "vectors", "pages"} {
				prefix := topics.PartitionLogPrefix(layout, space, 7, domain) + "."
				for _, ns := range namespaces {
					other := ns + "."
					if strings.HasPrefix(prefix, other) || strings.HasPrefix(other, prefix) {
						t.Errorf("the partition log %q and the namespace %q overlap", prefix, ns)
					}
				}
			}
		}
	}
}

// A PARTITION'S NAME ROUND-TRIPS, AND THE INVERSE ACCEPTS ONLY WHAT THE
// BUILDER COULD HAVE PRODUCED.
//
// The name is a key in a register row, a map entry and a lease, so a lenient
// inverse would give one partition several keys and every one of those
// places would hold it more than once.
func TestAPartitionsNameRoundTripsAndNothingElseParses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		space string
		index int
		name  string
	}{
		{"tracker", 7, "tracker.007"},
		{"pages", 0, "pages.000"},
		{"company", 0, "company.000"},
		{"estate", 0, "estate.000"},
		{"tracker", 42, "tracker.042"},
		{"tracker", topics.MaxPartitionIndex, "tracker.999"},
	} {
		if got := topics.PartitionName(tc.space, tc.index); got != tc.name {
			t.Errorf("PartitionName(%q, %d) = %q, pinned as %q", tc.space, tc.index, got, tc.name)
		}
		space, index, ok := topics.ParsePartitionName(tc.name)
		if !ok || space != tc.space || index != tc.index {
			t.Errorf("%q parses as (%q, %d, %v), want (%q, %d, true)",
				tc.name, space, index, ok, tc.space, tc.index)
		}
	}
	for _, name := range []string{
		"", ".", ".000", "tracker", "tracker.", "tracker.7", "tracker.07",
		"tracker.0007", "tracker.1000", "tracker.-01", "tracker.+07", "tracker.00a",
		"tracker. 07", "Tracker.007", "track_er.007", "tracker.007.x", "tracker..007",
		" tracker.007", "tracker.007 ", "7tracker.007",
	} {
		if space, index, ok := topics.ParsePartitionName(name); ok {
			t.Errorf("%q parsed as (%q, %d) and PartitionName could not have produced it",
				name, space, index)
		}
	}
	if got := topics.PartitionName("", 0); got != "" {
		t.Errorf("a partition of no space was named %q", got)
	}
	if got := topics.PartitionName("tracker", topics.MaxPartitionIndex+1); got != "" {
		t.Errorf("an index past three digits was named %q", got)
	}
}

// A PARTITION LOG'S SUBJECTS CARRY ITS PARTITION'S NAME, AND ITS WILDCARD
// COVERS THEM.
//
// The first is what lets anything holding a subject read which partition it
// belongs to without a table; the second is the stream's own creation
// argument, and a subject outside it is refused by the broker at publish.
func TestAPartitionLogsSubjectsCarryItsPartitionAndItsWildcardCoversThem(t *testing.T) {
	t.Parallel()
	for _, space := range []string{"tracker", "pages", "company"} {
		for _, index := range []int{0, 7, topics.MaxPartitionIndex} {
			prefix := topics.PartitionLogPrefix(1, space, index, "tracker")
			if !strings.Contains(prefix, "."+topics.PartitionName(space, index)+".") {
				t.Errorf("%q does not carry its partition's name %q",
					prefix, topics.PartitionName(space, index))
			}
			wildcard := topics.PartitionLogWildcard(1, space, index, "tracker")
			covered, ok := strings.CutSuffix(wildcard, ">")
			if !ok || covered != prefix+"." {
				t.Errorf("the wildcard %q does not cover the subjects under %q", wildcard, prefix)
			}
		}
	}
}
