package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/objstore/migrate"
	"github.com/crewlet/crewlet/internal/objstore/references"
	"github.com/crewlet/crewlet/internal/objstore/s3obj"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
)

// MOVING THE FLEET'S FILES TO AN S3 BUCKET: `crewlet objects migrate --to s3`.
//
// # Two passes, and why the second needs the fleet held still
//
// A COPY PASS runs on a live fleet, as often as an operator likes: it copies
// every object the estate names that the target does not hold, verified, and
// a pass that stopped resumes where it did ([migrate.Run]). It moves nothing:
// the fleet keeps writing to its own store throughout.
//
// The FINAL pass is what moves the fleet, and a move is safe only where no
// object can be named that the pass did not copy. So it runs on a fleet
// restarted into maintenance mode, where no node publishes a record: a file
// uploaded while it runs could never be named, because no data node writes
// one. It refuses unless this node is in that mode, no node is admitted to
// publish (coord.MaintenanceRegister's admissions, which every publishing
// data node holds), and this node's copy of the estate is current and
// complete. It then copies what is left, SEALS the broker's own bucket so a
// node not yet restarted onto the bucket has its uploads refused rather than
// stored where nobody will look, and moves the fleet's recorded object
// backend by a compare-and-set ([coord.ObjectStores]). Every node then boots
// with the target's store.objects block, and a node still naming the old
// store refuses to start.

// ErrMigrationRefused is a migration this node will not run as asked. It
// wraps a sentence saying why and what to change.
var ErrMigrationRefused = errors.New("engine: the object store migration was refused")

// ObjectMigration is one `crewlet objects migrate --to s3` request.
type ObjectMigration struct {
	// Target is an absolute path ON THIS NODE'S HOST to a Tier A file
	// whose store.objects block names the bucket the files move to: the
	// file every node runs with once the move is made. Its ${VAR}
	// references resolve from this node's environment, so a credential
	// never crosses the API.
	Target string

	// Final runs the final pass, which moves the fleet's record: see the
	// file comment for what it requires.
	Final bool
}

// ObjectMigrationReport is what a migration did.
type ObjectMigrationReport struct {
	// From and To are the fleet's object store before and after, as the
	// fleet records each.
	From string `json:"from"`
	To   string `json:"to"`

	// Final is whether this was the final pass, Sealed whether it sealed
	// the broker's bucket, and Moved whether the fleet's record now names
	// To.
	Final  bool `json:"final"`
	Sealed bool `json:"sealed"`
	Moved  bool `json:"moved"`

	// DirectUpload is what the target's boot probe found: whether the
	// fleet will offer direct uploads once it runs on the bucket.
	DirectUpload string `json:"direct_upload"`

	// Pass is what the copy found and did.
	Pass migrate.Report `json:"pass"`

	// Next is what the operator does now.
	Next string `json:"next"`
}

// sealer is a backend that can be made read-only for good: the broker's own
// bucket ([natsobj.Backend.Seal]). A provider's bucket cannot, and needs no
// seal: the maintenance window is its fence.
type sealer interface {
	Seal(ctx context.Context) error
}

// MigrateObjects runs one migration pass: see the file comment.
func (e *Engine) MigrateObjects(ctx context.Context, req ObjectMigration) (ObjectMigrationReport, error) {
	if e.objects == nil || e.backends == nil {
		return ObjectMigrationReport{}, migrationRefused("this node runs no object store")
	}
	if !filepath.IsAbs(req.Target) {
		return ObjectMigrationReport{}, migrationRefused("the target %q is not an absolute path; it "+
			"names a Tier A file on this node's host, not the caller's", req.Target)
	}
	target, err := config.LoadBootstrap(req.Target, config.NewResolver(e.environment()))
	if err != nil {
		return ObjectMigrationReport{}, migrationRefused("the target file does not load: %v", err)
	}
	objects := target.Store.Objects
	if objects.BackendOrDefault() != config.ObjectBackendS3 {
		return ObjectMigrationReport{}, migrationRefused("the target file's store.objects names the %q "+
			"backend; the files move to an S3-compatible bucket (backend: s3)",
			objects.BackendOrDefault())
	}
	report := ObjectMigrationReport{From: e.backends.objectsIdentity, To: objects.Identity(), Final: req.Final}
	if report.From == report.To {
		return report, migrationRefused("the fleet's files are already in %s", report.To)
	}
	refs, err := e.migrationReferences()
	if err != nil {
		return report, err
	}
	return e.migrateTo(ctx, req.Final, objects, refs, report)
}

// migrateTo is [Engine.MigrateObjects] once the target is loaded and the
// estate's references are in hand.
func (e *Engine) migrateTo(ctx context.Context, final bool, objects config.StoreObjects,
	refs collect.References, report ObjectMigrationReport) (ObjectMigrationReport, error) {

	if final {
		if err := e.fenced(ctx); err != nil {
			return report, err
		}
	}
	s3 := objects.S3
	bucket, err := s3obj.Open(ctx, s3obj.Config{
		Endpoint: s3.Endpoint, Region: s3.Region, Bucket: s3.Bucket, Prefix: s3.Prefix,
		PathStyle: s3.PathStyle, AccessKeyID: s3.AccessKeyID, SecretAccessKey: s3.SecretAccessKey,
	})
	if err != nil {
		return report, migrationRefused("the target bucket did not pass its probe: %v", err)
	}
	report.DirectUpload = "available"
	if ok, why := bucket.DirectUpload(); !ok {
		report.DirectUpload = "unavailable (" + why + ")"
	}
	log.InfoContext(ctx, "objects_migration_started", "from", report.From, "to", report.To,
		"final", final)
	report.Pass, err = migrate.Run(ctx, migrate.Options{
		Source: e.objects.store, Target: bucket, References: refs,
		// A BARRIER ONLY WHERE THIS NODE MAY APPEND ONE: a maintenance
		// node reads what it holds, which nothing is changing.
		Pin: e.Mode().Publishes(),
	})
	if err != nil {
		return report, err
	}
	if !final {
		report.Next = "Every referenced object the store can give back is in the bucket. " +
			"To move the fleet, restart every node with `crewlet run -mode maintenance` " +
			"and run this again with -final."
		log.InfoContext(ctx, "objects_migration_copied", "to", report.To,
			"copied", report.Pass.Copied, "present", report.Pass.Present,
			"unreadable", report.Pass.Unreadable)
		return report, nil
	}
	if !report.Pass.Done() {
		return report, migrationRefused("the estate this node read is not complete (it holds a record " +
			"it could not apply), so an object the pass did not see may be named; upgrade " +
			"the node that cannot apply it and run this again")
	}
	if s, ok := e.backends.Objects.(sealer); ok {
		if err := s.Seal(ctx); err != nil {
			return report, fmt.Errorf("engine: seal the fleet's object bucket before the move: %w", err)
		}
		report.Sealed = true
	}
	if err := e.backends.Fleet.MoveObjectBackend(ctx, report.From, report.To); err != nil {
		return report, fmt.Errorf("engine: move the fleet's object store record to %s: %w", report.To, err)
	}
	report.Moved = true
	report.Next = "The fleet's files are in " + report.To + ". Give every node the target " +
		"file's store.objects block and restart it with `crewlet run` (no -mode); a node still " +
		"naming the old store refuses to start. The old store is left whole for you to delete " +
		"once the fleet runs on the bucket."
	log.InfoContext(ctx, "objects_migrated", "from", report.From, "to", report.To,
		"copied", report.Pass.Copied, "present", report.Pass.Present,
		"unreadable", report.Pass.Unreadable, "sealed", report.Sealed)
	return report, nil
}

// migrationReferences is every declared table this node's estate holds, as a
// migration walks them: only a data node running the native runtime holds
// the rows that name objects, the tracker's files and the artifacts' versions
// and assets alike ([objectEstatesFor]), so a migration copies every object
// the collector keeps alive.
func (e *Engine) migrationReferences() (collect.References, error) {
	n := e.native.Load()
	if n == nil {
		return nil, migrationRefused("this node holds no copy of the company's records; run the " +
			"migration against a data node")
	}
	estates := objectEstatesFor(n, e.ArtifactsReader(), e.AssistantReader(), e.ExecutorReader())
	if len(estates) == 0 {
		return nil, migrationRefused("this node holds no copy of the company's records; run the " +
			"migration against a data node")
	}
	refs, err := collect.Sources(references.All, estates...)
	if err != nil {
		return nil, fmt.Errorf("engine: the estate's object references: %w", err)
	}
	return refs, nil
}

// fenced refuses a final pass on a fleet that could still name an object the
// pass does not copy: see the file comment.
func (e *Engine) fenced(ctx context.Context) error {
	if mode := e.Mode(); mode != statelog.ModeMaintenance {
		return migrationRefused("the final pass runs only where nothing publishes, and this node runs "+
			"in %s mode: restart every node with `crewlet run -mode maintenance` first "+
			"(seal mode cannot seal the bucket, which is a stream configuration change)", mode)
	}
	admissions, err := e.backends.Fleet.Admissions(ctx)
	if err != nil {
		return fmt.Errorf("engine: read which nodes may publish: %w", err)
	}
	if len(admissions) > 0 {
		nodes := make([]string, 0, len(admissions))
		for _, a := range admissions {
			nodes = append(nodes, a.NodeID)
		}
		// NO EXCLUSION TO OFFER, for [Engine.openCapacity]'s reason: an
		// exclusion is a gesture on an open capacity window, and a final
		// pass is none.
		return migrationRefused("these nodes may still publish records naming objects, since "+
			"each holds an admission: %s. Restart each in maintenance mode. A node withdraws "+
			"its admission when it stops", words.And(nodes))
	}
	if !e.StateLogHydrated(ctx) {
		return migrationRefused("this node has not applied the company's records to the end of " +
			"their logs, so the pass could miss an object; wait for it to catch up and " +
			"run this again")
	}
	// WHAT THIS BUILD CANNOT SEE, as the collector asks it ([Engine.unseen]):
	// a final pass copies every object this build's declarations name, and
	// the store it leaves behind is the operator's to delete, so a newer
	// build's objects it never copied would go with it.
	why, err := e.unseen(ctx)
	if err != nil {
		return fmt.Errorf("engine: read what this node cannot see that may name an object: %w", err)
	}
	if why != "" {
		return migrationRefused("the final pass would leave objects behind in the store it is "+
			"leaving: %s. Run the migration from a build that knows every log the fleet runs", why)
	}
	return nil
}

// migrationRefused is [ErrMigrationRefused] with why.
func migrationRefused(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrMigrationRefused, fmt.Sprintf(format, args...))
}
