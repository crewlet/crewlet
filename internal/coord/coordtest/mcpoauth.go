package coordtest

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// ---- MCP grants and sign-ins --------------------------------------------- //

func grantOf(server string, at time.Time) coord.MCPGrant {
	return coord.MCPGrant{
		Server: server, Grant: uuid.New(), Client: "cimd",
		ClientID: "https://crewlet.example.com/webhooks/mcp-oauth/client-metadata.json",
		Issuer:   "https://auth.example.com", Scopes: []string{"read", "offline_access"},
		Resource: "https://mcp.example.com/mcp",
		By:       "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"), At: at,
	}
}

func signInOf(server string, at time.Time) coord.MCPSignIn {
	return coord.MCPSignIn{
		ID: uuid.New(), Server: server, Status: coord.MCPSignInPending,
		StartedAt: at, ExpiresAt: at.Add(10 * time.Minute), Sealed: "v1:k1:sealed-envelope",
		By: "jane-doe", ByKind: iam.ActorHuman, OperatorID: sessionOf("jane-doe"),
	}
}

var mcpOAuthCases = []fleetCase{{
	// ONE GRANT PER SERVER, REWRITTEN ONLY AT THE VERSION READ: a renewal
	// that failed and a sign-in that replaced the grant meanwhile must not
	// write over each other.
	name: "an MCP grant is created once and rewritten or removed at its version",
	fn: func(h *fleetHarness) {
		g := grantOf("linear", h.now())
		stored, created, err := h.f.CreateMCPGrant(h.ctx, g)
		if err != nil || !created || stored.Version == 0 || stored.Grant != g.Grant ||
			len(stored.Scopes) != 2 || !stored.At.Equal(h.now()) {
			h.t.Fatalf("CreateMCPGrant = (%+v, %v, %v)", stored, created, err)
		}
		if _, again, rerr := h.f.CreateMCPGrant(h.ctx, grantOf("linear", h.now())); rerr != nil || again {
			h.t.Fatalf("a second grant for one server was created: (%v, %v)", again, rerr)
		}
		failed := stored
		at := h.now().Add(time.Hour)
		failed.Failure, failed.FailedAt = "invalid_grant: the refresh token was revoked", &at
		moved, ok, err := h.f.UpdateMCPGrant(h.ctx, failed)
		if err != nil || !ok || !moved.Failed() || moved.Version == stored.Version {
			h.t.Fatalf("UpdateMCPGrant = (%+v, %v, %v)", moved, ok, err)
		}
		if _, ok, rerr := h.f.UpdateMCPGrant(h.ctx, stored); rerr != nil || ok {
			h.t.Fatalf("an update at a version that moved wrote: (%v, %v)", ok, rerr)
		}
		read, found, err := h.f.MCPGrant(h.ctx, "linear")
		if err != nil || !found || read.Failure != failed.Failure || read.Version != moved.Version {
			h.t.Fatalf("MCPGrant = (%+v, %v, %v)", read, found, err)
		}
		if gone, err := h.f.DeleteMCPGrant(h.ctx, "linear", stored.Version); err != nil || gone {
			h.t.Fatalf("a removal at a version that moved removed: (%v, %v)", gone, err)
		}
		if gone, err := h.f.DeleteMCPGrant(h.ctx, "linear", moved.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPGrant = (%v, %v)", gone, err)
		}
		if _, found, err := h.f.MCPGrant(h.ctx, "linear"); err != nil || found {
			h.t.Fatalf("a removed grant reads found=%t (%v)", found, err)
		}
		bad := grantOf("linear", h.now())
		bad.Failure = "no instant"
		if _, _, err := h.f.CreateMCPGrant(h.ctx, bad); err == nil {
			h.t.Fatal("a grant failing at no instant was written")
		}
	},
}, {
	// EVERY NODE WATCHES THE GRANTS: a sign-in, a failed renewal and a
	// sign-out each reach every node that runs the server.
	name: "an MCP grant watch delivers every grant, the marker, then each change",
	fn: func(h *fleetHarness) {
		first, _, err := h.f.CreateMCPGrant(h.ctx, grantOf("linear", h.now()))
		if err != nil {
			h.t.Fatalf("CreateMCPGrant: %v", err)
		}
		ctx, cancel := context.WithCancel(h.ctx)
		defer cancel()
		updates, err := h.f.WatchMCPGrants(ctx)
		if err != nil {
			h.t.Fatalf("WatchMCPGrants: %v", err)
		}
		if u := next(h, updates, "grant"); u.Server != "linear" || u.Grant == nil || u.Grant.Grant != first.Grant {
			h.t.Fatalf("update = %+v, want the existing grant", u)
		}
		if u := next(h, updates, "grant"); !u.Current {
			h.t.Fatalf("update = %+v, want the marker", u)
		}
		second, _, err := h.f.CreateMCPGrant(h.ctx, grantOf("notion", h.now()))
		if err != nil {
			h.t.Fatalf("CreateMCPGrant: %v", err)
		}
		if u := next(h, updates, "grant"); u.Server != "notion" || u.Grant == nil || u.Grant.Version != second.Version {
			h.t.Fatalf("update after a sign-in = %+v, want it as stored", u)
		}
		if gone, err := h.f.DeleteMCPGrant(h.ctx, "linear", first.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPGrant = (%v, %v)", gone, err)
		}
		if u := next(h, updates, "grant"); u.Server != "linear" || u.Grant != nil {
			h.t.Fatalf("update after a sign-out = %+v, want the grant removed", u)
		}
	},
}, {
	// A SIGN-IN'S STATE IS SPENT ONCE: the callback moves the pending
	// sign-in on at the version it read, and a second callback carrying the
	// same state finds that version gone, on any node.
	name: "an MCP sign-in is moved on once at the version read",
	fn: func(h *fleetHarness) {
		s := signInOf("linear", h.now())
		stored, created, err := h.f.CreateMCPSignIn(h.ctx, s)
		if err != nil || !created || stored.Version == 0 || stored.Sealed != s.Sealed {
			h.t.Fatalf("CreateMCPSignIn = (%+v, %v, %v)", stored, created, err)
		}
		if _, again, rerr := h.f.CreateMCPSignIn(h.ctx, signInOf("linear", h.now())); rerr != nil || again {
			h.t.Fatalf("a second sign-in for one server was created: (%v, %v)", again, rerr)
		}
		first, second := stored, stored
		first.Status, second.Status = coord.MCPSignInCompleting, coord.MCPSignInCompleting
		if _, ok, rerr := h.f.UpdateMCPSignIn(h.ctx, first); rerr != nil || !ok {
			h.t.Fatalf("the first callback could not move the sign-in on: (%v, %v)", ok, rerr)
		}
		if _, ok, rerr := h.f.UpdateMCPSignIn(h.ctx, second); rerr != nil || ok {
			h.t.Fatalf("a second callback with the same state moved it on too: (%v, %v)", ok, rerr)
		}
		read, found, err := h.f.MCPSignIn(h.ctx, "linear")
		if err != nil || !found || read.Status != coord.MCPSignInCompleting || read.ID != s.ID {
			h.t.Fatalf("MCPSignIn = (%+v, %v, %v)", read, found, err)
		}
		listed, err := h.f.ListMCPSignIns(h.ctx)
		if err != nil || len(listed) != 1 || listed[0].ID != s.ID {
			h.t.Fatalf("ListMCPSignIns = (%+v, %v)", listed, err)
		}
		if gone, err := h.f.DeleteMCPSignIn(h.ctx, "linear", read.Version); err != nil || !gone {
			h.t.Fatalf("DeleteMCPSignIn = (%v, %v)", gone, err)
		}
		unsealed := signInOf("linear", h.now())
		unsealed.Sealed = ""
		if _, _, err := h.f.CreateMCPSignIn(h.ctx, unsealed); err == nil {
			h.t.Fatal("a pending sign-in with nothing to finish it with was written")
		}
	},
}}
