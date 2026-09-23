package stream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/iam"
)

// A WATCH IS RE-DECIDED WITH THE CREDENTIAL, and only a refusal withdraws it.
//
// A socket re-resolves its credential every interval precisely because a
// decision kept for the life of a tab outlives what it decided (revalidate.go).
// A watch is one of those decisions: a lead moved off a team must stop
// following a former report's inbox within an interval, not at their next
// reconnect. But a chart this node momentarily cannot read has taught it
// NOTHING against a decision it already made, so that answer keeps the watch —
// a blip that silently unsubscribed every lead's screen would be a
// notification outage caused by the node being behind.
func TestARevalidatedWatchIsWithdrawnOnlyByARefusal(t *testing.T) {
	t.Parallel()
	chart := &mutableChart{leads: true}
	lead := person("platform-lead")
	f := openRevalidatedOver(t, lead, func(r *http.Request) (*http.Request, *auth.Refusal) {
		return r.WithContext(iam.WithPrincipal(r.Context(), person("platform-lead"))), nil
	}, func(context.Context, string, map[string]any) (any, error) { return nil, nil }, chart)
	if got := f.read(t); got.Kind != KindSnapshot {
		t.Fatalf("first frame is %q, want the snapshot", got.Kind)
	}

	f.write(t, map[string]any{"kind": "watch", "seat": "sarah-chen"})
	waitUntil(t, func() bool { return f.svc.Hub().Watchers("sarah-chen") == 1 },
		"a lead's watch of their report never reached the index")

	// UNKNOWN KEEPS IT, across several checks.
	chart.set(false, errors.New("the chart view is behind"))
	time.Sleep(4 * testInterval)
	if got := f.svc.Hub().Watchers("sarah-chen"); got != 1 {
		t.Fatalf("a chart this node could not read withdrew a watch it had "+
			"decided (%d watchers)", got)
	}

	// A REFUSAL WITHDRAWS IT, and says so on the socket.
	chart.set(false, nil)
	for {
		got := f.read(t)
		if got.Kind != KindError {
			continue
		}
		if raw := string(got.Data); raw != "" && raw != "null" {
			t.Fatalf("a watch refusal carried data %s", raw)
		}
		break
	}
	waitUntil(t, func() bool { return f.svc.Hub().Watchers("sarah-chen") == 0 },
		"a lead who no longer leads the seat is still watching it")
}

// mutableChart answers the one lead question the case asks, changeably.
type mutableChart struct {
	mu    sync.Mutex
	leads bool
	err   error
}

func (c *mutableChart) set(leads bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leads, c.err = leads, err
}

func (c *mutableChart) Leads(_ context.Context, actor, subject string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	return c.leads && actor == "platform-lead" && strings.EqualFold(subject, "sarah-chen"), nil
}

func (c *mutableChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *mutableChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *mutableChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

// waitUntil polls cond for up to ten seconds.
func waitUntil(t *testing.T, cond func() bool, why string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(why)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
