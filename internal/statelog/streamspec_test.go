package statelog_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A SPEC IS THE DOMAIN'S OWN STREAM IN EVERYTHING BUT ITS CEILING.
//
// A position row and a floor are keyed by the domain's name, and every row
// the log writes — the checkpoint, the anchors — by the spec's stream, so a
// runner handed another stream's names would file one log's positions under
// another's. And the loop that reads a log, the arbitration a publisher forms
// and the duplicate window a retry leans on are all chosen from the domain's
// declaration, so a spec of another shape is one those choices are wrong for.
// The ceiling alone is the node's: Tier A sizes it.
func TestASpecIsTheDomainsOwnStreamInEverythingButItsCeiling(t *testing.T) {
	t.Parallel()
	d := probeDomain{}
	if err := d.Stream().Instantiates(d); err != nil {
		t.Fatalf("the domain's own stream is refused: %v", err)
	}
	smaller := d.Stream()
	smaller.MaxBytes = 1 << 20
	if err := smaller.Instantiates(d); err != nil {
		t.Errorf("a stream at another ceiling is refused, and the ceiling is the "+
			"one setting a node sizes for itself: %v", err)
	}
	for name, mutate := range map[string]func(*statelog.StreamSpec){
		"another stream name":       func(s *statelog.StreamSpec) { s.Name = secondProbeStream },
		"another subject prefix":    func(s *statelog.StreamSpec) { s.SubjectPrefix = secondProbePrefix },
		"another subject space":     func(s *statelog.StreamSpec) { s.Subjects = []string{secondProbePrefix + ".>"} },
		"another duplicate window":  func(s *statelog.StreamSpec) { s.Duplicates = time.Second },
		"other arbitrated kinds":    func(s *statelog.StreamSpec) { s.ArbitratedKinds = nil },
		"another per-subject bound": func(s *statelog.StreamSpec) { s.Replay, s.MaxPerSubject = statelog.ReplayCompacted, 1 },
		"another replay protocol": func(s *statelog.StreamSpec) {
			s.Replay, s.MaxPerSubject, s.MaxAge = statelog.ReplayCompacted, 1, time.Hour
		},
		"another domain's whole stream": func(s *statelog.StreamSpec) { *s = secondProbeDomain{}.Stream() },
	} {
		spec := d.Stream()
		mutate(&spec)
		if err := spec.Instantiates(d); err == nil {
			t.Errorf("a spec with %s is accepted as the %s domain's stream", name, d.Name())
		}
	}
	if err := d.Stream().Instantiates(nil); err == nil {
		t.Error("a spec is accepted as the stream of no domain at all")
	}
}
