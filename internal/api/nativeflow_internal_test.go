package api

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE NATIVE SOURCES ANSWER EVERY READ THE ADVISOR MAKES OF THE TRACKER: the
// sources a node serves (NativeReads.Into, as `crewlet run` wires them) carry a
// work reader that answers how long work stood in each flow state and how many
// items each seat had, so the staffing and flow classes are judged on a real
// node and not only on a test's reader. Mutation: drop a forward from
// liveWork, and the node's reader answers neither.
func TestTheNativeSourcesAnswerEveryReadTheAdvisorMakes(t *testing.T) {
	t.Parallel()
	var s queries.Sources
	NativeReads{Work: liveWork{}, Pages: livePages{}, Backlinks: liveBacklinks{}, WorkSearch: liveWorkSearch{},
		Files: liveFiles{}, Chat: liveChat{}, ChatSearch: liveChatSearch{}, ChatDraft: liveChat{}}.Into(&s)
	flows, ok := s.Work.(interface {
		FlowHours(ctx context.Context, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error)
		FlowItems(ctx context.Context, q worklet.FlowHoursQuery) (map[string]int, error)
		FlowPickups(ctx context.Context, q worklet.FlowHoursQuery) ([]time.Duration, error)
	})
	if !ok || flows == nil {
		t.Fatalf("the native work reader %T answers neither the flow hours nor the flow items read", s.Work)
	}
}
