package atlas_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
)

// A SECOND SIGNAL IS NAMED: "and one more thing needs attention" told a reader
// there was something else and nothing of what. Two or more beyond the first
// are counted, and a second of the same kind is said as one more like it.
//
// Mutation: put the old "one more thing" sentence back and the first case fails.
func TestAnEntrysReasonNamesItsSecondSignal(t *testing.T) {
	signal := func(k atlas.SignalKind, s atlas.Severity) atlas.Signal { return atlas.Signal{Kind: k, Severity: s} }
	cases := []struct {
		name    string
		signals []atlas.Signal
		want    string
	}{
		{
			name: "two kinds",
			signals: []atlas.Signal{
				signal(atlas.SignalSignInExpired, atlas.SeverityWarning),
				signal(atlas.SignalToolServerDown, atlas.SeverityDanger),
			},
			want: "An instance of the tool server failed to start, and the tool server's sign-in has lapsed",
		},
		{
			name: "one kind twice",
			signals: []atlas.Signal{
				signal(atlas.SignalToolServerDown, atlas.SeverityDanger),
				signal(atlas.SignalToolServerDown, atlas.SeverityDanger),
			},
			want: "An instance of the tool server failed to start, and one more like it",
		},
		{
			name: "three",
			signals: []atlas.Signal{
				signal(atlas.SignalMonitorAlerting, atlas.SeverityDanger),
				signal(atlas.SignalMonitorWarning, atlas.SeverityWarning),
				signal(atlas.SignalMonitorNoData, atlas.SeverityWarning),
			},
			want: "A monitor is alerting, and 2 more things need attention",
		},
	}
	for _, c := range cases {
		if got := (atlas.Entry{Signals: c.signals}).Reason(); got != c.want {
			t.Errorf("%s: the reason is %q, want %q", c.name, got, c.want)
		}
	}
}
