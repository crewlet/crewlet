package iamdomain_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// A SHARED WRITER CARRIES NO MARK BETWEEN CONCURRENT GESTURES.
//
// The node's own writer is handed to the identity duties, the sign-in surface
// and every request that acts as the deployment, and they call it at once: the
// sweep publisher and the deactivation probe tick on their own goroutines, and
// a sign-in arrives on a request's. That writer used to keep ONE high-water mark
// and advance it from every one of them, which the race detector names as a
// data race on the first two concurrent publishes — and which, where it did not
// race, made one gesture's position the session wait of an unrelated one.
//
// So this runs the shapes that share it in production — the node's own
// closes and revocations, a sweep, and requests deriving their own sequence
// from it with As — all at once, under -race, and requires every one to land.
func TestASharedWriterCarriesNoMarkBetweenConcurrentGestures(t *testing.T) {
	t.Parallel()
	rig := newWriteRig(t)
	shared, err := iamdomain.NewWriter(iamdomain.WriterDeps{
		Publisher: rig.publisher, DB: rig.db, Actor: "node-a",
		ActorKind: iam.KindMachine,
		// THE NODE'S OWN TWO GRANTS, so the sweep's gate and the
		// administrative one both admit it — this case is about the
		// mark, not the gates.
		Grants: []iam.Grant{iam.GrantFleetOperate, iam.GrantPeopleManage},
		Now:    func() time.Time { return brokerAt },
	})
	if err != nil {
		t.Fatalf("build the node's writer: %v", err)
	}
	horizons := iamdomain.Horizons{Changes: 400 * 24 * time.Hour,
		Sessions: 90 * 24 * time.Hour}

	// ENOUGH OVERLAP THAT THE DETECTOR SEES IT on every run rather than on
	// some: four goroutines of one call each caught the shared mark about
	// half the time, and a guard that fails half the time is a claim.
	const gestures, rounds = 8, 3
	errs := make(chan error, 4*gestures*rounds)
	err = rig.during(func() error {
		var wg sync.WaitGroup
		for i := range gestures {
			person := fmt.Sprintf("018f3a9c-0000-7000-8000-%012d", i+1)
			wg.Add(4)
			go func() {
				defer wg.Done()
				for r := range rounds {
					_, err := shared.CloseSession(t.Context(),
						fmt.Sprintf("lineage-%d-%d", i, r), person,
						"signed_out", fmt.Sprintf("op-close-%d-%d", i, r))
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				for r := range rounds {
					_, err := shared.Revoke(t.Context(), person,
						fmt.Sprintf("op-revoke-%d-%d", i, r),
						"a test revocation")
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				for range rounds {
					_, err := shared.Sweep(t.Context(), horizons)
					errs <- err
				}
			}()
			go func() {
				defer wg.Done()
				for r := range rounds {
					// A REQUEST'S OWN SEQUENCE, derived while the
					// shared writer is publishing on the others.
					seq := shared.As(principalNamed("ana.admin", iam.KindPerson,
						[]iam.Grant{iam.GrantPeopleManage}))
					_, err := seq.CloseSession(t.Context(),
						fmt.Sprintf("request-lineage-%d-%d", i, r), person,
						"signed_out",
						fmt.Sprintf("op-request-close-%d-%d", i, r))
					errs <- err
				}
			}()
		}
		wg.Wait()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a concurrent gesture through the shared writer failed: %v", err)
		}
	}
	rig.drain()
	if got := rig.column(`SELECT lineage FROM iam_sessions`); len(got) != 0 {
		t.Errorf("closing sessions nobody opened wrote rows %v", got)
	}
	if got := rig.column(`SELECT person_id FROM iam_revocation_epochs`); len(got) != gestures {
		t.Errorf("%d revocations landed %d epoch rows", gestures, len(got))
	}
}
