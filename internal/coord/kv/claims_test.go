package kv

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// openFleetClaims opens a fleet whose claims bucket asks for maxClaim, every
// other retention the suite's.
func openFleetClaims(t *testing.T, js jetstream.JetStream, prefix string,
	maxClaim time.Duration) (*FleetStore, error) {

	t.Helper()
	return OpenFleet(context.Background(), js, FleetConfig{
		BucketPrefix: prefix, RateWindow: time.Minute, MaxClaimTTL: maxClaim,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, RebaseRetention: 10 * time.Minute,
		CooldownMax: time.Hour, BudgetRetention: time.Hour,
		StatusFreshness: 10 * time.Minute, CustodyRetention: 10 * time.Minute,
	})
}

// THE CLAIMS BUCKET'S AGE IS ONLY EVER RAISED, because every claim carries its
// own deadline and the age is the ceiling on them: a bucket an earlier build
// created at the webhook's five minutes reaped a socket's thirty-minute claim
// at five, and a seat that reconnected at six minutes delivered the post again.
// And the ceiling Claim enforces is the age IN FORCE, never the config.
func TestTheClaimsBucketAgeIsOnlyEverRaised(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	js := jsOf(nc)
	for _, tc := range []struct {
		name     string
		existing time.Duration
		want     time.Duration
	}{
		{"a younger bucket is raised to the ceiling", 5 * time.Minute, coord.MaxClaimTTL},
		{"an older bucket keeps its age", 2 * coord.MaxClaimTTL, 2 * coord.MaxClaimTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			prefix := fmt.Sprintf("c%d", bucketSeq.Add(1))
			if _, err := js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
				Bucket: prefix + claimsSuffix, TTL: tc.existing,
			}); err != nil {
				t.Fatalf("pre-create the claims bucket: %v", err)
			}
			store, err := openFleetClaims(t, js, prefix, coord.MaxClaimTTL)
			if err != nil {
				t.Fatalf("OpenFleet over an existing claims bucket: %v", err)
			}
			status, err := store.claims.Status(ctx)
			if err != nil {
				t.Fatalf("claims bucket status: %v", err)
			}
			if got := status.TTL(); got != tc.want {
				t.Fatalf("claims bucket age after OpenFleet = %v, want %v", got, tc.want)
			}
			if won, err := store.Claim(ctx, "mattermost|alice|p1", tc.want, time.Now()); err != nil || !won {
				t.Fatalf("a claim at the age in force = (%v, %v), want a win", won, err)
			}
			_, err = store.Claim(ctx, "mattermost|alice|p2", tc.want+time.Second, time.Now())
			if !errors.Is(err, coord.ErrTTLTooLong) {
				t.Fatalf("a claim past the age in force = %v, want coord.ErrTTLTooLong", err)
			}
		})
	}
}

// A CLAIM VALUE THIS BUILD CANNOT DATE IS HELD. An earlier build wrote a bare
// timestamp and judged a claim by the key existing at all; read as lapsed, it
// would hand that build's live claim to a second caller.
func TestAnUndatableClaimIsHeld(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	ctx := context.Background()
	store, err := openFleetClaims(t, jsOf(nc), fmt.Sprintf("c%d", bucketSeq.Add(1)), time.Minute)
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	if _, err := store.claims.Create(ctx, encodeKey("gitlab|old"),
		[]byte(time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano))); err != nil {
		t.Fatalf("plant an older build's claim: %v", err)
	}
	won, err := store.Claim(ctx, "gitlab|old", time.Minute, time.Now())
	if err != nil || won {
		t.Fatalf("Claim over an undatable record = (%v, %v), want held", won, err)
	}
}
