package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/configplane"
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
	if err := seedCompany(ctx, db, seed, cipher, log); err != nil {
		return nil, err
	}

	nodeID, err := config.ResolveNodeID(boot, nil)
	if err != nil {
		return nil, fmt.Errorf("node identity: %w", err)
	}
	reconciler, err := e.NewReconciler(engine.ReconcilerOptions{
		Store: db, NodeID: nodeID, Cipher: cipher,
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
func seedCompany(ctx context.Context, db *store.DB, seed *config.Company,
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
		current, err := secrets.Open(cipher, active.Payload)
		if err != nil {
			return fmt.Errorf("open the active revision: %w", err)
		}
		if bytes.Equal(current, document) {
			return nil
		}
		parent = active.ID
	}

	payload, err := secrets.Seal(cipher, document)
	if err != nil {
		return err
	}
	summary := "seeded from " + seed.Name
	id, epoch, err := configs.InsertActive(ctx, store.Revision{
		ParentID: parent, Source: "file", CreatedBy: "node",
		Summary: summary, Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("seed the company config: %w", err)
	}
	log.Info("company_config_seeded", "revision", id, "epoch", epoch,
		"parent", parent, "sealed", cipher != nil)
	return nil
}
