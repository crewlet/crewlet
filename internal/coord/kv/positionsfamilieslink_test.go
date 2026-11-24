package kv_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
)

// EVERY POSITIONS CLASS PAST A SMALL VALUE IS READ ACROSS A LEAF'S LINK AT ITS
// FIRST ASK: a model check at its bound, and an OAuth grant and a sign-in at
// their widest in text no link compresses, each held a second behind a reply
// sent ahead of it on a link at the slowest pace allowed. Every read of each
// decides a write somebody waits on, so it is given its reply's crossing and
// asks the leader once. Every other family of the register is a small value
// whose reply crosses the floor inside the read term
// (TestASmallClassCrossesTheFloorInsideTheReadTerm).
//
// Mutation: read the model checks, the grants or the sign-ins at the term
// (replyTerm in settle.go), and its row asks again or fails.
func TestEveryPositionsClassPastASmallValueIsReadAcrossALeafsLinkAtItsFirstAsk(t *testing.T) {
	t.Parallel()
	floor := int64(1<<20) * int64(time.Second) / int64(objstore.MiBPace)
	for _, c := range []struct {
		name  string
		write func(ctx context.Context, t *testing.T, member *kv.FleetStore)
		read  func(ctx context.Context, leaf *kv.FleetStore) error
	}{
		{"a model check", putModelCheck, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.ModelCheck(ctx, "anthropic/claude")
			return found1(found, err)
		}},
		{"an OAuth grant", putGrant, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.OAuthGrant(ctx, coord.MCPGrantFamily, "search")
			return found1(found, err)
		}},
		{"an OAuth sign-in", putSignIn, func(ctx context.Context, leaf *kv.FleetStore) error {
			_, found, err := leaf.OAuthSignIn(ctx, coord.IntegrationGrantFamily, "gitlab")
			return found1(found, err)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			across := kvtest.SlowLeaf(t, floor)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			c.write(ctx, t, across.Member)
			asked := leaderReads(t, across, positionsStream)
			across.Link.Delay(time.Second)
			if err := c.read(ctx, across.Leaf); err != nil {
				t.Fatalf("read %s at its widest across the link: %v", c.name, err)
			}
			if n := asked(); n != 1 {
				t.Errorf("reading %s at its widest across the link asked the leader %d times, want 1", c.name, n)
			}
		})
	}
}

func putModelCheck(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	frame := len(`{"d":""}`)
	answer, err := json.Marshal(map[string]string{"d": randomText(t, coord.MaxModelCheckAnswerBytes-frame)})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	c := coord.ModelCheck{ID: uuid.New(), Entry: "anthropic/claude", Node: "node-a", StartedAt: now,
		Deadline: now.Add(time.Hour), EndedAt: now, Answer: answer, By: "jane-doe", ByKind: iam.ActorHuman}
	if _, created, err := member.CreateModelCheck(ctx, c); err != nil || !created {
		t.Fatalf("store a model check at its bound = %v, %v", created, err)
	}
}

func putGrant(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	now := time.Now().UTC()
	g := coord.OAuthGrant{Name: "search", Grant: uuid.New(), Client: "own",
		ClientID: randomText(t, coord.MaxGrantAddressBytes), Issuer: randomText(t, coord.MaxGrantAddressBytes),
		Resource: randomText(t, coord.MaxGrantAddressBytes), By: "jane-doe", ByKind: iam.ActorHuman, At: now,
		Failure: randomText(t, coord.MaxGrantWords), FailedAt: &now}
	for range coord.MaxGrantScopes {
		g.Scopes = append(g.Scopes, randomText(t, coord.MaxGrantScopeBytes))
	}
	for range coord.MaxGrantAccounts {
		g.Accounts = append(g.Accounts, randomText(t, coord.MaxGrantAccount))
	}
	if _, created, err := member.CreateOAuthGrant(ctx, coord.MCPGrantFamily, g); err != nil || !created {
		t.Fatalf("store an OAuth grant at its widest = %v, %v", created, err)
	}
}

func putSignIn(ctx context.Context, t *testing.T, member *kv.FleetStore) {
	t.Helper()
	now := time.Now().UTC()
	s := coord.OAuthSignIn{ID: uuid.New(), Name: "gitlab", Status: coord.OAuthSignInPending,
		Detail: randomText(t, coord.MaxGrantWords), StartedAt: now, ExpiresAt: now.Add(time.Hour),
		Sealed: randomText(t, coord.MaxOAuthSealedBytes), By: "jane-doe", ByKind: iam.ActorHuman}
	if _, created, err := member.CreateOAuthSignIn(ctx, coord.IntegrationGrantFamily, s); err != nil || !created {
		t.Fatalf("store an OAuth sign-in at its widest = %v, %v", created, err)
	}
}
