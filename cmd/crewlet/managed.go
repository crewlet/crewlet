package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
)

// A MANAGED COMPANY DOCUMENT, from the host's side — ADR-0030.
//
// The API refuses a change to a managed document from every credential that
// `api.auth.company_writers` does not list. The commands that write the
// document WITHOUT the API — an offline `crewlet config import` or `activate`,
// and a Tier B file on `crewlet run`'s command line — present no credential
// at all, so no writer list can name them. They read the same Tier A that
// declares the document managed, and they honour it: the document's source is
// the managing system, and a revision written here would be replaced at its
// next reconcile exactly as an API edit would.
//
// What stays open is what changes no byte of the company: `config seal` and
// `config rekey` re-encrypt the active document under the keyring, which is
// Tier A's own business and nothing the managing system can do, and
// `config activate` of the revision that is already active, which is the
// offline re-publish a rotated credential needs.

// managedRefusal is what a host-side write of a managed document is told:
// who manages it, why the write is refused, and the two ways forward.
func managedRefusal(boot *config.Bootstrap, gesture string) error {
	return fmt.Errorf("the company document is managed: api.auth.company_writers "+
		"names %s as its only writers, and %s presents no credential, so it would "+
		"write a revision the managing system replaces at its next reconcile.\n"+
		"Change the company where it is managed. To write it as one of those "+
		"writers, run `crewlet config import -api <node>` with %s set to that "+
		"token; to take the document back, remove api.auth.company_writers from "+
		"every node's Tier A and restart",
		strings.Join(boot.API.Auth.CompanyWriters, ", "), gesture, apiTokenEnv)
}

// refuseManagedOffline refuses an offline write onto a managed document.
func refuseManagedOffline(boot *config.Bootstrap, gesture string) error {
	if !boot.API.Auth.CompanyManaged() {
		return nil
	}
	return managedRefusal(boot, gesture)
}

// refuseManagedActivation refuses an offline activation of any revision but
// the active one on a managed document.
func refuseManagedActivation(ctx context.Context, cs *configStore, revisionID string) error {
	if !cs.boot.API.Auth.CompanyManaged() {
		return nil
	}
	active, found, err := cs.configs.Active(ctx)
	if err != nil {
		return fmt.Errorf("read the active revision: %w", err)
	}
	if found && revisionID == active.ID {
		return nil
	}
	return managedRefusal(cs.boot, "`crewlet config activate` of a revision that is not the active one")
}

// refuseManagedOverride refuses `-import-company` on a managed deployment.
//
// REFUSED RATHER THAN IGNORED, unlike a `-company` seed: it is an explicit
// instruction to replace what the fleet is running, and a node that started
// without doing it would read as one that had.
func refuseManagedOverride(boot *config.Bootstrap, path string) error {
	return refuseManagedOffline(boot, "-import-company "+path)
}

// managedSeed drops a `-company` bootstrap seed on a managed deployment, and
// says so.
//
// IGNORED, LOUDLY, rather than refused: `-company` defaults to
// ./company.yaml, so a node can carry one it was never told about, and a
// bootstrap seed is ignored once a company exists anyway — this is the same
// answer for the same reason, said at the same level. An empty store on a
// managed deployment waits for its managing system's first write.
func managedSeed(ctx context.Context, boot *config.Bootstrap, seed tierBSeed, log *slog.Logger) tierBSeed {
	if seed.Company == nil || !boot.API.Auth.CompanyManaged() {
		return seed
	}
	log.WarnContext(ctx, "company_seed_ignored",
		"file", seed.Path,
		"reason", "api.auth.company_writers names the company document's only "+
			"writers ("+strings.Join(boot.API.Auth.CompanyWriters, ", ")+"), so a "+
			"Tier B file on this node's command line is never imported",
		"hint", "the managing system writes the company through the API; remove "+
			"the file, or remove api.auth.company_writers to take the document back")
	return tierBSeed{Path: seed.Path}
}
