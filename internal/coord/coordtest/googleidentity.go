package coordtest

import (
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- Google growth identities -------------------------------------------- //

func identityOf(handle string, at time.Time) coord.GoogleIdentity {
	return coord.GoogleIdentity{
		Handle: handle, UniqueID: "112233445566778899001",
		Email:     "crewlet-" + handle + "-1a2b3c@acme-crewlet-id.iam.gserviceaccount.com",
		Delegated: true, CreatedAt: at,
	}
}

var googleIdentityCases = []fleetCase{{
	// ONE IDENTITY PER SEAT, REWRITTEN ONLY AT THE VERSION READ: two passes
	// that both found no account must not both record one, and a disable
	// must not write over a pass that made the account again.
	name: "a Google identity is created once and rewritten or removed at its version",
	fn: func(h *fleetHarness) {
		g := identityOf("growth-lead", h.now())
		stored, created, err := h.f.CreateGoogleIdentity(h.ctx, g)
		if err != nil || !created || stored.Version == 0 || stored.UniqueID != g.UniqueID ||
			!stored.CreatedAt.Equal(h.now()) || !stored.Usable() {
			h.t.Fatalf("CreateGoogleIdentity = (%+v, %v, %v)", stored, created, err)
		}
		if _, again, rerr := h.f.CreateGoogleIdentity(h.ctx, identityOf("growth-lead", h.now())); rerr != nil || again {
			h.t.Fatalf("a second identity for one seat was created: (%v, %v)", again, rerr)
		}
		off := stored
		off.Disabled = true
		moved, ok, err := h.f.UpdateGoogleIdentity(h.ctx, off)
		if err != nil || !ok || moved.Usable() || moved.Version == stored.Version {
			h.t.Fatalf("UpdateGoogleIdentity = (%+v, %v, %v)", moved, ok, err)
		}
		if _, ok, rerr := h.f.UpdateGoogleIdentity(h.ctx, stored); rerr != nil || ok {
			h.t.Fatalf("an update at a version that moved wrote: (%v, %v)", ok, rerr)
		}
		if _, _, cerr := h.f.CreateGoogleIdentity(h.ctx, identityOf("mobile-lead", h.now())); cerr != nil {
			h.t.Fatalf("CreateGoogleIdentity: %v", cerr)
		}
		all, err := h.f.ListGoogleIdentities(h.ctx)
		if err != nil || len(all) != 2 || all[0].Handle != "growth-lead" || all[1].Handle != "mobile-lead" {
			h.t.Fatalf("ListGoogleIdentities = (%+v, %v), want both seats by handle", all, err)
		}
		if gone, derr := h.f.DeleteGoogleIdentity(h.ctx, "growth-lead", stored.Version); derr != nil || gone {
			h.t.Fatalf("a removal at a version that moved removed: (%v, %v)", gone, derr)
		}
		if gone, derr := h.f.DeleteGoogleIdentity(h.ctx, "growth-lead", moved.Version); derr != nil || !gone {
			h.t.Fatalf("DeleteGoogleIdentity = (%v, %v)", gone, derr)
		}
		if _, found, rerr := h.f.GoogleIdentity(h.ctx, "growth-lead"); rerr != nil || found {
			h.t.Fatalf("a removed identity reads found=%t (%v)", found, rerr)
		}
		bad := identityOf("growth-lead", h.now())
		bad.Email = ""
		if _, _, berr := h.f.CreateGoogleIdentity(h.ctx, bad); berr == nil {
			h.t.Fatal("an identity with no email was written")
		}
		intent := identityOf("ops-lead", h.now())
		intent.UniqueID, intent.Delegated = "", false
		stored, _, err = h.f.CreateGoogleIdentity(h.ctx, intent)
		if err != nil || stored.Made() || stored.Usable() {
			h.t.Fatalf("an intent to make an account = (%+v, %v), want one not yet made", stored, err)
		}
	},
}}
