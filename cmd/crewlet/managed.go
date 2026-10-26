package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
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
// What stays open is what changes no byte of the company: `config rekey`
// re-seals the revision this node boots on under the keyring, which is Tier
// A's own business and nothing the managing system can do, and `config
// activate` of the company's own revision — the one this node knows the fleet
// to run ([store.Configs.Restorable]) — which is the offline re-publish a
// rotated credential needs. Any other revision, the one this node boots on
// while it refuses the fleet's included, is a change of document.

// managedRefusal is what a host-side write of a managed document is told:
// who manages it, why the write is refused, and the two ways forward.
func managedRefusal(boot *config.Bootstrap, gesture string) error {
	return fmt.Errorf("the company config is managed by another system "+
		"(`api.auth.company_writers`: %s), so %s can't change it here.\n"+
		"Change it there, or run `crewlet config import -api <node>` with %s "+
		"set to one of those tokens",
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
// the company's own on a managed document.
func refuseManagedActivation(ctx context.Context, cs *configStore, revisionID string) error {
	if !cs.boot.API.Auth.CompanyManaged() {
		return nil
	}
	known, found, err := cs.configs.Restorable(ctx)
	if err != nil {
		return fmt.Errorf("read the company this node knows: %w", err)
	}
	if found && revisionID == known.ID {
		return nil
	}
	return managedRefusal(cs.boot, "`crewlet config activate` of a revision that is not the company's own")
}

// refuseManagedOverride refuses `-import-company` on a managed deployment.
//
// REFUSED RATHER THAN IGNORED, unlike a `-company` seed: it is an explicit
// instruction to replace what the fleet is running, and a node that started
// without doing it would read as one that had.
func refuseManagedOverride(boot *config.Bootstrap, path string) error {
	return refuseManagedOffline(boot, "-import-company "+path)
}

// managedSeed is the seed a managed deployment's command line carries, decided
// as it is resolved and before anything reads the file.
//
// `-import-company` is refused ([refuseManagedOverride]), its file unread. A
// `-company` bootstrap seed is NEVER READ: on a managed deployment it is
// neither imported nor run, so there is nothing to read it for, and reading it
// anyway is how a stray ./company.yaml the node was never told about — or one
// that no longer parses — would fail the boot of a node that ignores it. What
// comes back names the file when there is one to ignore (a named path, or the
// default where it exists), for [decideSeed] to say so; an absent default is
// no seed at all.
//
// IGNORED, LOUDLY, rather than refused: `-company` defaults to
// ./company.yaml, so a node can carry one it was never told about, and a
// bootstrap seed is ignored wherever a company exists anyway — this is the same
// answer for the same reason, said at the same level. An empty store on a
// managed deployment waits for its managing system's first write.
func managedSeed(boot *config.Bootstrap, path string, named bool, importPath string) (tierBSeed, error) {
	if importPath != "" {
		return tierBSeed{}, refuseManagedOverride(boot, importPath)
	}
	if !named {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return tierBSeed{Path: path}, nil
		}
	}
	return tierBSeed{Path: path, Managed: true}, nil
}

// warnManagedSeed says that a managed deployment ignored a -company seed.
//
// Said by [decideSeed] once its decision holds, rather than where the seed is
// resolved: that is before the log file is open and before Tier A's last
// refusals, and a warning printed beside the refusal that stops the boot — a
// store that cannot be read, a stored revision this build cannot run — is
// noise at the moment somebody is reading carefully.
func warnManagedSeed(ctx context.Context, boot *config.Bootstrap, path string, log *slog.Logger) {
	log.WarnContext(ctx, "company_seed_ignored",
		"file", path,
		"reason", "api.auth.company_writers names the company document's only "+
			"writers ("+strings.Join(boot.API.Auth.CompanyWriters, ", ")+"), so a "+
			"Tier B file on this node's command line is neither imported nor run, "+
			"and is not read",
		"hint", "the managing system writes the company through the API; remove "+
			"the file or the -company flag, or remove api.auth.company_writers "+
			"to take the document back")
}
