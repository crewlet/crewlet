package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// The company config a running node serves comes from the STORE, not from the
// file `-company` names. The file is a SEED, reconciled into the store like
// any other change.
//
// Both halves of that are load-bearing. Without the seed, a first run has
// nothing to activate and the node serves a company no peer can see. Without
// the store, a PUT /config on one node is invisible to every other — which is
// the fan-out failure the whole control plane exists to remove.
//
// The seed is compared against the ACTIVE revision's opened document, not
// against its stored bytes: with a keyring configured the stored form is
// ciphertext and differs on every seal, so a byte comparison would import a
// fresh revision on every single boot.

// startReconciler seeds the store from the file, converges this node on the
// pointer, and returns the loop for the caller to run.
func startReconciler(ctx context.Context, e *engine.Engine, boot *config.Bootstrap,
	seed *config.Company, cipher secrets.Cipher, log *slog.Logger,
) (*engine.Reconciler, error) {
	db := e.Backends().Store
	plane := e.Backends().Fleet
	if err := seedCompany(ctx, db, plane, seed, cipher, log); err != nil {
		return nil, err
	}

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("node identity: %w", err)
	}
	reconciler, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: db, Fleet: plane, Queue: e.Backends().Queue,
		NodeID: nodeID, Cipher: cipher,
		OnApply: func(epoch int64, status configplane.ApplyStatus) {
			log.Info("config_revision_applied",
				"epoch", epoch, "status", string(status))
		},
	})
	if err != nil {
		return nil, err
	}
	// One tick now, so the node boots on the epoch the fleet is on. Its
	// failure is NOT fatal: a node that cannot reach the current revision
	// still serves the one it has, which is the whole point of publishing
	// an epoch rather than mutating one.
	if err := reconciler.Tick(ctx); err != nil {
		log.Warn("initial_reconcile_failed", "error", err,
			"hint", "this node is serving the configuration it booted with; "+
				"it will keep trying on its reconcile interval")
	}
	return reconciler, nil
}

// seedCompany imports the file as a revision when the store does not already
// hold it.
//
// Idempotent by CONTENT, not by a marker: a node that boots ten times with an
// unchanged file imports nothing, and one whose file an operator edited
// imports once. Silently ignoring the edited file instead would be the worst
// of the three — an operator changes a config, restarts, and nothing happens,
// with nothing anywhere saying why.
func seedCompany(ctx context.Context, db *store.DB, plane coord.Plane, seed *config.Company,
	cipher secrets.Cipher, log *slog.Logger,
) error {
	document, err := json.Marshal(seed)
	if err != nil {
		return fmt.Errorf("encode the company config: %w", err)
	}
	configs := db.Configs()
	active, found, err := configs.Active(ctx)
	if err != nil {
		return fmt.Errorf("read the active revision: %w", err)
	}
	parent := ""
	if found {
		//nolint:govet // shadow: scoped to this block; see .golangci.yml
		current, err := secrets.Open(cipher, active.Payload)
		if err != nil {
			return fmt.Errorf("open the active revision: %w", err)
		}
		if bytes.Equal(current, document) {
			// The file has not changed, so there is nothing to import.
			// The node may still owe the fleet a POINTER — see
			// publishLocalActive for the case that puts it there.
			return publishLocalActive(ctx, plane, active, log)
		}
		parent = active.ID
	}

	payload, err := secrets.Seal(cipher, document)
	if err != nil {
		return err
	}
	summary := "seeded from " + seed.Name
	// STORED FIRST, then pointed at: a crash between the two leaves a
	// revision nothing points at, which the next boot re-seeds over. The
	// other order would point the fleet at a payload no node can read.
	id, err := configs.InsertActive(ctx, store.Revision{
		ParentID: parent, Source: "file", CreatedBy: "node",
		Summary: summary, Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	published, err := plane.Activate(ctx, id, summary, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("activate the seeded company config: %w", err)
	}
	log.Info("company_config_seeded", "revision", id, "epoch", published.Epoch,
		"parent", parent, "sealed", cipher != nil)
	return nil
}

// publishLocalActive points the fleet at this node's active revision when the
// fleet has no pointer, or an OLDER one.
//
// This is what makes an offline `crewlet config import` reach the fleet. The
// activation pointer lives in the coordination store, and on the default
// embedded topology that store is inside the engine's own process — so a
// command run while the engine is stopped cannot move it. It marks the
// revision active in the node's own database instead, and this publishes it
// at the next start.
//
// # The pointer wins when it is NEWER
//
// A node booting into a running fleet must converge on what the fleet already
// decided, not overwrite it with whatever its local database happens to say —
// that would let one restarted node roll a company back. So the local
// revision is published only when the fleet has nothing, or when it was
// activated AFTER the pointer was.
//
// Both instants are written by the same node in the offline case, which is
// the case this exists for. Across a fleet they come from different clocks,
// and the consequence is bounded and stated: a node whose clock is ahead can
// republish its own revision once, which every peer then converges on — the
// same outcome as an operator re-activating it deliberately.
func publishLocalActive(ctx context.Context, plane coord.Plane, active store.Revision, log *slog.Logger) error {
	target, found, err := plane.Target(ctx)
	if err != nil {
		return fmt.Errorf("read the activation pointer: %w", err)
	}
	switch {
	case found && target.RevisionID == active.ID:
		return nil
	case found && !active.ActivatedAt.After(target.At):
		// The fleet is on something else and decided it later. Converging
		// on the pointer is the reconciler's job, not this function's.
		log.Info("local_revision_superseded", "local", active.ID,
			"fleet", target.RevisionID, "epoch", target.Epoch,
			"detail", "this node converges on the fleet's revision")
		return nil
	}
	published, err := plane.Activate(ctx, active.ID, active.Summary, active.ActivatedAt)
	if err != nil {
		return fmt.Errorf("publish the active revision: %w", err)
	}
	log.Info("local_revision_published", "revision", active.ID, "epoch", published.Epoch)
	return nil
}
