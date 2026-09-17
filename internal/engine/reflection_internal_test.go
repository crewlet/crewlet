package engine

import (
	"context"
	"testing"
)

// A FIRST ATTACH THAT FAILS REFUSES THE APPLY, AND THE RETRY ATTACHES. A
// node's first company has no dispatcher behind it until one attaches, so
// serving that company without one is a company that learns nothing while
// looking healthy. The refusal is what earns the retry, which has to find
// nothing left over from the attempt that failed.
func TestAFailedFirstReflectAttachIsRefusedAndRetried(t *testing.T) {
	t.Parallel()
	e, q := engineOn(t)
	c := companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
`)
	// A broker that refuses the subscription is what fails an attach.
	if err := q.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := e.reconfigureReflection(t.Context(), c); err == nil {
		t.Fatal("a first company was served with no reflect dispatcher behind it")
	}
	if e.reflector != nil {
		t.Fatal("a dispatcher that never subscribed was kept, so the retry would " +
			"swap workers into it and nothing would ever read a turn")
	}

	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.reconfigureReflection(t.Context(), c); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if e.reflector == nil {
		t.Error("the retry on a healthy broker attached no dispatcher")
	}
}
