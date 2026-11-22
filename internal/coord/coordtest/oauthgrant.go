package coordtest

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- OAuth grants and sign-ins ------------------------------------------- //

func grantOf(f coord.GrantFamily, name string, at time.Time) coord.OAuthGrant {
	g := coord.OAuthGrant{
		Name: name, Grant: uuid.New(), Client: "cimd",
		ClientID: "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json",
		Issuer:   "https://auth.example.com", Scopes: []string{"read", "offline_access"},
		By: "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"), At: at,
	}
	if f.Resourced {
		g.Resource = "https://mcp.example.com/mcp"
	}
	return g
}

func signInOf(name string, at time.Time) coord.OAuthSignIn {
	return coord.OAuthSignIn{
		ID: uuid.New(), Name: name, Status: coord.OAuthSignInPending,
		StartedAt: at, ExpiresAt: at.Add(10 * time.Minute), Sealed: "v1:k1:sealed-envelope",
		By: "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"),
	}
}

// oauthGrantCases are one family's cases, which every family passes.
func oauthGrantCases(fam coord.GrantFamily) []fleetCase {
	return []fleetCase{{

		// ONE GRANT PER SERVER, REWRITTEN ONLY AT THE VERSION READ: a renewal
		// that failed and a sign-in that replaced the grant meanwhile must not
		// write over each other.
		name: "a grant is created once and rewritten or removed at its version",
		fn: func(h *fleetHarness) {
			g := grantOf(fam, "linear", h.now())
			stored, created, err := h.f.CreateOAuthGrant(h.ctx, fam, g)
			if err != nil || !created || stored.Version == 0 || stored.Grant != g.Grant ||
				len(stored.Scopes) != 2 || !stored.At.Equal(h.now()) {
				h.t.Fatalf("CreateOAuthGrant = (%+v, %v, %v)", stored, created, err)
			}
			if _, again, rerr := h.f.CreateOAuthGrant(h.ctx, fam, grantOf(fam, "linear", h.now())); rerr != nil || again {
				h.t.Fatalf("a second grant for one name was created: (%v, %v)", again, rerr)
			}
			failed := stored
			at := h.now().Add(time.Hour)
			failed.Failure, failed.FailedAt = "invalid_grant: the refresh token was revoked", &at
			moved, ok, err := h.f.UpdateOAuthGrant(h.ctx, fam, failed)
			if err != nil || !ok || !moved.Failed() || moved.Version == stored.Version {
				h.t.Fatalf("UpdateOAuthGrant = (%+v, %v, %v)", moved, ok, err)
			}
			if _, ok, rerr := h.f.UpdateOAuthGrant(h.ctx, fam, stored); rerr != nil || ok {
				h.t.Fatalf("an update at a version that moved wrote: (%v, %v)", ok, rerr)
			}
			read, found, err := h.f.OAuthGrant(h.ctx, fam, "linear")
			if err != nil || !found || read.Failure != failed.Failure || read.Version != moved.Version {
				h.t.Fatalf("OAuthGrant = (%+v, %v, %v)", read, found, err)
			}
			if gone, err := h.f.DeleteOAuthGrant(h.ctx, fam, "linear", stored.Version); err != nil || gone {
				h.t.Fatalf("a removal at a version that moved removed: (%v, %v)", gone, err)
			}
			if gone, err := h.f.DeleteOAuthGrant(h.ctx, fam, "linear", moved.Version); err != nil || !gone {
				h.t.Fatalf("DeleteOAuthGrant = (%v, %v)", gone, err)
			}
			if _, found, err := h.f.OAuthGrant(h.ctx, fam, "linear"); err != nil || found {
				h.t.Fatalf("a removed grant reads found=%t (%v)", found, err)
			}
			bad := grantOf(fam, "linear", h.now())
			bad.Failure = "no instant"
			if _, _, err := h.f.CreateOAuthGrant(h.ctx, fam, bad); err == nil {
				h.t.Fatal("a grant failing at no instant was written")
			}
		},
	}, {
		// EVERY NODE WATCHES THE GRANTS: a sign-in, a failed renewal and a
		// sign-out each reach every node that runs the server.
		name: "a grant watch delivers every grant, the marker, then each change",
		fn: func(h *fleetHarness) {
			first, _, err := h.f.CreateOAuthGrant(h.ctx, fam, grantOf(fam, "linear", h.now()))
			if err != nil {
				h.t.Fatalf("CreateOAuthGrant: %v", err)
			}
			ctx, cancel := context.WithCancel(h.ctx)
			defer cancel()
			updates, err := h.f.WatchOAuthGrants(ctx, fam)
			if err != nil {
				h.t.Fatalf("WatchOAuthGrants: %v", err)
			}
			if u := next(h, updates, "grant"); u.Name != "linear" || u.Grant == nil || u.Grant.Grant != first.Grant {
				h.t.Fatalf("update = %+v, want the existing grant", u)
			}
			if u := next(h, updates, "grant"); !u.Current {
				h.t.Fatalf("update = %+v, want the marker", u)
			}
			second, _, err := h.f.CreateOAuthGrant(h.ctx, fam, grantOf(fam, "notion", h.now()))
			if err != nil {
				h.t.Fatalf("CreateOAuthGrant: %v", err)
			}
			if u := next(h, updates, "grant"); u.Name != "notion" || u.Grant == nil || u.Grant.Version != second.Version {
				h.t.Fatalf("update after a sign-in = %+v, want it as stored", u)
			}
			if gone, err := h.f.DeleteOAuthGrant(h.ctx, fam, "linear", first.Version); err != nil || !gone {
				h.t.Fatalf("DeleteOAuthGrant = (%v, %v)", gone, err)
			}
			if u := next(h, updates, "grant"); u.Name != "linear" || u.Grant != nil {
				h.t.Fatalf("update after a sign-out = %+v, want the grant removed", u)
			}
		},
	}, {
		// A SIGN-IN'S STATE IS SPENT ONCE: the callback moves the pending
		// sign-in on at the version it read, and a second callback carrying the
		// same state finds that version gone, on any node.
		name: "a sign-in is moved on once at the version read",
		fn: func(h *fleetHarness) {
			s := signInOf("linear", h.now())
			stored, created, err := h.f.CreateOAuthSignIn(h.ctx, fam, s)
			if err != nil || !created || stored.Version == 0 || stored.Sealed != s.Sealed {
				h.t.Fatalf("CreateOAuthSignIn = (%+v, %v, %v)", stored, created, err)
			}
			if _, again, rerr := h.f.CreateOAuthSignIn(h.ctx, fam, signInOf("linear", h.now())); rerr != nil || again {
				h.t.Fatalf("a second sign-in for one name was created: (%v, %v)", again, rerr)
			}
			first, second := stored, stored
			first.Status, second.Status = coord.OAuthSignInCompleting, coord.OAuthSignInCompleting
			if _, ok, rerr := h.f.UpdateOAuthSignIn(h.ctx, fam, first); rerr != nil || !ok {
				h.t.Fatalf("the first callback could not move the sign-in on: (%v, %v)", ok, rerr)
			}
			if _, ok, rerr := h.f.UpdateOAuthSignIn(h.ctx, fam, second); rerr != nil || ok {
				h.t.Fatalf("a second callback with the same state moved it on too: (%v, %v)", ok, rerr)
			}
			read, found, err := h.f.OAuthSignIn(h.ctx, fam, "linear")
			if err != nil || !found || read.Status != coord.OAuthSignInCompleting || read.ID != s.ID {
				h.t.Fatalf("OAuthSignIn = (%+v, %v, %v)", read, found, err)
			}
			listed, err := h.f.ListOAuthSignIns(h.ctx, fam)
			if err != nil || len(listed) != 1 || listed[0].ID != s.ID {
				h.t.Fatalf("ListOAuthSignIns = (%+v, %v)", listed, err)
			}
			if gone, err := h.f.DeleteOAuthSignIn(h.ctx, fam, "linear", read.Version); err != nil || !gone {
				h.t.Fatalf("DeleteOAuthSignIn = (%v, %v)", gone, err)
			}
			unsealed := signInOf("linear", h.now())
			unsealed.Sealed = ""
			if _, _, err := h.f.CreateOAuthSignIn(h.ctx, fam, unsealed); err == nil {
				h.t.Fatal("a pending sign-in with nothing to finish it with was written")
			}
		},
	}, {
		// THE FAMILIES NEVER MIX: one name in two families is two grants, so
		// an MCP server named like an integration kind reaches neither's.
		name: "a grant of one family is not read, listed or removed through another",
		fn: func(h *fleetHarness) {
			other := coord.MCPGrantFamily
			if fam == other {
				other = coord.IntegrationGrantFamily
			}
			stored, _, err := h.f.CreateOAuthGrant(h.ctx, fam, grantOf(fam, "shared", h.now()))
			if err != nil {
				h.t.Fatalf("CreateOAuthGrant: %v", err)
			}
			if _, found, err := h.f.OAuthGrant(h.ctx, other, "shared"); err != nil || found {
				h.t.Fatalf("the other family reads the grant: found=%t (%v)", found, err)
			}
			if listed, err := h.f.ListOAuthGrants(h.ctx, other); err != nil || len(listed) != 0 {
				h.t.Fatalf("the other family lists %d grants (%v)", len(listed), err)
			}
			if gone, err := h.f.DeleteOAuthGrant(h.ctx, other, "shared", stored.Version); err != nil || gone {
				h.t.Fatalf("the other family removed the grant: (%v, %v)", gone, err)
			}
			if _, _, err := h.f.OAuthGrant(h.ctx, coord.GrantFamily{GrantClass: "made_up"}, "shared"); err == nil {
				h.t.Fatal("a family this build does not name was read")
			}
		},
	}, {
		// A RESOURCED FAMILY'S GRANT NAMES ITS RESOURCE (an MCP server's
		// address, RFC 8707), and every grant's accounts are bounded.
		name: "a grant is held to its family's resource rule and the account bounds",
		fn: func(h *fleetHarness) {
			bare := grantOf(fam, "bare", h.now())
			bare.Resource = ""
			_, created, err := h.f.CreateOAuthGrant(h.ctx, fam, bare)
			if fam.Resourced && (err == nil || created) {
				h.t.Fatal("a resourced family's grant with no resource was written")
			}
			if !fam.Resourced && (err != nil || !created) {
				h.t.Fatalf("a grant of a family with no resources was refused: (%v, %v)", created, err)
			}
			many := grantOf(fam, "many", h.now())
			for i := range coord.MaxGrantAccounts + 1 {
				many.Accounts = append(many.Accounts, "pub-"+strconv.Itoa(i))
			}
			if _, _, err := h.f.CreateOAuthGrant(h.ctx, fam, many); err == nil {
				h.t.Fatal("a grant naming more accounts than a record holds was written")
			}
			many.Accounts = many.Accounts[:coord.MaxGrantAccounts]
			if _, created, err := h.f.CreateOAuthGrant(h.ctx, fam, many); err != nil || !created {
				h.t.Fatalf("a grant at the account bound was refused: (%v, %v)", created, err)
			}
		},
	}}
}
