package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// LAYOUT 0 IS TODAY'S ESTATE: every registered domain, in the register's
// order, on the stream that domain declares today and under the key it is
// registered by today.
//
// This is the test that holds the layout vocabulary to the running system
// rather than to a copy of its names. Every build before the partitioned
// layout runs layout 0 against a broker holding these streams and a
// positions register holding these keys; a layout-0 log answering any other
// name would provision a second, empty stream beside the real one and read a
// company with nothing in it.
func TestLayoutZeroIsTodaysEstateUnderTodaysNames(t *testing.T) {
	t.Parallel()
	l := LayoutZero()
	if err := l.Validate(); err != nil {
		t.Fatalf("LayoutZero does not validate: %v", err)
	}
	estate := statelog.PartitionID{Space: statelog.SpaceEstate}
	if got := l.Partitions(); !slices.Equal(got, []statelog.PartitionID{estate}) {
		t.Fatalf("layout 0's partitions are %v, want exactly %s", got, estate)
	}

	logs := l.Logs(estate)
	domains := registeredDomains()
	if len(logs) != len(domains) {
		t.Fatalf("layout 0 carries %d logs and the register %d domains", len(logs), len(domains))
	}
	for i, d := range domains {
		log := logs[i]
		if log.Domain != d.Name() {
			t.Errorf("layout 0's log %d is %q, and the register's domain %d is %q — the "+
				"order is the operator surfaces' order", i, log.Domain, i, d.Name())
		}
		if got := log.String(); got != d.Name() {
			t.Errorf("layout 0's %s log is keyed %q; the positions register, the manifest "+
				"and the floors key it %q today", d.Name(), got, d.Name())
		}
		spec := d.Stream()
		stream, prefix := l.Stream(log)
		if stream != spec.Name || prefix != spec.SubjectPrefix {
			t.Errorf("layout 0 names the %s log (%q, %q); the domain declares (%q, %q)",
				d.Name(), stream, prefix, spec.Name, spec.SubjectPrefix)
		}
		wildcard := topics.PartitionLogWildcard(l.Number, string(estate.Space), int(estate.Index), d.Name())
		if !slices.Equal(spec.Subjects, []string{wildcard}) {
			t.Errorf("layout 0's %s wildcard is %q; the domain's stream is created over %v",
				d.Name(), wildcard, spec.Subjects)
		}
	}
}

// THE FIRST PARTITIONED LAYOUT HAS THE SHAPE ITS COSTS WERE COUNTED FOR.
//
// The broker figures the benchmark is judged against — 161 partition logs
// over 81 partitions at T = 64 — are functions of this shape: which spaces
// exist, which domains each carries, and the counts. A domain moved between
// spaces, a vectors log dropped from pages, or a second company partition
// would each change what the benchmark had to measure without anyone
// re-measuring it.
func TestDefaultLayoutOneHasTheShapeItsCostsWereCountedFor(t *testing.T) {
	t.Parallel()
	l := DefaultLayoutOne()
	if err := l.Validate(); err != nil {
		t.Fatalf("DefaultLayoutOne does not validate: %v", err)
	}
	trackerLog, vectors, pagesLog := tracker.Domain{}.Name(), search.Domain{}.Name(), pages.Domain{}.Name()
	for _, tc := range []struct {
		space   statelog.Space
		count   int
		domains []string
	}{
		{statelog.SpaceTracker, DefaultTrackerPartitions, []string{trackerLog, vectors}},
		{statelog.SpacePages, DefaultPagesPartitions, []string{pagesLog, vectors}},
		{statelog.SpaceCompany, CompanyPartitions, []string{trackerLog}},
		{statelog.SpaceEstate, 0, nil},
	} {
		if got := l.Count(tc.space); got != tc.count {
			t.Errorf("the %s space has %d partitions, want %d", tc.space, got, tc.count)
		}
		if tc.count == 0 {
			continue
		}
		var got []string
		for _, log := range l.Logs(statelog.PartitionID{Space: tc.space}) {
			got = append(got, log.Domain)
		}
		if !slices.Equal(got, tc.domains) {
			t.Errorf("a %s partition carries %v, want %v", tc.space, got, tc.domains)
		}
	}

	const T = DefaultTrackerPartitions
	partitions := l.Partitions()
	logs := 0
	carried := map[string]bool{}
	for _, p := range partitions {
		for _, log := range l.Logs(p) {
			logs++
			carried[log.Domain] = true
		}
	}
	if want := T + T/4 + 1; len(partitions) != want {
		t.Errorf("%d partitions, and the cost table counts T + T/4 + 1 = %d", len(partitions), want)
	}
	if want := 2*T + 2*(T/4) + 1; logs != want {
		t.Errorf("%d partition logs, and the cost table counts 2T + 2·T/4 + 1 = %d", logs, want)
	}
	for _, d := range registeredDomains() {
		if !carried[d.Name()] {
			t.Errorf("the registered domain %s has no log in the first partitioned layout", d.Name())
		}
		delete(carried, d.Name())
	}
	for d := range carried {
		t.Errorf("the first partitioned layout carries a log of %q, which nothing registers", d)
	}

	for _, tc := range []struct{ domain, partition, stream string }{
		{trackerLog, "tracker.007", "CREWLET_L1_TRACKER_007_TRACKER"},
		{vectors, "pages.015", "CREWLET_L1_PAGES_015_VECTORS"},
		{trackerLog, "company.000", "CREWLET_L1_COMPANY_000_TRACKER"},
	} {
		p, err := statelog.ParsePartitionID(tc.partition)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := l.Stream(statelog.LogID{Domain: tc.domain, Partition: p}); got != tc.stream {
			t.Errorf("%s@%s is the stream %q, pinned as %q", tc.domain, tc.partition, got, tc.stream)
		}
	}
}

// THE PAGES SPACE IS A WHOLE QUARTER OF THE TRACKER'S.
//
// The pages count is justified as "a pages partition's log grows at the same
// rate as a tracker partition's", which holds only while it is exactly the
// tracker count over the corpus divisor. Integer division would floor a
// tracker count the divisor does not divide and quietly size every pages log
// larger than its argument says.
func TestThePagesSpaceIsAWholeQuarterOfTheTrackers(t *testing.T) {
	t.Parallel()
	if DefaultTrackerPartitions%config.DerivedPagesLogDivisor != 0 {
		t.Fatalf("DefaultTrackerPartitions %d is not a multiple of DerivedPagesLogDivisor %d",
			DefaultTrackerPartitions, config.DerivedPagesLogDivisor)
	}
	if DefaultPagesPartitions*config.DerivedPagesLogDivisor != DefaultTrackerPartitions || DefaultPagesPartitions < 1 {
		t.Fatalf("DefaultPagesPartitions is %d, not the tracker's %d over %d",
			DefaultPagesPartitions, DefaultTrackerPartitions, config.DerivedPagesLogDivisor)
	}
}
