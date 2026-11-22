package coordtest

import (
	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the sealed credentials -------------------------------------------- //

func (h *fleetHarness) putSecret(name, sealed, keyID string) {
	h.t.Helper()
	err := h.f.PutSecret(h.ctx, coord.SecretRecord{
		Name: name, Value: sealed, KeyID: keyID,
		UpdatedAt: h.now(), UpdatedBy: "jane.doe", UpdatedByKind: "operator",
		OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a", Source: "cli",
	})
	if err != nil {
		h.t.Fatalf("PutSecret(%s): %v", name, err)
	}
}

func (h *fleetHarness) secret(name string) (coord.SecretRecord, bool) {
	h.t.Helper()
	rec, found, err := h.f.Secret(h.ctx, name)
	if err != nil {
		h.t.Fatalf("Secret(%s): %v", name, err)
	}
	return rec, found
}

func (h *fleetHarness) secretNames() []string {
	h.t.Helper()
	rows, err := h.f.SecretValues(h.ctx)
	if err != nil {
		h.t.Fatalf("SecretValues: %v", err)
	}
	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}
	return names
}

var secretCases = []fleetCase{{
	// THE REASON THIS MOVED. A rotation reached the one node whose Tier A
	// file the CLI was pointed at; every other node kept what it booted
	// with, and nothing failed until a seat landed on one of them and the
	// vendor rejected a credential the operator believed they had replaced.
	name: "a credential one node stored is readable by every other",
	fn: func(h *fleetHarness) {
		h.putSecret("SLACK_BOT_TOKEN", "v1:sealed-envelope", "key-1")
		rec, found := h.secret("SLACK_BOT_TOKEN")
		if !found {
			h.t.Fatal("a peer cannot see the credential — the rotation half-landed")
		}
		if rec.Value != "v1:sealed-envelope" {
			h.t.Errorf("value = %q, want the envelope exactly as stored", rec.Value)
		}
		if rec.KeyID != "key-1" {
			h.t.Errorf("KeyID = %q — a rotation sweep cannot find stale rows without it", rec.KeyID)
		}
		// ALL FOUR, and the credential beside the author above all: a
		// store that kept only one of the two names lost whichever it
		// dropped — the author once the credential's row was swept, or
		// the credential for good.
		if rec.UpdatedBy != "jane.doe" || rec.UpdatedByKind != "operator" ||
			rec.OperatorID != "pat:0192f00d-0000-7000-8000-00000000000a" ||
			rec.Source != "cli" {
			h.t.Errorf("provenance lost: by=%q kind=%q operator=%q source=%q",
				rec.UpdatedBy, rec.UpdatedByKind, rec.OperatorID, rec.Source)
		}
	},
}, {
	// THE WRITE-ONLY MARK IS A FACT OF THE ROW, kept by every write a store
	// takes: a backend that dropped it would hand a value stored never to
	// be read back to the next reveal, and nothing would say so.
	name: "a write-only row stays marked through a put, a create and an update",
	fn: func(h *fleetHarness) {
		marked := coord.SecretRecord{Name: "MCP_LINEAR_AUTHORIZATION", Value: "v1:sealed",
			KeyID: "key-1", UpdatedAt: h.now(), Source: "mcp", WriteOnly: true}
		if err := h.f.PutSecret(h.ctx, marked); err != nil {
			h.t.Fatalf("PutSecret: %v", err)
		}
		if rec, _ := h.secret(marked.Name); !rec.WriteOnly {
			h.t.Fatal("a put dropped the write-only mark")
		}
		rows, err := h.f.SecretValues(h.ctx)
		if err != nil {
			h.t.Fatalf("SecretValues: %v", err)
		}
		for _, row := range rows {
			if row.Name == marked.Name && !row.WriteOnly {
				h.t.Error("the listing dropped the write-only mark")
			}
		}
		current, _ := h.secret(marked.Name)
		resealed := marked
		resealed.Value, resealed.KeyID = "v2:resealed", "key-2"
		if wrote, err := h.f.UpdateSecret(h.ctx, resealed, current.Version); err != nil || !wrote {
			h.t.Fatalf("UpdateSecret = (%v, %v)", wrote, err)
		}
		if rec, _ := h.secret(marked.Name); !rec.WriteOnly || rec.KeyID != "key-2" {
			h.t.Errorf("an update dropped the mark or the key: %+v", rec)
		}
		created := marked
		created.Name = "MCP_SENTRY_AUTHORIZATION"
		if ok, err := h.f.CreateSecret(h.ctx, created); err != nil || !ok {
			h.t.Fatalf("CreateSecret = (%v, %v)", ok, err)
		}
		if rec, _ := h.secret(created.Name); !rec.WriteOnly {
			h.t.Error("a create dropped the write-only mark")
		}
		h.putSecret("PLAIN_TOKEN", "v1:sealed", "key-1")
		if rec, _ := h.secret("PLAIN_TOKEN"); rec.WriteOnly {
			h.t.Error("a row stored without the mark reads as write-only")
		}
	},
}, {
	// The store holds an ENVELOPE and has no key. Anything that mangled
	// the bytes would produce a value that decrypts to nothing on the far
	// side — an auth failure attributed to the vendor rather than to the
	// store that corrupted it.
	name: "the sealed bytes come back byte-identical",
	fn: func(h *fleetHarness) {
		const envelope = `{"k":"key-1","n":"YWJj","c":"ZGVmZ2hpamts+/=="}`
		h.putSecret("WEBHOOK_SECRET", envelope, "key-1")
		rec, found := h.secret("WEBHOOK_SECRET")
		if !found {
			h.t.Fatal("stored and then absent")
		}
		if rec.Value != envelope {
			h.t.Fatalf("value = %q, want %q — the envelope was altered in transit",
				rec.Value, envelope)
		}
	},
}, {
	// ROTATION IS THE COMMON PATH and it is last-write-wins by contract:
	// two operators rotating at once must leave the store holding one of
	// their values, never a failure one of them has to notice and retry.
	name: "a rotation replaces the prior value",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		h.putSecret("GITLAB_TOKEN", "v2:new", "key-2")
		rec, found := h.secret("GITLAB_TOKEN")
		if !found {
			h.t.Fatal("the rotation removed the credential")
		}
		if rec.Value != "v2:new" || rec.KeyID != "key-2" {
			h.t.Fatalf("value = %q key = %q, want the second write", rec.Value, rec.KeyID)
		}
		if names := h.secretNames(); len(names) != 1 {
			h.t.Fatalf("names = %v — a rotation added a row instead of replacing one", names)
		}
	},
}, {
	// The engine takes ONE snapshot at boot and on every apply, because
	// ${VAR} expansion happens per role, per provider, per MCP server. A
	// listing that missed a name resolves it to an empty string on every
	// node at once.
	name: "the listing is every credential, by name",
	fn: func(h *fleetHarness) {
		for _, name := range []string{"ZULU", "ALPHA", "MIKE"} {
			h.putSecret(name, "v1:"+name, "key-1")
		}
		got := h.secretNames()
		want := []string{"ALPHA", "MIKE", "ZULU"}
		if len(got) != len(want) {
			h.t.Fatalf("names = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				h.t.Fatalf("names = %v, want %v — the order differs between "+
					"backends, so a diff of two captures is unreadable", got, want)
			}
		}
	},
}, {
	// An absent credential is a THREE-VALUED answer's middle case, and it
	// has to be distinguishable from a read that failed: the first is an
	// operator who has not set it yet, the second is a store outage that
	// must never render as one.
	name: "an unset credential is absent rather than empty",
	fn: func(h *fleetHarness) {
		rec, found := h.secret("NEVER_SET")
		if found {
			h.t.Fatalf("an unset name answered with %+v", rec)
		}
	},
}, {
	name: "deleting reports whether it was there, and it stays gone",
	fn: func(h *fleetHarness) {
		h.putSecret("RETIRED", "v1:sealed", "key-1")
		removed, err := h.f.DeleteSecret(h.ctx, "RETIRED")
		if err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		if !removed {
			h.t.Fatal("deleting a stored credential reported it absent")
		}
		if _, found := h.secret("RETIRED"); found {
			h.t.Fatal("the credential survived its delete")
		}
		// AGAIN. An operator re-running the command, or two of them, must
		// get "there was nothing" rather than an error.
		again, err := h.f.DeleteSecret(h.ctx, "RETIRED")
		if err != nil {
			h.t.Fatalf("second DeleteSecret: %v", err)
		}
		if again {
			h.t.Error("a second delete claimed to have removed something")
		}
	},
}, {
	// A name is an environment-variable name and those are not restricted
	// to what a KV key permits. A backend that stored one and could not
	// list it back would drop the credential from the boot snapshot while
	// answering a direct read — the worst shape, because it works in a
	// test that looks it up by name.
	name: "an awkward name survives storage and listing",
	fn: func(h *fleetHarness) {
		const name = "acme.co/token-v2_PROD"
		h.putSecret(name, "v1:sealed", "key-1")
		if _, found := h.secret(name); !found {
			h.t.Fatal("a direct read cannot find it")
		}
		names := h.secretNames()
		if len(names) != 1 || names[0] != name {
			h.t.Fatalf("names = %v, want [%s] — the boot snapshot would miss it",
				names, name)
		}
	},
}, {
	// An unsealed value is a caller that forgot to encrypt, not a secret
	// whose value is empty. Storing it resolves as an empty ${VAR} on
	// every node — which is the failure this bucket exists to prevent, so
	// it is refused at the door rather than replicated.
	name: "an unsealed value is refused rather than replicated",
	fn: func(h *fleetHarness) {
		err := h.f.PutSecret(h.ctx, coord.SecretRecord{
			Name: "EMPTY", UpdatedAt: h.now(),
		})
		if err == nil {
			h.t.Fatal("a credential with no sealed value was accepted")
		}
		if _, found := h.secret("EMPTY"); found {
			h.t.Fatal("the refused write landed anyway")
		}
	},
}, {
	name: "a nameless credential is refused",
	fn: func(h *fleetHarness) {
		if err := h.f.PutSecret(h.ctx, coord.SecretRecord{Value: "v1:x"}); err == nil {
			h.t.Fatal("a credential with no name was accepted")
		}
		if _, _, err := h.f.Secret(h.ctx, ""); err == nil {
			h.t.Fatal("reading an empty name was accepted")
		}
		if _, err := h.f.DeleteSecret(h.ctx, ""); err == nil {
			h.t.Fatal("deleting an empty name was accepted")
		}
	},
}, {
	// A CREATE IS THE FIRST WRITER'S. Two nodes minting the company's
	// blind-index key at once each derive blinds under the key they wrote,
	// and with a plain put the store keeps one of them — so the loser's
	// blinds match nothing any other node computes. The second create must
	// be told the row is somebody else's, and must not have touched it.
	name: "a create writes only where no row is stored",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:first", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || !created {
			h.t.Fatalf("the first create = (%v, %v), want it written", created, err)
		}
		created, err = h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:second", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || created {
			h.t.Fatalf("the second create = (%v, %v), want it refused as "+
				"somebody else's row", created, err)
		}
		if rec, _ := h.secret("iam/blind-index-key"); rec.Value != "v1:first" {
			h.t.Fatalf("a refused create replaced the row: %q", rec.Value)
		}
		// AND A DELETED ROW IS ABSENT AGAIN: a create is not refused over
		// the history of a key somebody destroyed.
		if _, err = h.f.DeleteSecret(h.ctx, "iam/blind-index-key"); err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		created, err = h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "iam/blind-index-key", Value: "v1:third", KeyID: "key-1",
			UpdatedAt: h.now(),
		})
		if err != nil || !created {
			h.t.Fatalf("a create after the row was deleted = (%v, %v), want "+
				"it written", created, err)
		}
	},
}, {
	// A CREATE'S OPERATION KEY IS KEPT, AND A PLAIN WRITE CLEARS IT. The key
	// is how the same create retried finishes what it sealed (fleetsecrets
	// Store.Reclaim); a store that dropped it would refuse every retry, and
	// one that kept it across a plain write would let a stale retry re-seal
	// over a value somebody else wrote.
	name: "a create's operation key round-trips and a plain write clears it",
	fn: func(h *fleetHarness) {
		created, err := h.f.CreateSecret(h.ctx, coord.SecretRecord{
			Name: "GATEWAY_KEY", Value: "v1:first", KeyID: "key-1",
			UpdatedAt: h.now(), OpID: "op-1",
		})
		if err != nil || !created {
			h.t.Fatalf("the keyed create = (%v, %v)", created, err)
		}
		rec, _ := h.secret("GATEWAY_KEY")
		if rec.OpID != "op-1" {
			h.t.Fatalf("the create's operation key reads back as %q", rec.OpID)
		}
		h.putSecret("GATEWAY_KEY", "v1:rotated", "key-1")
		if rec, _ = h.secret("GATEWAY_KEY"); rec.OpID != "" {
			h.t.Fatalf("a plain write left the operation key %q", rec.OpID)
		}
	},
}, {
	// A VERSION IS READ, AND EVERY WRITE MOVES IT. What a conditional write
	// is conditioned on has to be on what a read hands back, and a write
	// that left it where it was would let a caller holding the old one win
	// against a row it never saw.
	name: "every read carries the row's version and every write moves it",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		first, _ := h.secret("GITLAB_TOKEN")
		if first.Version == 0 {
			h.t.Fatal("a stored row reads back with no version, so nothing " +
				"can be conditioned on it")
		}
		h.putSecret("GITLAB_TOKEN", "v2:new", "key-1")
		second, _ := h.secret("GITLAB_TOKEN")
		if second.Version == first.Version {
			h.t.Fatalf("a rotation left the version at %d", first.Version)
		}
		rows, err := h.f.SecretValues(h.ctx)
		if err != nil || len(rows) != 1 || rows[0].Version != second.Version {
			h.t.Fatalf("the listing = (%+v, %v), want the row at version %d",
				rows, err, second.Version)
		}
	},
}, {
	// AN UPDATE IS REFUSED OVER A ROW THAT MOVED, AND NEVER RESURRECTS ONE.
	// A rekey re-seals the value it READ: written over an operator's
	// rotation it silently reinstates the credential they replaced, and
	// written after an operator deleted a credential it brings it back.
	// Both are an update at a version the row no longer holds.
	name: "an update writes only while the row is at the version read",
	fn: func(h *fleetHarness) {
		h.putSecret("GITLAB_TOKEN", "v1:old", "key-1")
		read, _ := h.secret("GITLAB_TOKEN")
		h.putSecret("GITLAB_TOKEN", "v2:rotated", "key-1")
		wrote, err := h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v3:resealed-old", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, read.Version)
		if err != nil || wrote {
			h.t.Fatalf("an update at a version the row moved past = (%v, %v), "+
				"want it refused", wrote, err)
		}
		if rec, _ := h.secret("GITLAB_TOKEN"); rec.Value != "v2:rotated" {
			h.t.Fatalf("the refused update replaced the rotation: %q", rec.Value)
		}
		current, _ := h.secret("GITLAB_TOKEN")
		wrote, err = h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v3:resealed", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, current.Version)
		if err != nil || !wrote {
			h.t.Fatalf("an update at the current version = (%v, %v), want it "+
				"written", wrote, err)
		}
		// GONE IS NOT A VERSION.
		gone, _ := h.secret("GITLAB_TOKEN")
		if _, err = h.f.DeleteSecret(h.ctx, "GITLAB_TOKEN"); err != nil {
			h.t.Fatalf("DeleteSecret: %v", err)
		}
		wrote, err = h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v4:resurrected", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, gone.Version)
		if err != nil || wrote {
			h.t.Fatalf("an update of a deleted row = (%v, %v), want it refused", wrote, err)
		}
		if _, found := h.secret("GITLAB_TOKEN"); found {
			h.t.Fatal("an update brought a deleted row back")
		}
		// NOR IS ZERO: a caller that never read one has judged nothing.
		if wrote, err := h.f.UpdateSecret(h.ctx, coord.SecretRecord{
			Name: "GITLAB_TOKEN", Value: "v5:blind", KeyID: "key-2",
			UpdatedAt: h.now(),
		}, 0); err != nil || wrote {
			h.t.Fatalf("an update at version zero = (%v, %v), want nothing written",
				wrote, err)
		}
		if _, found := h.secret("GITLAB_TOKEN"); found {
			h.t.Fatal("an update at version zero created the row")
		}
	},
}}
