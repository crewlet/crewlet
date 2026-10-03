package stream

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
)

// A WATCH IS RE-DECIDED WITH THE CREDENTIAL, and only a refusal withdraws it.
//
// A socket's credential is decided again whenever something could have moved
// it, a published company among them (lifetime.go), because a decision kept
// for the life of a tab outlives what it decided. A watch is one of those
// decisions: a lead moved off a team must stop following a former report's
// inbox when the chart that moves them is published, not at their next
// reconnect. But a chart this node momentarily cannot read has taught it
// NOTHING against a decision it already made, so that answer keeps the watch —
// a blip that silently unsubscribed every lead's screen would be a
// notification outage caused by the node being behind.
func TestADecidedWatchIsWithdrawnOnlyByARefusal(t *testing.T) {
	t.Parallel()
	chart := &mutableChart{leads: true}
	lead := person("platform-lead")
	svc := newDecidingService(t, chart)
	s := serve(t, svc, socketCase{principal: lead, opened: sessionOf(lead),
		decide: resolvedAs(lead)})
	s.settled(t, 1)

	s.write(t, map[string]any{"kind": "watch", "seat": "sarah-chen"})
	waitUntil(t, func() bool { return svc.Hub().Watchers("sarah-chen") == 1 },
		"a lead's watch of their report never reached the index")

	// UNKNOWN KEEPS IT, across a published company it was decided on.
	chart.set(false, errors.New("the chart view is behind"))
	svc.CompanyPublished()
	s.settled(t, 2)
	s.open(t)
	if got := svc.Hub().Watchers("sarah-chen"); got != 1 {
		t.Fatalf("a chart this node could not read withdrew a watch it had "+
			"decided (%d watchers)", got)
	}

	// A REFUSAL WITHDRAWS IT, and says so on the socket.
	chart.set(false, nil)
	svc.CompanyPublished()
	for {
		got := s.read(t)
		if got.Kind != KindError {
			continue
		}
		if raw := string(got.Data); raw != "" && raw != "null" {
			t.Fatalf("a watch refusal carried data %s", raw)
		}
		break
	}
	waitUntil(t, func() bool { return svc.Hub().Watchers("sarah-chen") == 0 },
		"a lead who no longer leads the seat is still watching it")
}

// A RE-CHECK WITHDRAWS ONLY THE WATCH IT DECIDED.
//
// The re-check runs on the decision's goroutine and asks the authority table
// with no lock held; the socket's own read loop installs watches meanwhile.
// When a viewer's seat moves — a rebind, a rename — the read loop installs the
// new seat's watch, allowed, at the very moment the old seat starts being
// refused. The re-check that read the OLD seat then comes back refused, and an
// unconditional clear withdrew the NEW watch and told the tab it was refused;
// the dashboard retries only an `unavailable` refusal, so that tab heard no
// inbox change until its next socket. The race is forced here, not waited for:
// the chart holds the re-check's question until the new watch is in.
func TestARecheckWithdrawsOnlyTheWatchItDecided(t *testing.T) {
	t.Parallel()
	hub := NewHub()
	client := NewClient(AudienceOf([]iam.Grant{iam.GrantStateRead}))
	hub.Register(client)
	chart := &heldChart{asked: make(chan struct{}), release: make(chan struct{})}
	w := &watching{hub: hub, client: client, chart: chart, holders: blindHolders{}}
	hub.Watch(client, "sarah-chen")

	ctx := iam.WithPrincipal(t.Context(), person("platform-lead"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.recheck(ctx)
	}()
	<-chart.asked
	hub.Watch(client, "sarah") // the read loop's allowed watch, mid-decision
	close(chart.release)
	<-done

	if got := hub.Watchers("sarah"); got != 1 {
		t.Errorf("the re-check of the old seat withdrew the new watch (%d watchers)",
			got)
	}
	select {
	case frame := <-client.Out():
		t.Errorf("the tab was told its watch was refused (%d bytes) while the watch it "+
			"holds is allowed", len(frame.Raw()))
	default:
	}

	// THE CONTROL: with nothing moving, the same refusal withdraws the watch
	// it decided and says so.
	chart.answerNow()
	hub.Watch(client, "sarah-chen")
	w.recheck(ctx)
	if got := hub.Watchers("sarah-chen"); got != 0 {
		t.Errorf("a refused watch nobody moved is still installed (%d watchers)", got)
	}
	select {
	case <-client.Out():
	default:
		t.Error("a withdrawn watch said nothing to the tab")
	}
}

// testReaders is the store width this package's internal cases build a
// service over: wide enough that no case meets the node's query ceiling.
const testReaders = 64

// blindHolders is an identity directory that can say nothing, for a case that
// never names a login — every one in this package's own suite names a seat.
type blindHolders struct{}

func (blindHolders) HolderRecord(context.Context, string) (string, error) {
	return "", errors.New("this fixture holds no identity directory")
}

// heldChart refuses every lead question, and holds the FIRST one until
// released — which is what lets a case land a watch mid-decision.
type heldChart struct {
	asked, release chan struct{}
	once           sync.Once
	now            bool
	mu             sync.Mutex
}

func (c *heldChart) answerNow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = true
}

func (c *heldChart) Leads(ctx context.Context, _, _ string) (bool, error) {
	c.mu.Lock()
	now := c.now
	c.mu.Unlock()
	if !now {
		c.once.Do(func() { close(c.asked) })
		select {
		case <-c.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	return false, nil
}

func (c *heldChart) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *heldChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *heldChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

// LeadsAnyone is never the question a seat-named watch asks, so it answers
// what [heldChart.Leads] finally does: nobody.
func (c *heldChart) LeadsAnyone(context.Context, string) (bool, error) {
	return false, nil
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

// LeadsAnyone is [mutableChart.Leads] asked of every subject.
func (c *mutableChart) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	return c.leads && actor == "platform-lead", nil
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
