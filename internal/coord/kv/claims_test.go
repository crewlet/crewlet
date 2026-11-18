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
		BucketPrefix: prefix, Reach: InProcess, MaxRateWindow: time.Minute, MaxClaimTTL: maxClaim,
		LedgerRetention: 10 * time.Minute, FireRetention: 10 * time.Minute,
		FollowRetention: 10 * time.Minute, RebaseRetention: 10 * time.Minute,
		BudgetRetention: time.Hour,
		StatusFreshness: 10 * time.Minute, CustodyRetention: 10 * time.Minute,
	})
}

// THE CEILING CLAIM ENFORCES IS THE CLAIMS BUCKET'S AGE IN FORCE, never this
// node's config: the age caps every claim's own deadline, so a claim longer
// than the adopted bucket's age would be reaped live and its post delivered
// again. It is refused instead, and the bucket adopted as it stands.
func TestAClaimIsCappedByTheClaimsBucketAgeInForce(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	js := jsOf(nc)
	for _, tc := range []struct {
		name     string
		existing time.Duration
	}{
		{"a younger bucket", 5 * time.Minute},
		{"an older bucket", 2 * coord.MaxClaimTTL},
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
			if got := status.TTL(); got != tc.existing {
				t.Fatalf("claims bucket age after OpenFleet = %v, want it adopted at %v", got, tc.existing)
			}
			if won, err := store.Claim(ctx, "mattermost|alice|p1", tc.existing, time.Now()); err != nil || !won {
				t.Fatalf("a claim at the age in force = (%v, %v), want a win", won, err)
			}
			_, err = store.Claim(ctx, "mattermost|alice|p2", tc.existing+time.Second, time.Now())
			if !errors.Is(err, coord.ErrTTLTooLong) {
				t.Fatalf("a claim past the age in force = %v, want coord.ErrTTLTooLong", err)
			}
		})
	}
}

// A CLAIM VALUE THAT NAMES NO DEADLINE IS TAKEN OVER, as a lapsed one is. This
// store writes none, so it says nothing about a live holder, and the claim
// contract fails open: held, every delivery under that key would be dropped as
// a duplicate until the bucket reaped the value.
func TestAClaimValueThatNamesNoDeadlineIsTakenOver(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	ctx := context.Background()
	store, err := openFleetClaims(t, jsOf(nc), fmt.Sprintf("c%d", bucketSeq.Add(1)), time.Minute)
	if err != nil {
		t.Fatalf("OpenFleet: %v", err)
	}
	if _, err := store.claims.Create(ctx, encodeKey("gitlab|odd"), []byte("not a claim")); err != nil {
		t.Fatalf("plant a value that names no deadline: %v", err)
	}
	won, err := store.Claim(ctx, "gitlab|odd", time.Minute, time.Now())
	if err != nil || !won {
		t.Fatalf("Claim over a value that names no deadline = (%v, %v), want it taken over", won, err)
	}
	if won, err := store.Claim(ctx, "gitlab|odd", time.Minute, time.Now()); err != nil || won {
		t.Fatalf("a second Claim of the taken-over key = (%v, %v), want it held", won, err)
	}
}
