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
// The API refuses a change from every credential `api.auth.company_writers`
// does not list. Commands that write the document WITHOUT the API (an offline
// `crewlet config import` or `activate`, a Tier B file on `crewlet run`) carry
// no credential, so they read the same Tier A and honour it: a revision
// written here would be replaced at the next reconcile.
//
// What stays open changes no byte of the company: `config rekey`, and `config
// activate` of the company's own revision, the one the fleet runs
// ([store.Configs.Restorable]), which is the offline re-publish a rotated
// credential needs. Any other revision is a change of document.

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
// `-company` bootstrap seed is NEVER READ: it is neither imported nor run, and
// reading it would let a stray or unparseable ./company.yaml fail the boot of
// a node that ignores it. The result names the file when there is one to
// ignore, for [decideSeed] to say so.
//
// IGNORED, LOUDLY, rather than refused: `-company` defaults to ./company.yaml,
// and a seed is ignored wherever a company exists anyway. An empty store waits
// for the managing system's first write.
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
