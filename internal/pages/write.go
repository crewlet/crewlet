package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WRITE PATHS, one record each.
//
// Every one of them takes ONE snapshot of this node's own rows, decides inside
// it, publishes, and lets the broker arbitrate. That is the framework's rule
// and not this package's — see [statelog.Publisher] — and what it buys here is
// that a behind node cannot corrupt the knowledge base. It can only fail to
// write.

// NewPage is a page to create.
type NewPage struct {
	Container string
	Title     string
	Body      string
	ParentID  string
	Labels    []string

	// Status defaults to published. A DRAFT IS THE OPT-IN: somebody
	// writing a page normally means it to be read, and a knowledge base
	// whose default was draft would fill up with pages nobody can find and
	// nobody remembers to publish.
	Status Status

	// Message is the one-line note recorded with the first revision.
	Message string

	// Watchers are added beyond the author.
	Watchers []string

	// Quiet suppresses the wake, for an import.
	Quiet bool
}

// Written is what a write reports back.
type Written struct {
	Page Page

	// Revision is the log revision the page's row is AT after this write,
	// in the one number space every reader here answers in — see
	// [HeadRevision]. A record that landed produces it from its own
	// position; a write that changed nothing produces it from the row its
	// decision read, because nothing landed and the row did not move.
	//
	// NEVER A LITERAL ZERO on a write that succeeded. Zero is the one
	// value a caller cannot act on — it is both "this node has applied
	// nothing for this page" and "nobody answered" — and it is what a
	// no-op used to report while also reporting success, straight into the
	// map a page tool serializes for a model.
	Revision uint64

	ChangeID string

	// Outcome is the framework's three-valued answer — applied, pending or
	// unknown — carried unchanged. A caller that needs its own write back
	// waits for the position; one that does not can act on `applied` alone.
	Outcome statelog.Result
}

// Create writes a new page.
//
// ONE RECORD, arbitrated on the TITLE. The address is what two writers contend
// for: two people making "Deploy Runbook" must fight, and two fresh uuids
// never would. Its apply writes the claim, the head, the first revision and
// the history entry in one transaction, so there is no orphan claim and no
// grace rule for stepping over one.
func (s *Store) Create(ctx context.Context, actor Actor, in NewPage) (Written, error) {
	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	container := strings.ToUpper(strings.TrimSpace(in.Container))
	if container == "" {
		return Written{}, invalid("container", "a page needs a container to live in")
	}
	if err := s.checkTitle(in.Title); err != nil {
		return Written{}, err
	}
	if err := s.checkBody(in.Body); err != nil {
		return Written{}, err
	}
	labels := cleanList(in.Labels)
	if err := s.checkLabels(labels); err != nil {
		return Written{}, err
	}
	if in.Status == "" {
		in.Status = StatusPublished
	}
	if !in.Status.Valid() {
		return Written{}, invalid("status", "%q is not one of %v", in.Status, Statuses())
	}
	if len(in.Message) > MaxMessage {
		return Written{}, invalid("message", "%d bytes, past the %d-byte cap",
			len(in.Message), MaxMessage)
	}

	at := s.now()
	title := strings.Join(strings.Fields(in.Title), " ")
	page := Page{
		V: DocumentVersion, ID: s.newID(), Container: container,
		ParentID: strings.TrimSpace(in.ParentID),
		Title:    title, Body: in.Body, Status: in.Status, Labels: labels,
		Version: 1, Author: actor.Name(), CreatedAt: at, UpdatedAt: at,
	}
	page.Watchers = watcherSet(actor.Handle, in.Watchers)
	if len(page.Watchers) > MaxWatchers {
		return Written{}, invalid("watchers", "%d watchers, past the cap of %d",
			len(page.Watchers), MaxWatchers)
	}

	subject := TitleSubject(container, title)
	opID := s.newSeqID()
	scope := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermTitle, Container: container, ID: TitleToken(title)},
		{Kind: TermObject, Container: container, ID: page.ID},
	}}
	notify := s.notifyOf(in.Quiet, ChangeCreated, page,
		excerpt(firstLine(in.Body, title)), nil)

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindTitle), ID: subject.ID},
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternCreate,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			if err := checkParent(ctx, tx, page.ID, page.ParentID); err != nil {
				return statelog.Decision{}, err
			}
			return s.decide(actor, subject, OpCreate, scope, opID, CreatePayload{
				V: DocumentVersion, PageID: page.ID, Container: container,
				Title: title, ParentID: page.ParentID, Body: page.Body,
				Status: page.Status, Labels: page.Labels,
				Watchers: page.Watchers, Author: page.Author,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	// NO ROW EXISTED TO READ, so there is no prior revision to fall back
	// on — and a create that changed nothing is not a shape this path has:
	// it either takes the address or is refused as taken.
	return Written{
		Page: page, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	}, nil
}

// writtenRevision is the number a write reports as [Written.Revision].
//
// ONE RULE IN ONE PLACE, because every write path here has both arms: a record
// that landed answers with ITS OWN composed position, which is exactly what
// the applier stamps into the row, so a caller can compare it against a later
// read and know whether its write is visible. A decision that published
// nothing answers with the revision the row was already at, read inside the
// same snapshot the decision was made in.
//
// The alternative — zero, on the argument that no record exists — is the one
// this package already rejected for [Store.Page], and the two must not
// disagree about what the same field means.
func writtenRevision(result statelog.Result, read uint64) uint64 {
	if result.Position.Seq == 0 {
		return read
	}
	return uint64(result.Position.Packed())
}

// Save is an edit to a page's content.
//
// IT DOES NOT CARRY A TITLE. An address change contends for the address and a
// content change contends for the page, and one record has one subject — so a
// record that carried both could arbitrate only one of them and the other
// would be a lost update with no symptom. Renaming is [Store.Rename].
type Save struct {
	// BaseVersion is the version the editor read. REQUIRED, unlike a work
	// item's optional If-Match: a wiki's worst failure is silently
	// overwriting a paragraph somebody else just wrote, and there is no
	// per-field merge that makes that safe for prose.
	BaseVersion int

	Body *string

	ParentID *string
	Labels   *[]string
	Status   *Status

	// Message is the one-line note recorded with this revision.
	Message string

	// Watch adds or removes the actor, setting the mute on a removal.
	Watch *bool

	Quiet bool
}

// SavePage applies an edit to a page, arbitrated on the page's own subject.
func (s *Store) SavePage(ctx context.Context, actor Actor, pageID string,
	save Save) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if save.BaseVersion <= 0 {
		return Written{}, invalid("base_version",
			"a save must say which version it edited. Read the page first and "+
				"pass its `version` back, so an edit somebody else made in the "+
				"meantime is a refusal rather than a silent overwrite")
	}
	if len(save.Message) > MaxMessage {
		return Written{}, invalid("message", "%d bytes, past the %d-byte cap",
			len(save.Message), MaxMessage)
	}
	if save.Body != nil {
		if err := s.checkBody(*save.Body); err != nil {
			return Written{}, err
		}
	}
	if save.Labels != nil {
		if err := s.checkLabels(cleanList(*save.Labels)); err != nil {
			return Written{}, err
		}
	}
	if save.Status != nil && !save.Status.Valid() {
		return Written{}, invalid("status", "%q is not one of %v",
			*save.Status, Statuses())
	}
	if save.ParentID != nil {
		// TRIMMED as [Store.Create] trims it, so " p1" and "p1" are
		// one parent rather than a move to a page that does not exist.
		parent := strings.TrimSpace(*save.ParentID)
		save.ParentID = &parent
	}

	at := s.now()
	opID := s.newSeqID()
	var out Page
	var read uint64
	subject := PageSubject(pageID)

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, revision, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Version != save.BaseVersion {
				return statelog.Decision{}, fmt.Errorf(
					"%w: this edit is against version %d and the page is at %d",
					ErrStaleVersion, save.BaseVersion, head.Version)
			}
			if save.ParentID != nil && *save.ParentID != head.ParentID {
				if err = checkParent(ctx, tx, pageID, *save.ParentID); err != nil {
					return statelog.Decision{}, err
				}
			}
			patch, kind, changed := s.patchOf(actor, &head, save, at)
			// PATCHED OR NOT, THE HEAD IS THE ANSWER, and it is taken
			// BEFORE the no-op return. patchOf mutates the page it was
			// handed, so this one value is what the apply will produce
			// on a change and what the page already says on a no-op —
			// and a caller told its write landed reads the page out of
			// that answer. Assigned below the check instead, an
			// idempotent save reported success carrying an empty id,
			// title and version, which a tool serializes verbatim.
			//
			// THE REVISION IS TAKEN HERE FOR THE SAME REASON, and from
			// the same statement: on a no-op it is the only number
			// there will ever be, because no record lands to produce a
			// position.
			out, read = head, revision
			if !changed {
				// A NO-OP IS A SUCCESS, not an error: an update that
				// changes no field is one the caller should be told
				// landed.
				return statelog.Decision{}, nil
			}
			scope := ScopeSet{Subject: true, Container: head.Container}
			notify := s.notifyOf(save.Quiet, kind, head,
				excerptOfSave(save, head), nil)
			return s.decide(actor, subject, OpPatch, scope, opID, patch, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// Rename puts a page at a title, and there are TWO records behind that one
// gesture because there are two different things to contend for.
//
// WHEN THE ADDRESS MOVES it is arbitrated on the NEW title, create-only: the
// address is what two writers fight over, and two fresh uuids never would. It
// stamps the head's `scoped_through` and never its `version`, so the row's
// broker expectation still matches its subject's last message and cannot be
// poisoned into permanent unwritability; a read barrier compares the MAX of
// the two.
//
// WHEN ONLY THE CAPITALISATION OR THE SPACING MOVES the address does not, and
// the record goes to [Store.retitle] instead. That is not a special case
// smuggled past the rule — it is the rule: the title subject is the same
// subject this page already holds, so there is nothing there to contend for,
// and the only row the write touches is the page's own.
func (s *Store) Rename(ctx context.Context, actor Actor, pageID string,
	title string, quiet bool) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	if err := s.checkTitle(title); err != nil {
		return Written{}, err
	}
	title = strings.Join(strings.Fields(title), " ")

	at := s.now()
	opID := s.newSeqID()
	var out Page

	head, err := s.head(ctx, pageID)
	if err != nil {
		return Written{}, err
	}
	if NormalizeTitle(head.Title) == NormalizeTitle(title) {
		return s.retitle(ctx, actor, pageID, title, opID, at, quiet)
	}

	subject := TitleSubject(head.Container, title)
	scope := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermTitle, Container: head.Container, ID: TitleToken(title)},
		{Kind: TermTitle, Container: head.Container, ID: TitleToken(head.Title)},
		{Kind: TermObject, Container: head.Container, ID: pageID},
	}}

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindTitle), ID: subject.ID},
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternCreate,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			current, _, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = current
			out.Title = title
			out.UpdatedAt = at
			notify := s.notifyOf(quiet, ChangeRenamed, out, "", nil)
			return s.decide(actor, subject, OpRename, scope, opID, RenamePayload{
				V: DocumentVersion, PageID: pageID,
				Container: current.Container, Title: title,
				FormerContainer: current.Container, FormerTitle: current.Title,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	// A RENAME THAT MOVES THE ADDRESS ALWAYS LANDS A RECORD, so the read
	// arm is unreachable — the decision is unconditional and its refusals
	// are errors, never an empty decision.
	return Written{
		Page: out, Revision: writtenRevision(result, 0), ChangeID: opID,
		Outcome: result,
	}, nil
}

// retitle changes a page's DISPLAYED title without moving its address.
//
// # Why this is a write at all, and not the no-op it used to be
//
// `pages_titles` holds the NORMALISED title and `pages_heads.title_norm` is
// byte-identical either side of this record — but `pages_heads.title` is not.
// That column is the DISPLAYED title, kept beside the normalised one precisely
// because "a link is resolved by the second and rendered from the first"
// (migration 0005), so "Runbook" -> "RUNBOOK" changes a real field that every
// reader, every link and every wake renders. Answering `applied` while
// discarding it reported a change that did not happen.
//
// # Why it arbitrates on the PAGE
//
// The address does not move, so the title subject this would otherwise take is
// the one THIS PAGE ALREADY HOLDS: a create-only append there loses to its own
// claim and is reported as a name somebody else took. And the record writes
// exactly one row — the page's head — which is the domain's own rule for which
// subject a write belongs on: a body save, a label, a watch, a comment, a trash
// and a restore all contend for the page, and so does this. A title record that
// touched no title row would be an address record about no address.
//
// # What the page's subject cannot arbitrate, and what covers it
//
// A concurrent rename that MOVES the address contends on the NEW title's
// subject, which this record never touches, so the broker cannot order the two
// for us. The record therefore STATES the title it was decided against and the
// applier writes only while the row still holds that address — deterministically
// on every node, so a retitle that lost the race writes nothing everywhere
// rather than parking the page at a name it holds no claim to. The decision
// below refuses the same shape one step earlier, where it can still be reported
// to the caller instead of silently skipped.
func (s *Store) retitle(ctx context.Context, actor Actor, pageID, title,
	opID string, at time.Time, quiet bool) (Written, error) {

	subject := PageSubject(pageID)
	var out Page
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, revision, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = current, revision
			if current.Title == title {
				// THE TRUE NO-OP: the same displayed title at the
				// same address, so there is nothing left to write.
				//
				// RETURNED AS AN EMPTY DECISION so the framework's
				// own no-op arm answers it, rather than a result
				// spelled out here. That arm reports FOUR fields —
				// `applied`, the op id, the decision's version and
				// the round count — and the literal this replaces
				// set two of them, which was harmless only because
				// [Store.decide] happens never to set
				// Decision.Version on any path. A second copy of a
				// contract, correct by coincidence, is the shape
				// that breaks the day the coincidence ends.
				return statelog.Decision{}, nil
			}
			if NormalizeTitle(current.Title) != NormalizeTitle(title) {
				// THE ADDRESS MOVED UNDER THIS WRITE, so the
				// gesture is no longer a retitle: it is a move to
				// an address nothing has arbitrated. Refused
				// rather than published, because the applier's
				// own guard would drop it on every node while
				// this caller was told `applied`.
				return statelog.Decision{}, fmt.Errorf(
					"%w: this rename was decided against the title %q and the "+
						"page is now at %q, which is a different address — read "+
						"the page again and rename from where it is",
					ErrStaleVersion, title, current.Title)
			}
			out.Title = title
			out.UpdatedAt = at
			scope := ScopeSet{Subject: true, Container: current.Container}
			notify := s.notifyOf(quiet, ChangeRenamed, out, "", nil)
			return s.decide(actor, subject, OpRetitle, scope, opID, RetitlePayload{
				V: DocumentVersion, PageID: pageID,
				Title: title, FormerTitle: current.Title,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// Trash moves a page out of every reader's way, reversibly.
func (s *Store) Trash(ctx context.Context, actor Actor, pageID string) (Written, error) {
	return s.status(ctx, actor, pageID, OpTombstone, ChangeRemoved, "")
}

// Restore takes a trashed page back.
func (s *Store) Restore(ctx context.Context, actor Actor, pageID string) (Written, error) {
	return s.status(ctx, actor, pageID, OpRestore, ChangeStatus, "")
}

// Purge destroys a page permanently, and writes the marker that makes the
// removal irreversible.
//
// THE MARKER IS THE POINT: it is how a node that was away tells a page that
// never existed from one that was deliberately destroyed, and without it a
// redelivery months later would resurrect it.
func (s *Store) Purge(ctx context.Context, actor Actor, pageID, reason string) (Written, error) {
	return s.status(ctx, actor, pageID, OpPurge, ChangeRemoved, reason)
}

// status is the shared shape of a trash, a restore and a purge.
func (s *Store) status(ctx context.Context, actor Actor, pageID string,
	op OpKind, kind ChangeKind, reason string) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	at := s.now()
	opID := s.newSeqID()
	subject := PageSubject(pageID)
	var out Page
	var read uint64

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, revision, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out, read = head, revision
			scope := ScopeSet{Subject: true, Container: head.Container}
			notify := s.notifyOf(false, kind, head, "", nil)
			return s.decide(actor, subject, op, scope, opID, StatusPayload{
				V: DocumentVersion, Reason: reason,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// EnsureContainer creates a space if it is not there, or updates its settings.
//
// THE SECOND VALUE IS WHETHER ANYTHING WAS WRITTEN, not whether the call
// succeeded. This runs on every boot for every unit's space, so the ordinary
// outcome is that the row already says what the chart says — and a caller
// that could not tell that from a create would log "applied" on every restart
// for a company nobody had edited.
func (s *Store) EnsureContainer(ctx context.Context, key, name, purpose string) (
	Container, bool, error) {

	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return Container{}, false, invalid("container", "a container needs a key")
	}
	at := s.now()
	opID := s.newSeqID()
	subject := ContainerSubject(key)
	changed := true
	out := Container{V: DocumentVersion, Key: key, Name: name,
		Purpose: purpose, CreatedAt: at}

	_, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindContainer), ID: key},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			var held Container
			var document []byte
			err := tx.QueryRowContext(ctx,
				`SELECT document FROM pages_containers WHERE key = ?`, key).
				Scan(&document)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return statelog.Decision{}, fmt.Errorf(
					"pages: read the container %s: %w", key, err)
			default:
				if held, err = DecodeContainer(document); err != nil {
					return statelog.Decision{}, err
				}
				if held.Name == name && held.Purpose == purpose {
					// UNCHANGED IS A NO-OP. This runs on every
					// boot for every unit's space, and a record
					// per boot per space is a log that grows
					// with restarts rather than with edits.
					out = held
					changed = false
					return statelog.Decision{}, nil
				}
				out.CreatedAt = held.CreatedAt
			}
			return s.decide(Actor{Handle: "system", Kind: AuthorOperator},
				subject, OpPatch, ScopeSet{Subject: true}, opID,
				ContainerPayload{
					V: DocumentVersion, Key: key, Name: name, Purpose: purpose,
				}, nil, at)
		},
	})
	if err != nil {
		return Container{}, false, err
	}
	return out, changed, nil
}

// patchOf turns a save into a record's payload and reports what changed.
//
// IT MUTATES THE HEAD IT WAS GIVEN, so the caller's returned page is what the
// apply will produce — and it reports the patch's [PagePatch.editKind], the
// one verb the applier derives from the same record for the history row.
func (s *Store) patchOf(actor Actor, head *Page, save Save, at time.Time) (
	PagePatch, ChangeKind, bool) {

	patch := PagePatch{V: DocumentVersion}

	if save.Body != nil && *save.Body != head.Body {
		patch.Body = save.Body
		if save.Message != "" {
			message := save.Message
			patch.Message = &message
		}
		head.Body = *save.Body
		head.Version++
		patch.RetiredRevisions = retiredRevisions(head.Version)
	}
	if save.ParentID != nil && *save.ParentID != head.ParentID {
		patch.ParentID = save.ParentID
		head.ParentID = *save.ParentID
	}
	if save.Status != nil && *save.Status != head.Status {
		patch.Status = save.Status
		head.Status = *save.Status
	}
	if save.Labels != nil {
		labels := cleanList(*save.Labels)
		if !sameSet(labels, head.Labels) {
			// NEVER A NIL SLICE ON THE PATCH: cleanList answers nil for
			// a save that removed every label, and a pointer to nil
			// travels as `null` — an absent field, which leaves every
			// node's labels where they were. See [PagePatch.Labels].
			carried := append(make([]string, 0, len(labels)), labels...)
			patch.Labels = &carried
			head.Labels = labels
		}
	}
	// THE MUTATOR REPORTS WHAT IT MOVED, on [subscribeMentions]'s shape and
	// for a harder reason: the sizes of the two sets cannot answer this.
	// MUTED IS NOT A SUBSET OF WATCHERS — a `watch: false` from somebody
	// who never followed the page adds them to Muted alone — so their
	// later `watch: true` takes one handle OUT of Muted and puts one INTO
	// Watchers, and the total is exactly where it started. (An unwatch by
	// an ACTUAL watcher is the easy half: the handle stays in Watchers and
	// the mute is added beside it, so the total moves.) The clause that
	// used to cover the gap ("the mute now disagrees with what was asked")
	// is false by construction after applyWatch has run, so it covered
	// nothing and fired only for an operator, who has no handle at all: a
	// `watch: false` from the operator surface published a record and woke
	// every watcher for a change to nobody's subscription.
	if save.Watch != nil && applyWatch(head, actor.Handle, *save.Watch) {
		patch.Watchers = head.Watchers
		patch.Muted = head.Muted
	}
	kind, changed := patch.editKind()
	if !changed {
		return patch, "", false
	}
	head.UpdatedAt = at
	return patch, kind, true
}

// checkParent refuses a parent a page cannot have, inside the decision's own
// snapshot: one this node holds no page for, the page itself, or a page
// beneath it. Empty is the top of the container and always allowed.
//
// A PARENT THAT CLOSES A LOOP leaves the page and everything under it with a
// parent chain that never reaches the top of its container — a breadcrumb
// ends at the loop instead — and a parent nothing holds leaves a chain that
// ends at a page nobody can open. So both are refused naming the field rather
// than stored.
//
// WHAT THIS CANNOT REFUSE is a loop two writes close between them — each on
// its own page's subject, each decided before the other applied — because
// neither snapshot holds the other's move and the broker orders the two
// subjects independently. [parentChains.above] keeps its record of visited
// pages for that case.
//
// A PARENT ON ANOTHER CONTAINER IS NOT REFUSED: the detail read selects a
// page's children by parent alone, whatever container each is in, so such a
// child is still reachable from its parent.
//
// THE CHAIN IS [parentChains]'s, the walk a breadcrumb and a search's ancestor
// exclusion read, so what this refuses as "beneath" is what every reader
// renders above: a page this node does not hold ends a chain for all three.
func checkParent(ctx context.Context, tx *sql.Tx, pageID, parentID string) error {
	if parentID == "" {
		return nil
	}
	if parentID == pageID {
		return invalid("parent_id", "a page cannot be its own parent")
	}
	chains := newParentChains(tx)
	parent, held, err := chains.page(ctx, parentID)
	if err != nil {
		return err
	}
	if !held {
		return invalid("parent_id", "this node holds no page %s — a parent "+
			"is named by its page id, and a page written moments ago may "+
			"not have been applied here yet", parentID)
	}
	above, _, err := chains.above(ctx, parentID, parent.ParentID)
	if err != nil {
		return err
	}
	for _, ancestor := range above {
		if ancestor.ID == pageID {
			return invalid("parent_id", "page %s is beneath this page, so "+
				"putting this page under it would close a loop — move %s "+
				"out from under this page first", parentID, parentID)
		}
	}
	return nil
}

// retiredRevisions is the exact list of versions this save's apply deletes.
//
// COMPUTED BY THE WRITER AND CARRIED, never re-derived by each applier: a
// "keep the last hundred" rule evaluated per node deletes on that node's own
// authority, and two nodes that saw a different set delete different rows —
// permanently, inside the identity claim.
func retiredRevisions(newVersion int) []int {
	oldest := newVersion - RevisionsKept
	if oldest < 1 {
		return nil
	}
	return []int{oldest}
}

// notifyOf builds the routing snapshot, or nil for a quiet write.
//
// A NIL POINTER RATHER THAN A quiet FLAG on the record, so the wake filter is
// "has a Notify" — a question about the record's own shape — instead of a
// boolean a writer can forget to set.
func (s *Store) notifyOf(quiet bool, kind ChangeKind, page Page, text string,
	mentions []string) *Notify {

	if quiet {
		return nil
	}
	return &Notify{
		Kind:       kind,
		PageID:     page.ID,
		Recipients: recipientsOf(page.Watchers, page.Muted),
		Mentions:   mentions,
		Excerpt:    text,
		Container:  page.Container,
		Title:      page.Title,
		Version:    page.Version,
	}
}

// head reads one page outside a decision, for the checks a caller makes before
// publishing at all.
func (s *Store) head(ctx context.Context, pageID string) (Page, error) {
	page, _, err := s.headAt(ctx, pageID)
	return page, err
}

// headAt is [Store.head] plus the log revision the row was written through.
//
// IT DELEGATES TO [readHeadTx] rather than spelling the read out again: the
// query, the revision expression and the two refusals are one rule, and the
// only thing that differs between a decision's read and this one is which
// transaction it runs in. Written twice they drift — which is what a third
// spelling of the same SELECT had already started to do.
func (s *Store) headAt(ctx context.Context, pageID string) (Page, uint64, error) {
	var page Page
	var revision uint64
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		var err error
		page, revision, err = readHeadTx(ctx, tx, pageID)
		return err
	})
	return page, revision, err
}

// watcherSet is the author plus whoever else was named, de-duplicated and
// sorted so two nodes forming the same set write the same rows.
func watcherSet(author string, extra []string) []string {
	out := cleanList(append([]string{author}, extra...))
	slicesSort(out)
	return out
}

// applyWatch adds or removes one handle, setting the mute on a removal, and
// reports whether either set actually moved.
//
// A REMOVAL IS A MUTE rather than a delete, because a page's watcher list is
// also its history of who cared: somebody who unwatches and is later mentioned
// should not be re-subscribed silently.
//
// IT ANSWERS ITS OWN QUESTION rather than leaving the caller to compare the
// sets afterwards, because the comparison is not one a caller can make
// cheaply or safely: [slicesSort] runs in place and the append above it may
// keep the head's backing array, so a slice saved before the call can be
// reordered underneath whoever held it. An EMPTY HANDLE moves nothing and says
// so — an operator write names no seat, and there is no subscription to change
// on behalf of a token.
func applyWatch(page *Page, handle string, watch bool) bool {
	if handle == "" {
		return false
	}
	if watch {
		moved := contains(page.Muted, handle)
		page.Muted = without(page.Muted, handle)
		if !contains(page.Watchers, handle) {
			page.Watchers = append(page.Watchers, handle)
			slicesSort(page.Watchers)
			moved = true
		}
		return moved
	}
	if contains(page.Muted, handle) {
		return false
	}
	page.Muted = append(page.Muted, handle)
	slicesSort(page.Muted)
	return true
}

// excerptOfSave is what a card shows for one edit.
func excerptOfSave(save Save, page Page) string {
	if save.Message != "" {
		return excerpt(save.Message)
	}
	if save.Body != nil {
		return excerpt(firstLine(*save.Body, page.Title))
	}
	return excerpt(page.Title)
}

// firstLine is a body's first non-empty line, or a fallback.
func firstLine(body, fallback string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#> "))
		if line != "" {
			return line
		}
	}
	return fallback
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func without(haystack []string, needle string) []string {
	out := haystack[:0:0]
	for _, v := range haystack {
		if v != needle {
			out = append(out, v)
		}
	}
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, v := range a {
		seen[v] = true
	}
	for _, v := range b {
		if !seen[v] {
			return false
		}
	}
	return true
}
