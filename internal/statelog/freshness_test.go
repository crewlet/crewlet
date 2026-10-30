package statelog_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// freshnessBag is a request's parameters as the grammar reads them.
type freshnessBag map[string]string

func (b freshnessBag) String(key string) string { return b[key] }

func (b freshnessBag) Int(key string, def int) int {
	raw, ok := b[key]
	if !ok {
		return def
	}
	n := 0
	negative := strings.HasPrefix(raw, "-")
	for _, r := range strings.TrimPrefix(raw, "-") {
		n = n*10 + int(r-'0')
	}
	if negative {
		return -n
	}
	return n
}

func (b freshnessBag) Has(key string) bool { _, ok := b[key]; return ok }

// EVERY REFUSAL NAMES THE PARAMETER TO CHANGE, AND NO PACKAGE.
//
// The grammar is the framework's and every domain's surface reads it, so a
// refusal that opened with a package — it said "worklet:" when the parser was
// the tracker's, and the floor's said "statelog:" — named a domain the person
// asking a chat or a wiki question never asked about.
func TestEveryFreshnessRefusalNamesItsParameterAndNoPackage(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		bag   freshnessBag
		param string
	}{
		"a fifth level":           {freshnessBag{"read_level": "eventually"}, "read_level"},
		"a bare session":          {freshnessBag{"read_level": "session"}, "read_level"},
		"a floor that is not one": {freshnessBag{"min_position": "4711"}, "min_position"},
		"a floor out of range": {freshnessBag{
			"min_position": "CREWLET_CHAT_LOG@99999999:1"}, "min_position"},
		"a bound beside linearizable": {freshnessBag{
			"read_level": "linearizable", "max_lag_seq": "10"}, "max_lag_seq"},
		"a negative duration": {freshnessBag{"max_lag_seconds": "-1"}, "max_lag_seconds"},
		"a negative count":    {freshnessBag{"max_lag_seq": "-1"}, "max_lag_seq"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := statelog.ParseFreshness(c.bag)
			var refused *statelog.FreshnessError
			if !errors.As(err, &refused) || refused.Param != c.param {
				t.Fatalf("%v answered %v, want a refusal of %s", c.bag, err, c.param)
			}
			if !strings.HasPrefix(err.Error(), c.param+": ") {
				t.Errorf("the refusal %q does not open with the parameter", err)
			}
			for _, pkg := range []string{"worklet:", "statelog:"} {
				if strings.Contains(err.Error(), pkg) {
					t.Errorf("the refusal %q names %q", err, pkg)
				}
			}
		})
	}
	// AND A FLOOR OUT OF RANGE STILL SAYS WHICH RULE IT BROKE.
	_, err := statelog.ParseFreshness(freshnessBag{"min_position": "CREWLET_CHAT_LOG@99999999:1"})
	if !errors.Is(err, statelog.ErrPositionRange) {
		t.Errorf("an out-of-range floor answered %v, want ErrPositionRange beneath it", err)
	}
}

// WHAT IT ACCEPTS IT CARRIES WHOLE: the level, both bounds and the floor.
func TestFreshnessCarriesEverythingItAccepts(t *testing.T) {
	t.Parallel()
	got, err := statelog.ParseFreshness(freshnessBag{
		"read_level": "stale", "max_lag_seconds": "30", "max_lag_seq": "250",
		"min_position": "CREWLET_CHAT_LOG@1:7",
	})
	want := statelog.Freshness{Level: statelog.ReadStale, MaxLag: 30 * time.Second,
		MaxLagSeq: 250, MinPosition: statelog.Position{Stream: "CREWLET_CHAT_LOG",
			Generation: 1, Seq: 7}}
	if err != nil || got != want {
		t.Fatalf("parsed %+v (%v), want %+v", got, err, want)
	}
}
