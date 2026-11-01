package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/schedule"
)

// A DUTY CLAIMED BESIDE THE NODE'S OWN LOOP WAITS FOR THAT LOOP'S CLAIM, AND
// FOR NOTHING ELSE.
//
// TestTheRunningEstateIsTheDeclaredEstate claims the retention duty on a node
// whose own retention loop claims the same lease under the same owner. A claim
// made while the loop's is minting its epoch is answered ErrClaimInFlight,
// and a one-shot claim read that as failure: the test failed in CI whenever
// the loop's first tick landed there. Held mid-mint here, the claim is
// answered so, waits for the sibling to commit, and holds the duty. A store
// that cannot answer is an ErrUnavailable that is not that one, and fails at
// once.
func TestADutyClaimedBesideItsOwnLoopWaitsForIt(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	minting, release := make(chan struct{}), make(chan struct{})
	var held sync.Once
	backend := &memory.Backend{Epochs: coordtest.NewMintCounter(func(ctx context.Context, _ string) {
		held.Do(func() {
			close(minting)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	})}
	const owner = "node-0:boot-1"
	resource := coord.WorkerResource(retentionDutyName)
	claim := schedule.ClaimNamedDuty(backend, retentionDutyName, owner, "node-0", retentionDutyTTL)

	loop := make(chan error, 1)
	go func() {
		_, err := claim(ctx)
		loop <- err
	}()
	select {
	case <-minting:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop's claim never reached its mint")
	}

	first := make(chan error, 1)
	observed := func(ctx context.Context) (bool, error) {
		mine, err := claim(ctx)
		select {
		case first <- err:
		default:
		}
		return mine, err
	}
	done := make(chan error, 1)
	go func() { done <- claimHeld(ctx, observed, backend, resource, owner) }()
	if err := <-first; !errors.Is(err, coord.ErrClaimInFlight) {
		t.Fatalf("a claim while the loop's is minting = %v, want ErrClaimInFlight", err)
	}
	close(release)
	if err := <-loop; err != nil {
		t.Fatalf("the loop's claim: %v", err)
	}
	if err := <-done; err != nil {
		t.Errorf("a claim beside the loop's = %v, want it held once the loop's committed", err)
	}

	// A STORE THAT CANNOT ANSWER is not waited on.
	down := coordtest.NewFaulty(memory.New())
	down.Break(nil)
	broken := schedule.ClaimNamedDuty(down, retentionDutyName, owner, "node-0", retentionDutyTTL)
	err := claimHeld(ctx, broken, down, resource, owner)
	if !errors.Is(err, coord.ErrUnavailable) || errors.Is(err, coord.ErrClaimInFlight) {
		t.Errorf("a claim on a store that is down = %v, want its ErrUnavailable at once", err)
	}
}
