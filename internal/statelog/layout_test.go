package statelog_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The layouts these tests read. The domain names are the three domains'
// Name()s, which this package cannot import; internal/engine's own tests hold
// its LayoutZero and DefaultLayoutOne against the registered domains.
func layoutZero() statelog.Layout {
	return statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker", "vectors", "pages"}},
	}}
}

func layoutOne() statelog.Layout {
	return statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 64, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpacePages, Partitions: 16, Domains: []string{"pages", "vectors"}},
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
	}}
}

// layoutFull is a layout at the largest count every partitioned space may
// have, so the names at the three digits' edge are exercised.
func layoutFull() statelog.Layout {
	return statelog.Layout{Number: 7, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: statelog.MaxPartitions, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpacePages, Partitions: statelog.MaxPartitions, Domains: []string{"pages", "vectors"}},
		{Space: statelog.SpaceCompany, Partitions: statelog.MaxPartitions, Domains: []string{"tracker"}},
	}}
}

func mustParse(t *testing.T, s string) statelog.PartitionID {
	t.Helper()
	p, err := statelog.ParsePartitionID(s)
	if err != nil {
		t.Fatalf("ParsePartitionID(%q): %v", s, err)
	}
	return p
}

// THE ENUM KNOWS ITS FOUR SPACES AND NOTHING ELSE.
//
// A space read off a record is a value, never a panic, and an unknown one is
// refused rather than carried: a partition of a space this build has no
// schema or partition function for is one it can neither open nor route to.
func TestEverySpaceIsValidAndNothingElseIs(t *testing.T) {
	t.Parallel()
	if len(statelog.Spaces) != 4 {
		t.Fatalf("Spaces lists %d spaces, want the four the layouts use", len(statelog.Spaces))
	}
	for _, s := range statelog.Spaces {
		if !s.Valid() {
			t.Errorf("%q is listed and not valid", s)
		}
	}
	for _, s := range []statelog.Space{"", "Estate", "shard", "tracker ", "search"} {
		if s.Valid() {
			t.Errorf("%q is valid and is no space", s)
		}
	}
}

// A PARTITION'S NAME IS ITS SPACE AND A THREE-DIGIT INDEX, PINNED.
//
// The name is a key in the positions register, the estate map and a duty's
// lease, and two tokens of every subject its logs carry. A build that spelled
// it differently would look for every partition under a key nothing wrote.
func TestAPartitionsNameIsPinned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		p    statelog.PartitionID
		want string
	}{
		{statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7}, "tracker.007"},
		{statelog.PartitionID{Space: statelog.SpacePages, Index: 3}, "pages.003"},
		{statelog.PartitionID{Space: statelog.SpaceCompany}, "company.000"},
		{statelog.PartitionID{Space: statelog.SpaceEstate}, "estate.000"},
		{statelog.PartitionID{Space: statelog.SpaceTracker, Index: 63}, "tracker.063"},
		{statelog.PartitionID{Space: statelog.SpaceTracker, Index: statelog.MaxPartitions - 1}, "tracker.999"},
	} {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("%#v renders %q, pinned as %q", tc.p, got, tc.want)
		}
	}
}

// EVERY PARTITION A LAYOUT CARRIES ROUND-TRIPS THROUGH ITS NAME.
func TestEveryPartitionRoundTripsThroughItsName(t *testing.T) {
	t.Parallel()
	for _, l := range []statelog.Layout{layoutZero(), layoutOne(), layoutFull()} {
		for _, p := range l.Partitions() {
			if !p.Valid() {
				t.Errorf("layout %d carries %#v, which is not valid", l.Number, p)
			}
			back, err := statelog.ParsePartitionID(p.String())
			if err != nil {
				t.Errorf("%q does not parse back: %v", p, err)
				continue
			}
			if back != p {
				t.Errorf("%q parses back as %#v, want %#v", p, back, p)
			}
		}
	}
}

// PARSING REFUSES EVERYTHING String COULD NOT HAVE PRODUCED FOR A VALID
// PARTITION, says so as ErrInvalidPartitionID, and says which rule it broke.
//
// A lenient parse would give one partition several keys, and each place
// keyed on the name would hold it more than once.
func TestParsingRefusesAnyOtherSpelling(t *testing.T) {
	t.Parallel()
	const shape = "is not a space and a three-digit index"
	for name, tc := range map[string]struct{ input, names string }{
		"empty":                              {"", shape},
		"the zero partition's own rendering": {".000", shape},
		"no index":                           {"tracker", shape},
		"an unpadded index":                  {"tracker.7", shape},
		"a four-digit index":                 {"tracker.0007", shape},
		"an index past three digits":         {"tracker.1000", shape},
		"a signed index":                     {"tracker.+07", shape},
		"a capital":                          {"Tracker.007", shape},
		"a trailing segment":                 {"tracker.007.x", shape},
		"surrounding whitespace":             {" tracker.007", shape},
		"a log key rather than a partition":  {"tracker@tracker.007", shape},
		"a space this build does not know":   {"shard.007", `the space "shard", which this build does not know`},
		"the estate space past its one":      {"estate.001", "exactly one partition, estate.000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, err := statelog.ParsePartitionID(tc.input)
			if err == nil {
				t.Fatalf("%q parsed as %#v", tc.input, p)
			}
			if !errors.Is(err, statelog.ErrInvalidPartitionID) {
				t.Fatalf("%q was refused with %v, which is not ErrInvalidPartitionID", tc.input, err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("the refusal %q does not say %q", err, tc.names)
			}
			if p != (statelog.PartitionID{}) {
				t.Fatalf("a refused parse still returned %#v", p)
			}
		})
	}
}

// THE ZERO PARTITION IS NO PARTITION.
//
// Read as `estate.000` it would be layout 0's whole estate, and a field
// somebody forgot to fill would address every row the node holds. So it is
// invalid, it has no name, and no layout carries it or any log of it.
func TestTheZeroPartitionIsNoPartition(t *testing.T) {
	t.Parallel()
	var zero statelog.PartitionID
	if zero.Valid() {
		t.Fatal("the zero PartitionID is valid")
	}
	if got := zero.String(); got != "" {
		t.Fatalf("the zero PartitionID renders as %q; a name would let it into a key", got)
	}
	for _, l := range []statelog.Layout{layoutZero(), layoutOne()} {
		if slices.Contains(l.Partitions(), zero) {
			t.Errorf("layout %d lists the zero partition", l.Number)
		}
		if logs := l.Logs(zero); logs != nil {
			t.Errorf("layout %d gives the zero partition the logs %v", l.Number, logs)
		}
		if name, prefix := l.Stream(statelog.LogID{Domain: "tracker", Partition: zero}); name != "" || prefix != "" {
			t.Errorf("layout %d names a stream (%q, %q) for a log of the zero partition", l.Number, name, prefix)
		}
	}
}

// ONLY estate.000 OF THE ESTATE SPACE IS A PARTITION.
//
// A log of estate.000 is keyed by its domain alone; were estate.001 valid,
// its tracker log's key would be one of two that meant layout 0's.
func TestTheEstateSpaceHasOnePartition(t *testing.T) {
	t.Parallel()
	if !(statelog.PartitionID{Space: statelog.SpaceEstate}).Valid() {
		t.Fatal("estate.000 is not valid")
	}
	if (statelog.PartitionID{Space: statelog.SpaceEstate, Index: 1}).Valid() {
		t.Fatal("estate.001 is valid")
	}
}

// AN INDEX THE THREE DIGITS CANNOT HOLD IS NO PARTITION, AND HAS NO NAME.
//
// Every name a partition appears in writes its index in three digits, so a
// larger one could only be written by widening some name and not another.
func TestAnIndexPastThreeDigitsIsNoPartition(t *testing.T) {
	t.Parallel()
	past := statelog.PartitionID{Space: statelog.SpaceTracker, Index: statelog.MaxPartitions}
	if past.Valid() {
		t.Fatalf("%#v is valid", past)
	}
	if got := past.String(); got != "" {
		t.Fatalf("%#v is named %q", past, got)
	}
	last := statelog.PartitionID{Space: statelog.SpaceTracker, Index: statelog.MaxPartitions - 1}
	if !last.Valid() {
		t.Fatalf("%#v, the last index three digits hold, is not valid", last)
	}
}

// A LOG OF estate.000 IS KEYED BY ITS DOMAIN ALONE — TODAY'S KEY.
//
// The positions register, the snapshot manifest and the session floors are
// keyed by domain name today. Every build before the partitioned layout runs
// layout 0 on a fleet whose records hold those keys, so a layout-0 log's key
// must be the domain's name exactly, and every other log's must carry its
// partition.
func TestALogsKeyIsPinned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		domain, partition, want string
	}{
		{"tracker", "estate.000", "tracker"},
		{"vectors", "estate.000", "vectors"},
		{"pages", "estate.000", "pages"},
		{"tracker", "tracker.007", "tracker@tracker.007"},
		{"vectors", "tracker.007", "vectors@tracker.007"},
		{"vectors", "pages.003", "vectors@pages.003"},
		{"tracker", "company.000", "tracker@company.000"},
	} {
		log := statelog.LogID{Domain: tc.domain, Partition: mustParse(t, tc.partition)}
		if got := log.String(); got != tc.want {
			t.Errorf("%s in %s is keyed %q, pinned as %q", tc.domain, tc.partition, got, tc.want)
		}
	}
	// Only estate.000's logs drop the partition: a log of any other
	// partition of that space — which no valid layout has — must still
	// never be read as layout 0's.
	other := statelog.LogID{Domain: "tracker", Partition: statelog.PartitionID{Space: statelog.SpaceEstate, Index: 1}}
	if got := other.String(); got != "tracker@estate.001" {
		t.Errorf("a log of estate.001 is keyed %q, which is not its own", got)
	}
	var keys []string
	for _, p := range layoutZero().Partitions() {
		for _, log := range layoutZero().Logs(p) {
			keys = append(keys, log.String())
		}
	}
	if want := []string{"tracker", "vectors", "pages"}; !slices.Equal(keys, want) {
		t.Fatalf("layout 0's logs are keyed %v, want today's register keys %v", keys, want)
	}
}

// EVERY LOG OF A LAYOUT HAS ITS OWN KEY, ITS OWN STREAM AND ITS OWN SUBJECT
// SPACE — and layouts 0 and 1 share none of the three.
//
// Two logs on one key would be two positions in one register cell; on one
// stream, two histories in one sequence space; with overlapping subjects,
// one stream's wildcard taking the other's records. Layouts 0 and 1 coexist
// on one broker across the cutover, so the check spans both.
func TestEveryLogHasItsOwnKeyStreamAndSubjectSpace(t *testing.T) {
	t.Parallel()
	for name, layouts := range map[string][]statelog.Layout{
		"layout 0":                     {layoutZero()},
		"layout 1":                     {layoutOne()},
		"a full layout":                {layoutFull()},
		"layouts 0 and 1 side by side": {layoutZero(), layoutOne()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			keys, streams, spaces := map[string]bool{}, map[string]bool{}, map[string]bool{}
			var prefixes []string
			for _, l := range layouts {
				for _, p := range l.Partitions() {
					for _, log := range l.Logs(p) {
						key := log.String()
						stream, prefix := l.Stream(log)
						if stream == "" || prefix == "" {
							t.Fatalf("layout %d names no stream for %s", l.Number, key)
						}
						if keys[key] || streams[stream] || spaces[prefix] {
							t.Fatalf("layout %d's %s (%s, %s) repeats a key, a stream or a "+
								"subject space already taken", l.Number, key, stream, prefix)
						}
						keys[key], streams[stream], spaces[prefix] = true, true, true
						prefixes = append(prefixes, prefix+".")
					}
				}
			}
			for _, a := range prefixes {
				for _, b := range prefixes {
					if a != b && strings.HasPrefix(a, b) {
						t.Errorf("the subject space %q lies inside %q", a, b)
					}
				}
			}
		})
	}
}

// THE STREAM CARRIES THE LAYOUT, AND THE KEY DOES NOT.
//
// Layout 1's tracker.007 and a repartitioned layout 2's have one key and two
// streams. That is what makes [statelog.Cut] key on the stream: a position of
// the old layout's log can never stand beside the new one's under one key.
func TestTheStreamCarriesTheLayoutAndTheKeyDoesNot(t *testing.T) {
	t.Parallel()
	one := layoutOne()
	two := layoutOne()
	two.Number = 2
	log := statelog.LogID{Domain: "tracker", Partition: mustParse(t, "tracker.007")}
	streamOne, _ := one.Stream(log)
	streamTwo, _ := two.Stream(log)
	if streamOne == "" || streamOne == streamTwo {
		t.Fatalf("layouts 1 and 2 name %s's stream %q and %q; a repartition must "+
			"create a disjoint set", log, streamOne, streamTwo)
	}
	if log.String() != "tracker@tracker.007" {
		t.Fatalf("the key is %q", log.String())
	}
}

// LAYOUT 0'S STREAMS ARE TODAY'S — BY CONSTANT AND BY LITERAL.
//
// A running fleet's broker holds these three streams. Held against the
// topics constants the domains declare today, and against the literal
// strings, so neither a moved constant nor a moved table can rename one.
func TestLayoutZeroNamesTodaysStreams(t *testing.T) {
	t.Parallel()
	l := layoutZero()
	estate := statelog.PartitionID{Space: statelog.SpaceEstate}
	for _, tc := range []struct {
		domain, stream, prefix, literal string
	}{
		{"tracker", topics.TrackerLogStream, topics.TrackerLogPrefix, "CREWLET_TRACKER_LOG"},
		{"vectors", topics.TrackerVectorsStream, topics.TrackerVectorsPrefix, "CREWLET_TRACKER_VECTORS"},
		{"pages", topics.PagesLogStream, topics.PagesLogPrefix, "CREWLET_PAGES_LOG"},
	} {
		stream, prefix := l.Stream(statelog.LogID{Domain: tc.domain, Partition: estate})
		if stream != tc.stream || stream != tc.literal || prefix != tc.prefix {
			t.Errorf("layout 0's %s log is (%q, %q), want (%q, %q)",
				tc.domain, stream, prefix, tc.literal, tc.prefix)
		}
	}
}

// A PARTITIONED LAYOUT'S STREAMS ARE PINNED.
func TestLayoutOnesStreamsArePinned(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	for _, tc := range []struct {
		domain, partition, stream, prefix string
	}{
		{"tracker", "tracker.007", "CREWLET_L1_TRACKER_007_TRACKER", "crewlet.l1.tracker.007.tracker"},
		{"vectors", "tracker.007", "CREWLET_L1_TRACKER_007_VECTORS", "crewlet.l1.tracker.007.vectors"},
		{"pages", "pages.003", "CREWLET_L1_PAGES_003_PAGES", "crewlet.l1.pages.003.pages"},
		{"vectors", "pages.015", "CREWLET_L1_PAGES_015_VECTORS", "crewlet.l1.pages.015.vectors"},
		{"tracker", "company.000", "CREWLET_L1_COMPANY_000_TRACKER", "crewlet.l1.company.000.tracker"},
	} {
		stream, prefix := l.Stream(statelog.LogID{Domain: tc.domain, Partition: mustParse(t, tc.partition)})
		if stream != tc.stream || prefix != tc.prefix {
			t.Errorf("%s@%s is (%q, %q), pinned as (%q, %q)",
				tc.domain, tc.partition, stream, prefix, tc.stream, tc.prefix)
		}
	}
}

// A LOG THE LAYOUT DOES NOT CARRY HAS NO STREAM.
//
// A name here is a stream a provisioner would create and a publisher would
// write into, for a log nothing applies.
func TestNoStreamForALogTheLayoutDoesNotCarry(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	for name, log := range map[string]statelog.LogID{
		"a domain the space does not carry": {Domain: "pages", Partition: mustParse(t, "tracker.007")},
		"an index past the space's count":   {Domain: "tracker", Partition: mustParse(t, "tracker.064")},
		"a second company partition":        {Domain: "tracker", Partition: mustParse(t, "company.001")},
		"vectors in the company space":      {Domain: "vectors", Partition: mustParse(t, "company.000")},
		"a space the layout does not carry": {Domain: "tracker", Partition: mustParse(t, "estate.000")},
		"no domain":                         {Partition: mustParse(t, "tracker.007")},
	} {
		if stream, prefix := l.Stream(log); stream != "" || prefix != "" {
			t.Errorf("%s: %v is named (%q, %q)", name, log, stream, prefix)
		}
	}
}

// VALIDATE REFUSES EVERY LAYOUT THAT CANNOT BE RUN, AND NAMES WHAT TO CHANGE.
//
// Each case is one rule, and the message must name the thing the reader has
// to edit — the space, the count, the domain — not merely that something is
// wrong.
func TestValidateRefusesALayoutThatCannotBeRun(t *testing.T) {
	t.Parallel()
	with := func(edit func(*statelog.Layout)) statelog.Layout {
		l := layoutOne()
		edit(&l)
		return l
	}
	for name, tc := range map[string]struct {
		layout statelog.Layout
		names  string
	}{
		"the zero layout, which is not layout 0": {statelog.Layout{}, "no spaces"},
		"a negative number":                      {with(func(l *statelog.Layout) { l.Number = -1 }), "negative"},
		"no spaces":                              {statelog.Layout{Number: 1}, "no spaces"},
		"an unknown space": {with(func(l *statelog.Layout) {
			l.Spaces[0].Space = "shard"
		}), `"shard"`},
		"a space twice": {with(func(l *statelog.Layout) {
			l.Spaces[1].Space = statelog.SpaceTracker
		}), "tracker is listed twice"},
		"no partitions": {with(func(l *statelog.Layout) {
			l.Spaces[0].Partitions = 0
		}), "tracker has 0 partitions"},
		"more partitions than three digits hold": {with(func(l *statelog.Layout) {
			l.Spaces[1].Partitions = statelog.MaxPartitions + 1
		}), "pages has 1001 partitions"},
		"no domains": {with(func(l *statelog.Layout) {
			l.Spaces[2].Domains = nil
		}), "company carries no domain"},
		"a domain twice": {with(func(l *statelog.Layout) {
			l.Spaces[0].Domains = []string{"tracker", "vectors", "tracker"}
		}), `domain "tracker" twice`},
		"a domain the grammar cannot name": {with(func(l *statelog.Layout) {
			l.Spaces[0].Domains = []string{"tracker", "Vectors"}
		}), "Vectors log of tracker.000"},
		"an empty domain": {with(func(l *statelog.Layout) {
			l.Spaces[2].Domains = []string{""}
		}), " log of company.000"},
		"a partitioned layout carrying the estate space": {with(func(l *statelog.Layout) {
			l.Spaces = append(l.Spaces, statelog.SpaceLayout{
				Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker"}})
		}), "estate space is layout 0's alone"},
		"layout 0 with a partitioned space": {statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
			{Space: statelog.SpaceTracker, Partitions: 1, Domains: []string{"tracker"}},
		}}, "exactly one space, estate"},
		"layout 0 with two partitions": {statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
			{Space: statelog.SpaceEstate, Partitions: 2, Domains: []string{"tracker"}},
		}}, "exactly one space, estate, with one partition"},
		"layout 0 beside a second space": {statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
			layoutZero().Spaces[0],
			{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
		}}, "exactly one space, estate"},
		"layout 0 with a log it never had": {statelog.Layout{Number: 0, Spaces: []statelog.SpaceLayout{
			{Space: statelog.SpaceEstate, Partitions: 1, Domains: []string{"tracker", "turns"}},
		}}, "turns log of estate.000"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := tc.layout.Validate()
			if err == nil {
				t.Fatalf("%+v validated", tc.layout)
			}
			if !errors.Is(err, statelog.ErrInvalidLayout) {
				t.Fatalf("refused with %v, which is not ErrInvalidLayout", err)
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("the refusal %q does not name %q", err, tc.names)
			}
		})
	}
}

// VALIDATE ACCEPTS THE SHAPES A DEPLOYMENT RUNS: today's estate, the first
// partitioned layout, a doubled repartition of it, and the largest count.
func TestValidateAcceptsTheLayoutsADeploymentRuns(t *testing.T) {
	t.Parallel()
	doubled := layoutOne()
	doubled.Number = 2
	doubled.Spaces[0].Partitions *= 2
	doubled.Spaces[1].Partitions *= 2
	for name, l := range map[string]statelog.Layout{
		"layout 0":                  layoutZero(),
		"layout 1":                  layoutOne(),
		"a doubled repartition":     doubled,
		"the largest count allowed": layoutFull(),
	} {
		if err := l.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// PARTITIONS IS EVERY PARTITION ONCE, IN THE ORDER OF THEIR NAMES, however the
// spaces were declared.
//
// The estate map lists them in exactly this order, and every operator
// surface reads a list of partitions as a sorted list of names.
func TestPartitionsListsEveryPartitionOnceInNameOrder(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	// Declared in an order that is NOT their names' order, so a list that
	// merely followed the declaration fails here.
	l.Spaces = []statelog.SpaceLayout{l.Spaces[0], l.Spaces[2], l.Spaces[1]}
	got := l.Partitions()
	if len(got) != 64+16+1 {
		t.Fatalf("%d partitions, want %d", len(got), 64+16+1)
	}
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.String())
	}
	if !slices.IsSorted(names) {
		t.Fatalf("the partitions are not in name order: %v", names)
	}
	if len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatalf("a partition is listed twice: %v", names)
	}
	if names[0] != "company.000" || names[1] != "pages.000" || names[len(names)-1] != "tracker.063" {
		t.Fatalf("the list runs %s, %s … %s", names[0], names[1], names[len(names)-1])
	}
}

// A PARTITION'S LOGS FOLLOW ITS SPACE'S DOMAIN ORDER, which is the order its
// file composes their schemas in.
func TestLogsFollowTheSpacesDomainOrder(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	for partition, want := range map[string][]string{
		"tracker.007": {"tracker@tracker.007", "vectors@tracker.007"},
		"pages.015":   {"pages@pages.015", "vectors@pages.015"},
		"company.000": {"tracker@company.000"},
		"tracker.064": nil,
		"company.001": nil,
	} {
		var got []string
		for _, log := range l.Logs(mustParse(t, partition)) {
			got = append(got, log.String())
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s's logs are %v, want %v", partition, got, want)
		}
	}
}

// COUNT IS THE SPACE'S PARTITIONS, AND ZERO FOR A SPACE THE LAYOUT LACKS.
func TestCountIsZeroForASpaceTheLayoutLacks(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	for s, want := range map[statelog.Space]int{
		statelog.SpaceTracker: 64,
		statelog.SpacePages:   16,
		statelog.SpaceCompany: 1,
		statelog.SpaceEstate:  0,
		"shard":               0,
	} {
		if got := l.Count(s); got != want {
			t.Errorf("Count(%q) = %d, want %d", s, got, want)
		}
	}
}

// A LAYOUT IS RECORDED UNDER THESE KEYS, AND DECODES BACK TO ITSELF.
//
// The layout in force is a recorded value — in the estate map and beside
// every partition's checkpoint — rather than a build's defaults, so its wire
// form is a contract between builds and is pinned here.
func TestALayoutIsRecordedUnderItsDocumentedKeys(t *testing.T) {
	t.Parallel()
	l := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 64, Domains: []string{"tracker", "vectors"}},
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
	}}
	raw, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"number":1,"spaces":[` +
		`{"space":"tracker","partitions":64,"domains":["tracker","vectors"]},` +
		`{"space":"company","partitions":1,"domains":["tracker"]}]}`
	if string(raw) != want {
		t.Fatalf("a layout records as\n\t%s\nwant\n\t%s", raw, want)
	}
	var back statelog.Layout
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := back.Validate(); err != nil {
		t.Fatalf("the decoded layout does not validate: %v", err)
	}
	if !slices.Equal(back.Partitions(), l.Partitions()) {
		t.Fatalf("the decoded layout has other partitions")
	}
	var missing statelog.Layout
	if err := json.Unmarshal([]byte(`{}`), &missing); err != nil {
		t.Fatal(err)
	}
	if err := missing.Validate(); !errors.Is(err, statelog.ErrInvalidLayout) {
		t.Fatalf("a record with no layout in it decoded to one that validates (%v); "+
			"it would read as layout 0's whole estate", err)
	}
}

// namedDomain is the probe domain under another name and shape, so a case can
// instantiate a layout's logs of a domain the layout carries.
type namedDomain struct {
	probeDomain
	name  string
	shape statelog.StreamShape
}

func (d namedDomain) Name() string                      { return d.name }
func (d namedDomain) StreamShape() statelog.StreamShape { return d.shape }

// trackerShaped is a domain named as the tracker is, with the budget the cases
// divide.
func trackerShaped(budget int64) namedDomain {
	shape := probeDomain{}.StreamShape()
	shape.MaxBytes = budget
	return namedDomain{name: "tracker", shape: shape}
}

// A LOG'S STREAM IS ITS DOMAIN'S SHAPE UNDER THE NAMES THE LAYOUT GIVES IT.
//
// Everything but the names and the ceiling is the domain's and is the same on
// every one of its logs — the replay, the arbitration, the bounds and the
// window are what the framework reads and writes the log by. The names are the
// layout's grammar, so a log of one partition is never addressed as another's.
func TestAStreamSpecIsTheDomainsShapeUnderTheLayoutsNames(t *testing.T) {
	t.Parallel()
	d := trackerShaped(1 << 30)
	for _, tc := range []struct {
		layout statelog.Layout
		log    statelog.LogID
	}{
		{layoutZero(), statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition}},
		{layoutOne(), statelog.LogID{Domain: "tracker", Partition: mustParse(t, "tracker.007")}},
		{layoutOne(), statelog.LogID{Domain: "tracker", Partition: mustParse(t, "company.000")}},
	} {
		spec := tc.layout.StreamSpec(d, tc.log)
		name, prefix := tc.layout.Stream(tc.log)
		wildcard := topics.PartitionLogWildcard(tc.layout.Number,
			string(tc.log.Partition.Space), int(tc.log.Partition.Index), tc.log.Domain)
		if spec.Name != name || spec.SubjectPrefix != prefix ||
			!slices.Equal(spec.Subjects, []string{wildcard}) {
			t.Errorf("%s of layout %d is (%q, %q, %v), and the grammar names it (%q, %q, %q)",
				tc.log, tc.layout.Number, spec.Name, spec.SubjectPrefix, spec.Subjects,
				name, prefix, wildcard)
		}
		if err := spec.Instantiates(d); err != nil {
			t.Errorf("%s of layout %d is not an instance of its own domain: %v",
				tc.log, tc.layout.Number, err)
		}
		if want := tc.layout.LogShare("tracker", d.shape.MaxBytes); spec.MaxBytes != want {
			t.Errorf("%s of layout %d reserves %d bytes, and its share is %d",
				tc.log, tc.layout.Number, spec.MaxBytes, want)
		}
	}
	// A COPY OF THE ARBITRATED KINDS: a spec that shared the domain's slice
	// would let one log's caller rewrite what every log arbitrates.
	spec := layoutZero().StreamSpec(d, statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition})
	spec.ArbitratedKinds[0] = "rewritten"
	if d.shape.ArbitratedKinds[0] == "rewritten" {
		t.Error("a log's spec shares its domain's arbitrated kinds, so one log's " +
			"caller rewrote them for every log")
	}
}

// A LOG THE LAYOUT DOES NOT CARRY, OR ANOTHER DOMAIN'S, HAS NO STREAM.
//
// The zero spec is what every constructor's validation refuses, so a caller
// that asked for a log the layout lacks is stopped where it builds rather than
// handed a stream under a guessed name.
func TestAStreamSpecOfALogTheLayoutDoesNotCarryIsRefused(t *testing.T) {
	t.Parallel()
	d := trackerShaped(1 << 30)
	for name, tc := range map[string]struct {
		layout statelog.Layout
		log    statelog.LogID
	}{
		"a partition past the space's count": {layoutOne(),
			statelog.LogID{Domain: "tracker", Partition: mustParse(t, "tracker.064")}},
		"a space the domain has no log in": {layoutOne(),
			statelog.LogID{Domain: "tracker", Partition: mustParse(t, "pages.003")}},
		"another domain's log": {layoutOne(),
			statelog.LogID{Domain: "vectors", Partition: mustParse(t, "tracker.003")}},
		"layout 0's key under a partitioned layout": {layoutOne(),
			statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition}},
	} {
		spec := tc.layout.StreamSpec(d, tc.log)
		if err := spec.Validate(); err == nil {
			t.Errorf("%s: %s of layout %d was given the stream %q", name, tc.log,
				tc.layout.Number, spec.Name)
		}
	}
}

// A DOMAIN'S BUDGET IS DIVIDED EVENLY ACROSS ITS LOGS, AND ITS LOGS TOGETHER
// RESERVE NO MORE THAN IT.
//
// A ceiling is a reservation the broker takes in full at create, so the sum
// over a domain's logs is what the domain costs every broker member holding
// them. Divided by a SPACE's partitions instead, the tracker's one company log
// would take the tracker's whole budget beside its 64 tracker-space logs, and
// the vectors' pages-space logs the vectors' whole budget again.
func TestALogsShareDividesItsDomainsBudgetAcrossItsLogs(t *testing.T) {
	t.Parallel()
	const budget = int64(64<<30) + 7 // not a multiple of any count, so the rounding shows
	for _, tc := range []struct {
		layout statelog.Layout
		domain string
		logs   int64
	}{
		{layoutZero(), "tracker", 1},
		{layoutOne(), "tracker", 64 + 1},
		{layoutOne(), "vectors", 64 + 16},
		{layoutOne(), "pages", 16},
		{layoutFull(), "vectors", 2 * statelog.MaxPartitions},
	} {
		share := tc.layout.LogShare(tc.domain, budget)
		if got := int64(len(tc.layout.LogsOf(tc.domain))); got != tc.logs {
			t.Fatalf("layout %d gives %s %d logs, want %d", tc.layout.Number, tc.domain, got, tc.logs)
		}
		total := share * tc.logs
		if total < budget || total-budget >= tc.logs {
			t.Errorf("layout %d: %s's %d logs at %d bytes each reserve %d against a "+
				"budget of %d — the share must cover the budget and exceed it by "+
				"less than a byte a log", tc.layout.Number, tc.domain, tc.logs,
				share, total, budget)
		}
	}
	if share := layoutOne().LogShare("absent", budget); share != 0 {
		t.Errorf("a domain the layout does not carry is given a share of %d", share)
	}
}

// EVERY LOG OF A LAYOUT, ONCE, IN PARTITION ORDER AND THEN DOMAIN ORDER, and a
// domain's logs are exactly its entries in that walk.
func TestAllLogsWalksEveryPartitionsLogsInOrder(t *testing.T) {
	t.Parallel()
	l := layoutOne()
	all := l.AllLogs()
	if want := 2*64 + 2*16 + 1; len(all) != want {
		t.Fatalf("layout 1 walks %d logs, want %d", len(all), want)
	}
	seen := map[string]bool{}
	for i, log := range all {
		if seen[log.String()] {
			t.Errorf("%s is walked twice", log)
		}
		seen[log.String()] = true
		if i > 0 && log.Partition == all[i-1].Partition {
			continue
		}
		if i > 0 && log.Partition.String() < all[i-1].Partition.String() {
			t.Errorf("%s is walked after %s", log, all[i-1])
		}
	}
	for _, domain := range []string{"tracker", "vectors", "pages"} {
		var want []statelog.LogID
		for _, log := range all {
			if log.Domain == domain {
				want = append(want, log)
			}
		}
		if got := l.LogsOf(domain); !slices.Equal(got, want) {
			t.Errorf("LogsOf(%s) = %d logs, and the walk holds %d of them", domain, len(got), len(want))
		}
	}
}

// ONLY A LAYOUT THAT GIVES A DOMAIN ONE LOG PLACES IT WITHOUT A KEY.
//
// It is the whole partition function of an unkeyed domain, so the answer under
// a layout that divides the domain must be a partition no layout carries —
// never one of the domain's logs picked by a guess.
func TestOnlyPartitionAnswersOnlyWhereThereIsOneLog(t *testing.T) {
	t.Parallel()
	if got := layoutZero().OnlyPartition("tracker"); got != statelog.EstatePartition {
		t.Errorf("layout 0 places the tracker in %q, want %s", got, statelog.EstatePartition)
	}
	one := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceCompany, Partitions: 1, Domains: []string{"tracker"}},
		{Space: statelog.SpacePages, Partitions: 4, Domains: []string{"pages"}},
	}}
	if got := one.OnlyPartition("tracker"); got != mustParse(t, "company.000") {
		t.Errorf("a layout with one tracker log places it in %q", got)
	}
	for _, domain := range []string{"pages", "absent"} {
		if got := one.OnlyPartition(domain); got.Valid() {
			t.Errorf("a layout with no single %s log places it in %s", domain, got)
		}
	}
}

// A DOMAIN'S LAYOUT-0 LOG IS NAMED AS IT HAS ALWAYS BEEN, AT ITS WHOLE BUDGET.
//
// EstateStream is what a domain's own code and its suites address layout 0's
// log by, so it has to be the stream the engine's layout 0 provisions — a
// domain alone in the estate space is named exactly as it is beside the others,
// and its share is its budget either way.
func TestEstateStreamIsTheDomainsLayoutZeroLog(t *testing.T) {
	t.Parallel()
	d := trackerShaped(3 << 30)
	alone := statelog.EstateStream(d)
	beside := layoutZero().StreamSpec(d, statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition})
	if alone.Name != topics.TrackerLogStream || alone.SubjectPrefix != topics.TrackerLogPrefix {
		t.Errorf("the tracker's layout-0 log is %q under %q, want %q under %q",
			alone.Name, alone.SubjectPrefix, topics.TrackerLogStream, topics.TrackerLogPrefix)
	}
	if alone.Name != beside.Name || alone.MaxBytes != beside.MaxBytes || alone.MaxBytes != 3<<30 {
		t.Errorf("alone the tracker's layout-0 log is %q at %d bytes; beside the others "+
			"it is %q at %d — want one stream at the whole %d",
			alone.Name, alone.MaxBytes, beside.Name, beside.MaxBytes, int64(3<<30))
	}
}

// A SPEC IS A LOG OF A DOMAIN ONLY IF EVERY SETTING BUT THE CEILING IS THE
// DOMAIN'S.
//
// The replay loop, the arbitration, the per-subject and age bounds and the
// duplicate window are each chosen from the DOMAIN's declaration by whatever
// reads or writes the log, so a spec that differs in any is a log those
// choices are wrong for — and no loop can see that from the inside.
func TestASpecOfAnotherShapeIsNotALogOfTheDomain(t *testing.T) {
	t.Parallel()
	d := trackerShaped(1 << 30)
	base := func() statelog.StreamSpec {
		return layoutZero().StreamSpec(d, statelog.LogID{Domain: "tracker", Partition: statelog.EstatePartition})
	}
	if err := base().Instantiates(d); err != nil {
		t.Fatalf("the domain's own log is refused: %v", err)
	}
	smaller := base()
	smaller.MaxBytes = 1 << 20
	if err := smaller.Instantiates(d); err != nil {
		t.Errorf("a log at another ceiling is refused, and the ceiling is the one "+
			"setting a log may have of its own: %v", err)
	}
	for name, mutate := range map[string]func(*statelog.StreamSpec){
		"another duplicate window": func(s *statelog.StreamSpec) { s.Duplicates = time.Second },
		"other arbitrated kinds":   func(s *statelog.StreamSpec) { s.ArbitratedKinds = nil },
		"another replay protocol": func(s *statelog.StreamSpec) {
			s.Replay, s.MaxPerSubject, s.MaxAge = statelog.ReplayCompacted, 1, time.Hour
		},
	} {
		spec := base()
		mutate(&spec)
		if err := spec.Instantiates(d); err == nil {
			t.Errorf("a spec with %s is accepted as a log of the domain", name)
		}
	}
}
