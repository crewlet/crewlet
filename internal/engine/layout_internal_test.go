package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
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
		spec := estateSpec(d)
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

// runsLogs makes running every log s runs, placed in s's layout's order — for
// a case that builds a state log by hand rather than booting one.
func runsLogs(s *stateLog, running ...*runningLog) *stateLog {
	s.logs = newLogSet(s.layout, running)
	return s
}
