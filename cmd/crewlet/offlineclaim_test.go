package main

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// AN OFFLINE CHANGE REACHES THE FLEET AT THE NEXT START, as every offline
// command promises ("This node will publish it to the fleet at its next
// start"). The boot publishes this node's active revision when it was
// activated after the fleet's pointer, so what an offline command marks
// active has to be dated after the activation it replaces, and an offline
// re-activation of the revision the fleet already runs is still an
// activation to publish.

// offlineStore is the store the offline commands open, over db, under the
// fixture keyring.
func offlineStore(db *store.DB) *configStore {
	return offlineStoreUnder(db, fixtureCipher, "fixture")
}

// offlineStoreUnder is [offlineStore] under a keyring of the case's own, and
// the key it seals under.
func offlineStoreUnder(db *store.DB, cipher secrets.Cipher, activeKeyID string) *configStore {
	boot := config.DefaultBootstrap()
	return &configStore{configs: db.Configs(), cipher: cipher,
		activeKeyID: activeKeyID, boot: &boot}
}

// rotatedCipher is the fixture keyring halfway through a rotation: the
// fixture key still held, and a new one sealing.
func rotatedCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	next := make([]byte, 32)
	copy(next, "next")
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "next", Keys: map[string][]byte{"fixture": fixtureKey, "next": next},
	})
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

// RE-ACTIVATING THE RUNNING REVISION OFFLINE MINTS AN EPOCH AT THE NEXT START.
//
// It is the offline twin of POST /config/reload, the gesture a rotated
// credential needs: the pointer is append-only so that "the same revision,
// resolved again" is an activation every node applies. The boot returned
// early whenever the fleet's pointer named the revision this node held, so
// the re-activation never left this node and its peers went on running the
// credential it had rotated.
//
// The control is a restart with nothing done offline: no new epoch.
func TestAnOfflineReactivationOfTheRunningRevisionIsPublished(t *testing.T) {
	t.Parallel()
	for _, reactivated := range []bool{false, true} {
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
			fixtureCipher, testNode, quiet()); err != nil {
			t.Fatalf("first start: %v", err)
		}
		before, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if reactivated {
			if err := activateRevision(t.Context(), offlineStore(db), before.RevisionID,
				io.Discard); err != nil {
				t.Fatalf("crewlet config activate %s: %v", before.RevisionID, err)
			}
		}
		if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{}, fixtureCipher,
			testNode, quiet()); err != nil {
			t.Fatalf("restart: %v", err)
		}
		after, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case after.RevisionID != before.RevisionID:
			t.Errorf("reactivated=%v: the restart pointed the fleet at %s, want %s",
				reactivated, after.RevisionID, before.RevisionID)
		case reactivated && after.Epoch == before.Epoch:
			t.Error("an offline re-activation of the running revision was not published: " +
				"the fleet's epoch did not move, so no node resolves its credentials again")
		case !reactivated && after.Epoch != before.Epoch:
			t.Errorf("a restart with nothing done offline moved the fleet from epoch %d to %d",
				before.Epoch, after.Epoch)
		}
	}
}

// AN OFFLINE IMPORT IS PUBLISHED WHATEVER THIS MACHINE'S CLOCK SAYS.
//
// This node's copy of the fleet's revision carries the instant the fleet
// activated it at, from whichever node's clock made that activation. An
// import dated by this machine's clock alone, behind that one, read at the
// next start as older than the fleet's pointer, and the boot dropped it with
// an info line while the import had told its operator it would be published.
// The same holds for an offline rekey, which writes a revision the same way.
func TestAnOfflineImportOnASlowClockIsPublished(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"import", "rekey"} {
		db := seedStore(t)
		fleet := coordmemory.NewFleet()
		if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
			fixtureCipher, testNode, quiet()); err != nil {
			t.Fatalf("%s: first start: %v", command, err)
		}
		// THE FLEET'S NEXT ACTIVATION, made on a node whose clock runs a
		// day ahead, and this node's copy of it as its reconciler keeps one.
		ahead := time.Now().UTC().Add(24 * time.Hour)
		payload, err := secrets.Seal(fixtureCipher, []byte(`{"name":"Ahead"}`))
		if err != nil {
			t.Fatal(err)
		}
		origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
			Source: "api", CreatedAt: ahead}
		published, err := fleet.Activate(t.Context(), coord.ActivationRequest{
			RevisionID: "written-ahead", Summary: "from a node a day ahead",
			Payload: payload, At: ahead, Origin: origin,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Configs().Adopt(t.Context(), store.Revision{
			ID: published.RevisionID, CreatedBy: origin.Author, CreatedByKind: origin.AuthorKind,
			Source: origin.Source, Summary: published.Summary, Payload: payload,
			CreatedAt: origin.CreatedAt, ActivatedAt: published.At,
		}); err != nil {
			t.Fatalf("%s: this node's copy of the fleet's revision: %v", command, err)
		}

		cipher := fixtureCipher
		switch command {
		case "import":
			edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Imported", 1)
			path := filepath.Join(t.TempDir(), "company.yaml")
			if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := importConfig(t.Context(), offlineStore(db), path, parse(t, edited),
				"imported offline", io.Discard); err != nil {
				t.Fatalf("crewlet config import: %v", err)
			}
		case "rekey":
			cipher = rotatedCipher(t)
			if err := rekeyConfig(t.Context(), offlineStoreUnder(db, cipher, "next"), false,
				io.Discard); err != nil {
				t.Fatalf("crewlet config rekey: %v", err)
			}
		}
		claimed, found, err := db.Configs().Active(t.Context())
		if err != nil || !found || claimed.ID == published.RevisionID {
			t.Fatalf("%s: the offline command marked nothing new active (found=%v err=%v)",
				command, found, err)
		}

		if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{}, cipher,
			testNode, quiet()); err != nil {
			t.Fatalf("%s: restart: %v", command, err)
		}
		after, _, err := fleet.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if after.RevisionID != claimed.ID {
			t.Errorf("%s: the restart left the fleet on %s, want the offline revision %s published",
				command, after.RevisionID, claimed.ID)
		}
	}
}

// AN OFFLINE CHANGE THE FLEET OVERTOOK IS A WARNING, naming it: the offline
// command told whoever ran it that the next start would publish it, and the
// later activation wins, so a fleet that activated something after the command
// keeps it off the fleet. That was an info line, which nobody watching for the
// change they made would see. Checked to fail at INFO.
func TestAnOfflineChangeTheFleetOvertookIsAWarning(t *testing.T) {
	t.Parallel()
	db := seedStore(t)
	fleet := coordmemory.NewFleet()
	if err := seedCompany(t.Context(), db, fleet, nil, seedOf(parse(t, companyYAML)),
		fixtureCipher, testNode, quiet()); err != nil {
		t.Fatalf("first start: %v", err)
	}
	edited := strings.Replace(companyYAML, "name: Acme", "name: Acme Imported", 1)
	path := filepath.Join(t.TempDir(), "company.yaml")
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := importConfig(t.Context(), offlineStore(db), path, parse(t, edited),
		"imported offline", io.Discard); err != nil {
		t.Fatalf("crewlet config import: %v", err)
	}
	claimed, _, err := db.Configs().Active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// THE FLEET ACTIVATES AFTER THE COMMAND, on another node.
	payload, err := secrets.Seal(fixtureCipher, []byte(`{"name":"Later"}`))
	if err != nil {
		t.Fatal(err)
	}
	later := claimed.ActivatedAt.Add(time.Minute)
	if _, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "written-later", Summary: "after the offline import",
		Payload: payload, At: later,
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorOperator,
			Source: "api", CreatedAt: later},
	}); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if err := seedCompany(t.Context(), db, fleet, nil, tierBSeed{}, fixtureCipher,
		testNode, logger); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if after, _, err := fleet.Target(t.Context()); err != nil || after.RevisionID != "written-later" {
		t.Fatalf("the fleet after the restart is on %s (err %v), want its later activation",
			after.RevisionID, err)
	}
	line := ""
	for _, l := range strings.Split(logged.String(), "\n") {
		if strings.Contains(l, "local_revision_superseded") {
			line = l
		}
	}
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, claimed.ID) {
		t.Errorf("the dropped offline revision %s was not warned about: %q", claimed.ID, line)
	}
}
