package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet config scrub` — erasing personal data from superseded revisions.
//
// # The defect it closes
//
// A company's org chart used to live inside the company document, and every
// revision of that document is an immutable snapshot kept for ever, in an
// append-only table, on every node that met it, in every backup. A human seat
// carries an `email` and a `contact` block of external account ids — so a
// company that has ever named somebody in its chart has archived their
// personal data in a place nothing deletes from. Removing the seat never
// reached it: the removal writes a NEW revision and every older one still
// holds them.
//
// Revisions written after the chart moved onto its own log carry no chart at
// all, so nothing new enters the archive. This is for what is already there,
// and it is meant to be run ONCE — after which the retention sweep
// (maintenance.RevisionRetention) is what eventually removes the rows.
//
// # Why it refuses the active revision
//
// The fleet is serving that document and every node is holding it. Rewriting
// it underneath them would be a configuration change nothing activated, with
// no epoch, no apply and no event — the company would run on a document its
// operator never wrote. The refusal is enforced by the UPDATE's own WHERE
// clause rather than by this command remembering to check (see
// [store.Configs.Scrub]); an operator who wants an address out of the LIVE
// company edits the company, which writes a revision this can then reach.
//
// # It is not reversible
//
// There is no unscrub. The tombstone is [redact.ScrubMask], which is
// deliberately NOT the credential mask every config surface writes: that one
// means "you may not see this" and is restored from the row behind it, and
// this one means the value is gone.

// scrubConfig erases the personal fields of every superseded revision, or of
// the one an operator named.
//
// DRY RUN BY REPORT, not by flag default: it writes unless -dry-run says
// otherwise, because an operator running a privacy erasure has asked for one.
// The dry run exists because the set is not obvious from outside — an
// operator wants to know how many revisions hold personal data before they
// make an irreversible change to all of them.
func scrubConfig(ctx context.Context, cs *configStore, revisionID string,
	dryRun bool, stdout io.Writer,
) error {
	revisions, err := scrubTargets(ctx, cs, revisionID, scrubPage)
	if err != nil {
		return err
	}

	scrubbed, fields := 0, 0
	for _, rev := range revisions {
		// OPENED THROUGH THE SAME PATH every other reader uses, so a
		// payload this build cannot make sense of stops the run rather
		// than being rewritten into a shape no node can apply.
		document, err := secrets.Open(cs.cipher, rev.Payload)
		if err != nil {
			return fmt.Errorf("open revision %s: %w", rev.ID, err)
		}
		cleaned, n, err := config.ScrubPersonalData(document)
		if err != nil {
			return fmt.Errorf("scrub revision %s: %w", rev.ID, err)
		}
		if n == 0 {
			continue
		}
		fields += n
		scrubbed++
		if dryRun {
			fmt.Fprintf(stdout, "revision %s holds %d personal field(s)\n", rev.ID, n)
			continue
		}
		payload, err := secrets.Seal(cs.cipher, cleaned)
		if err != nil {
			return fmt.Errorf("seal revision %s: %w", rev.ID, err)
		}
		at := time.Now().UTC()
		if err := cs.configs.Scrub(ctx, rev.ID, payload, at); err != nil {
			return err
		}
		recordScrub(ctx, cs, rev.ID, n, at, stdout)
		fmt.Fprintf(stdout, "scrubbed revision %s: %d personal field(s) erased\n", rev.ID, n)
	}

	switch {
	case scrubbed == 0:
		fmt.Fprintf(stdout, "no personal data in %d superseded revision(s); nothing to scrub\n",
			len(revisions))
	case dryRun:
		fmt.Fprintf(stdout, "%d revision(s) hold %d personal field(s); "+
			"re-run without -dry-run to erase them\n", scrubbed, fields)
	default:
		fmt.Fprintf(stdout, "scrubbed %d revision(s), %d personal field(s) erased.\n",
			scrubbed, fields)
		// SAID EVERY TIME, because the two places the data also sits are
		// not reachable from here and an operator who thinks this
		// finished the job has not finished it.
		fmt.Fprintln(stdout,
			"This node only. Run it on every node, and note that backups "+
				"taken before now still hold the original revisions.")
	}
	return nil
}

// scrubTargets is the revisions a run will rewrite.
//
// EVERY SUPERSEDED ONE by default, because the erasure is only as complete as
// the set it covers and an operator asked for an erasure. A named revision is
// for the operator working through a list, and it is refused if it is the
// active one — by the store, with the reason, rather than silently skipped
// here, which would report a scrub that did not happen.
//
// THE PAGE SIZE IS A PARAMETER rather than the constant read inside, and it
// is not a test hook: it is what this walk is FOR. The failure it has is
// stopping at the first page, which on a real deployment is invisible — the
// command reports a clean scrub over an archive it read the newest two
// hundred rows of — and a case that proved otherwise by importing two hundred
// and one revisions would take minutes to say one thing.
func scrubTargets(ctx context.Context, cs *configStore, revisionID string, page int) ([]store.Revision, error) {
	if revisionID != "" {
		rev, err := revisionOrActive(ctx, cs, revisionID)
		if err != nil {
			return nil, err
		}
		if rev.Active {
			return nil, fmt.Errorf("%w: %s is the revision this fleet is "+
				"serving. Edit the company to remove the value from the live "+
				"configuration, which writes a revision this can then reach",
				store.ErrRevisionIsActive, rev.ID)
		}
		return []store.Revision{rev}, nil
	}
	// EVERY revision, paged, rather than one screen of them: the default
	// page is a listing's default and this is an erasure, so stopping at
	// fifty would leave the archive holding whatever came before.
	var out []store.Revision
	for offset := 0; ; offset += page {
		batch, err := cs.configs.List(ctx, page, offset)
		if err != nil {
			return nil, err
		}
		for _, rev := range batch {
			if !rev.Active {
				out = append(out, rev)
			}
		}
		if len(batch) < page {
			return out, nil
		}
	}
}

// scrubPage is how many revisions one listing reads.
//
// Two hundred, well above the store's own page default, because this walks
// the WHOLE table rather than rendering a screen: each round trip costs a
// query and the rows are a few kilobytes each since the chart left the
// document, so a bigger page is fewer queries for a bounded amount of memory.
// It stays a page rather than an unbounded read because the table is
// unbounded on a deployment that has never swept it — which is the deployment
// this command exists for.
const scrubPage = 200

// recordScrub writes the audit row.
//
// BEST EFFORT, and it says so when it fails rather than unwinding: the
// erasure has already happened and cannot be undone, so a failure to record
// it is a gap in the audit trail rather than a reason to claim the scrub did
// not occur. The column stamped by the store is the durable half either way.
func recordScrub(ctx context.Context, cs *configStore, revisionID string, fields int,
	at time.Time, stdout io.Writer,
) {
	if cs.events == nil {
		return
	}
	ev := events.New(types.ConfigRevisionScrubbed{
		RevisionID: revisionID, Fields: fields, ScrubbedBy: currentOperator(),
	}, events.NewTrace())
	ev.Timestamp = at
	ev.Source = "cli"
	rec, ok := observe.Record(ev)
	if !ok {
		return
	}
	if err := cs.events.Append(ctx, rec); err != nil {
		fmt.Fprintf(stdout, "warning: revision %s was scrubbed but the audit "+
			"row could not be written: %v\n", revisionID, err)
	}
}
