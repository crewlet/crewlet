package observe_test

import (
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/named"
	"github.com/crewlet/crewlet/internal/observe"
)

// THE ROW, THE LIVE ENVELOPE AND THE SEEDED ROW NAME ONE THING ALIKE: what the
// summary names is derived once, here, beside the summary itself, so a line
// seen live and the same line after a reload link to the same objects.
func TestEveryShapeOfAnEventCarriesWhatItsSummaryNames(t *testing.T) {
	t.Parallel()
	ev := events.New(types.WorkPickup{Agent: "a-1", RoleName: "SRE", Item: "0198-task", Key: "ENG-287"},
		events.TraceContext{TraceID: "tr", SpanID: "sp"})
	want := []named.Ref{{Kind: named.WorkItem, ID: "0198-task", Text: "ENG-287"}}

	rec, ok := observe.Record(ev)
	if !ok {
		t.Fatal("a pickup was not persisted")
	}
	if !reflect.DeepEqual(rec.Refs, want) {
		t.Errorf("the stored row names %+v, want %+v", rec.Refs, want)
	}
	env, ok := observe.Envelope(ev)
	if !ok {
		t.Fatal("a pickup has no live envelope")
	}
	if !reflect.DeepEqual(env.Refs, want) {
		t.Errorf("the live envelope names %+v, want %+v", env.Refs, want)
	}
	if row := observe.FeedRow(rec); !reflect.DeepEqual(row.Refs, want) {
		t.Errorf("the seeded feed row names %+v, want %+v", row.Refs, want)
	}
}
