package doclet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
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
	container := ContainerKey(in.Container)
	if container == "" {
		return Written{}, invalid("container", "a page needs a container to live in")
	}
	// THE DECLARED SPACES FIRST, and for every actor: a key nothing
	// declares is no space at all, whoever typed it — and a seat refused
	// for that is told the true reason rather than one about the reserved
	// containers.
	if err := s.refuseUndeclared(container); err != nil {
		return Written{}, err
	}
	if err := s.refuseReserved(actor, container, true); err != nil {
		return Written{}, err
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
		return Written{}, invalid("status", "%q is not one of %s", in.Status, words.Or(Statuses()))
	}
	if len(in.Message) > MaxMessage {
		return Written{}, invalid("message", "%d bytes, past the %d-byte cap",
			len(in.Message), MaxMessage)
	}

	at := s.now()
	title := strings.Join(strings.Fields(in.Title), " ")
	opID, opErr := s.operation(actor, "create", container+"/"+NormalizeTitle(title), in)
	if opErr != nil {
		return Written{}, opErr
	}
	page := Page{
		V: DocumentVersion, ID: pageIDOf(opID), Container: container,
		ParentID: strings.TrimSpace(in.ParentID),
		Title:    title, Body: in.Body, Status: in.Status, Labels: labels,
		Version: 1, Author: actor.Name(), CreatedAt: at, UpdatedAt: at,
	}
	page.Watchers = watcherSet(actor.subscriber(), in.Watchers)
	if len(page.Watchers) > MaxWatchers {
		return Written{}, invalid("watchers", "%d watchers, past the cap of %d",
			len(page.Watchers), MaxWatchers)
	}

	subject := TitleSubject(container, title)
	scope := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermTitle, Container: container, ID: TitleToken(title)},
		{Kind: TermObject, Container: container, ID: page.ID},
	}}
	notify := s.notifyOf(in.Quiet, ChangeCreated, page,
		excerpt(firstLine(in.Body, title)), nil)

	result, err := s.publish(ctx, statelog.Request{
		Subject: statelog.Subject{Kind: string(KindTitle), ID: subject.ID},
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE PARENT IS JUDGED IN THIS SNAPSHOT, beside the address:
			// a pointer at a page that is not there, is elsewhere or is
			// in the trash files the new page where no walk of its
			// container finds it — see parent.go.
			if err := checkParent(ctx, tx, page.ID, container, page.ParentID); err != nil {
				return statelog.Decision{}, err
			}
			return s.decide(stamp, actor, subject, OpCreate, scope, opID, CreatePayload{
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
	if result.Collapsed {
		// THE ADDRESS AND THE ID ARE WHAT THE OPERATION PINS — its id is
		// derived from the one and the page's from the id — and nothing
		// else about the page this call formed is what the earlier copy
		// wrote.
		return s.landed(ctx, Page{ID: page.ID, Container: container, Title: title},
			opID, result)
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
	// PUBLISHED OR DRAFT, the two a save moves a page between. The trash is
	// a state a page is MOVED into by [Store.Trash], which stamps when: a
	// save that set the status alone would leave a trashed page with no
	// instant for the reaper's thirty days to count from.
	if save.Status != nil && *save.Status != StatusPublished && *save.Status != StatusDraft {
		return Written{}, invalid("status", "a save moves a page between %s and %s, and was "+
			"asked for %q. A page goes to the trash by a trash, never by a save",
			StatusPublished, StatusDraft, *save.Status)
	}

	at := s.now()
	opID, opErr := s.operation(actor, "save", pageID, save)
	if opErr != nil {
		return Written{}, opErr
	}
	var out Page
	var read uint64
	subject := PageSubject(pageID)

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   ScopeSet{Subject: true, Container: container}.Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, revision, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(head, container); moved != nil {
					return statelog.Decision{}, moved
				}
				// INSIDE THE DECIDE, because a save names only a page id
				// and which container it is in comes out of the row. See
				// [Options.Reserved].
				if err := s.refuseReserved(actor, head.Container, false); err != nil {
					return statelog.Decision{}, err
				}
				// BEFORE THE VERSION: a page in the trash takes no save
				// whatever version it was read at, and its status in
				// particular comes back only by a restore, which clears
				// when it was trashed. A save that published it would
				// leave that instant on a page every reader finds again,
				// and the reaper would purge it on schedule.
				if refused := refuseTrashed(head); refused != nil {
					return statelog.Decision{}, refused
				}
				if head.Version != save.BaseVersion {
					return statelog.Decision{}, fmt.Errorf(
						"%w: this edit is against version %d and the page is at %d",
						ErrStaleVersion, save.BaseVersion, head.Version)
				}
				if save.ParentID != nil && *save.ParentID != head.ParentID {
					// A MOVE IS JUDGED IN THE SNAPSHOT IT IS DECIDED IN,
					// including the walk that finds this page above its new
					// parent — see parent.go for what the applier does with
					// the one shape two writers can still make.
					//nolint:govet // shadow: scoped to this block; see .golangci.yml
					if err := checkParent(ctx, tx, pageID, head.Container,
						*save.ParentID); err != nil {
						return statelog.Decision{}, err
					}
				}
				patch, kind, changed := s.patchOf(actor, &head, save, at)
				// PATCHED OR NOT, THE HEAD IS THE ANSWER, taken BEFORE the
				// no-op return: patchOf mutates head, so it is what the
				// apply will produce on a change and what the page says on
				// a no-op. Assigned below the check, an idempotent save
				// reported success with an empty id, title and version.
				//
				// THE REVISION IS TAKEN HERE TOO: on a no-op it is the only
				// number there will be, as no record lands.
				out, read = head, revision
				if !changed {
					// A NO-OP IS A SUCCESS, not an error: an update that
					// changes no field is one the caller should be told
					// landed.
					return statelog.Decision{}, nil
				}
				scope := ScopeSet{Subject: true, Container: head.Container}
				notify := s.notifyOf(save.Quiet, kind, head,
					excerptOfSave(save), nil)
				return s.decide(stamp, actor, subject, OpPatch, scope, opID, patch, notify, at)
			},
		}
	})
	if err != nil {
		return Written{}, err
	}
	if result.Collapsed {
		return s.landed(ctx, Page{ID: pageID}, opID, result)
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// renameAsk is what a rename SAYS, which its operation is bound to
// ([Store.operation]): the title asked for, and whether the change is quiet.
type renameAsk struct {
	Title string
	Quiet bool
}

// Rename puts a page at a title, through TWO records because there are two
// things to contend for.
//
// WHEN THE ADDRESS MOVES it is arbitrated on the NEW title, create-only: the
// address is what two writers fight over. It stamps the head's `scoped_through`
// and never its `version`, so the row's broker expectation still matches its
// subject's last message and cannot be poisoned into permanent unwritability;
// a read barrier compares the MAX of the two.
//
// WHEN ONLY THE CAPITALISATION OR THE SPACING MOVES the address does not, and
// the record goes to [Store.retitle]: the title subject is the one this page
// already holds, so nothing contends and the write touches only the page's row.
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
	var out Page

	head, err := s.head(ctx, pageID)
	if err != nil {
		return Written{}, err
	}
	if err = s.refuseReserved(actor, head.Container, false); err != nil {
		return Written{}, err
	}
	// TWO SHAPES, TWO OPERATIONS. Which one this is is decided from the row
	// BEFORE anything is published, and a retry of a move that landed finds
	// the page already at its new address and is routed to a retitle — on
	// the page's subject, where one id shared with the move would name a
	// ledger row on the title's subject and be refused as an operation
	// reused. Under its own verb the retry decides again and finds nothing
	// left to change, which is the true answer.
	asked := renameAsk{Title: title, Quiet: quiet}
	if NormalizeTitle(head.Title) == NormalizeTitle(title) {
		retitleID, opErr := s.operation(actor, "retitle", pageID, asked)
		if opErr != nil {
			return Written{}, opErr
		}
		return s.retitle(ctx, actor, pageID, title, retitleID, at, quiet)
	}
	opID, opErr := s.operation(actor, "rename", pageID, asked)
	if opErr != nil {
		return Written{}, opErr
	}

	subject := TitleSubject(head.Container, title)
	scope := ScopeSet{Terms: []ScopeTerm{
		{Kind: TermTitle, Container: head.Container, ID: TitleToken(title)},
		{Kind: TermTitle, Container: head.Container, ID: TitleToken(head.Title)},
		{Kind: TermObject, Container: head.Container, ID: pageID},
	}}

	result, err := s.publish(ctx, statelog.Request{
		Subject: statelog.Subject{Kind: string(KindTitle), ID: subject.ID},
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternCreate,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			current, _, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if refused := refuseTrashed(current); refused != nil {
				return statelog.Decision{}, refused
			}
			out = current
			out.Title = title
			out.UpdatedAt = at
			notify := s.notifyOf(quiet, ChangeRenamed, out, "", nil)
			return s.decide(stamp, actor, subject, OpRename, scope, opID, RenamePayload{
				V: DocumentVersion, PageID: pageID,
				Container: current.Container, Title: title,
				FormerContainer: current.Container, FormerTitle: current.Title,
			}, notify, at)
		},
	})
	if err != nil {
		return Written{}, err
	}
	if result.Collapsed {
		return s.landed(ctx, Page{ID: pageID}, opID, result)
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
// A write because the DISPLAYED `pages_heads.title`, which every reader, link
// and wake renders, changes though `title_norm` does not: "Runbook" ->
// "RUNBOOK". It arbitrates on the PAGE, not the title subject this page
// already holds, where a create-only append loses to its own claim.
//
// The page's subject cannot order a concurrent rename that MOVES the address,
// so the record STATES the title it was decided against and the applier writes
// only while the row still holds that address. The decision refuses the same
// shape earlier, where the caller can be told.
func (s *Store) retitle(ctx context.Context, actor Actor, pageID, title,
	opID string, at time.Time, quiet bool) (Written, error) {

	subject := PageSubject(pageID)
	var out Page
	var read uint64

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   ScopeSet{Subject: true, Container: container}.Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				current, revision, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(current, container); moved != nil {
					return statelog.Decision{}, moved
				}
				if refused := refuseTrashed(current); refused != nil {
					return statelog.Decision{}, refused
				}
				out, read = current, revision
				if current.Title == title {
					// THE TRUE NO-OP: the same displayed title at the
					// same address, so there is nothing left to write.
					//
					// RETURNED AS AN EMPTY DECISION so the framework's
					// no-op arm answers it, not a result spelled out
					// here: that arm reports four fields (`applied`, op
					// id, decision version, round count), and a second
					// copy of the contract breaks the day it drifts.
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
				return s.decide(stamp, actor, subject, OpRetitle, scope, opID, RetitlePayload{
					V: DocumentVersion, PageID: pageID,
					Title: title, FormerTitle: current.Title,
				}, notify, at)
			},
		}
	})
	if err != nil {
		return Written{}, err
	}
	if result.Collapsed {
		return s.landed(ctx, Page{ID: pageID}, opID, result)
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// refuseTrashed refuses a change to a page in the trash.
//
// A PAGE IN THE TRASH TAKES NO CHANGE BUT A RESTORE OR A PURGE ([Store.Restore],
// [Store.Purge]): no save, label, watch or move, no rename and no comment
// gesture. It is deleted as far as any reader is concerned ([StatusTrashed])
// and still recoverable, so it is kept as it was trashed: a page rewritten,
// relabelled and discussed while nobody could find it came back from the
// trash as something nobody had read. Only a status was refused here once,
// and the rest landed on the deleted page. Decided in each write's own
// snapshot, where a restore that lands first is seen.
func refuseTrashed(head Page) error {
	if head.Status != StatusTrashed {
		return nil
	}
	return invalid("page", "the page is in the trash, where nothing changes it. "+
		"Restore it first")
}

// Trash moves a page out of every reader's way, reversibly, until the trash
// reaper purges it once it has been in the trash past the trash term
// ([Store.PurgeTrashed]).
func (s *Store) Trash(ctx context.Context, actor Actor, pageID string) (Written, error) {
	return s.status(ctx, actor, pageID, statusMove{op: OpTombstone, kind: ChangeRemoved})
}

// Restore takes a trashed page back.
func (s *Store) Restore(ctx context.Context, actor Actor, pageID string) (Written, error) {
	return s.status(ctx, actor, pageID, statusMove{op: OpRestore, kind: ChangeStatus})
}

// Purge destroys a page permanently, and writes the marker that makes the
// removal irreversible.
//
// THE MARKER IS THE POINT: it is how a node that was away tells a page that
// never existed from one that was deliberately destroyed, and without it a
// redelivery months later would resurrect it.
func (s *Store) Purge(ctx context.Context, actor Actor, pageID, reason string) (Written, error) {
	return s.status(ctx, actor, pageID, statusMove{
		op: OpPurge, kind: ChangeRemoved, reason: reason,
	})
}

// statusMove is one move of a page's status, as [Store.status] makes it.
type statusMove struct {
	op     OpKind
	kind   ChangeKind
	reason string

	// quiet publishes the record with no routing snapshot, so it wakes
	// nobody ([Store.notifyOf]).
	quiet bool

	// admit refuses a head this move may not be made on. It is asked
	// INSIDE THE SNAPSHOT the record is decided in, so the broker's
	// arbitration on the page holds it: a write that lands on the page
	// between the snapshot and the append sends the decision round again,
	// and the head that round reads is the one asked. Nil admits every
	// head, which is what a person's trash, restore and purge are: the
	// authority to make them was decided before the store was reached.
	admit func(Page) error
}

// status is one move of a page's status: a trash, a restore or a purge.
func (s *Store) status(ctx context.Context, actor Actor, pageID string,
	move statusMove) (Written, error) {

	if err := actor.validate(); err != nil {
		return Written{}, err
	}
	op, kind, reason := move.op, move.kind, move.reason
	at := s.now()
	opID, opErr := s.operation(actor, string(op), pageID, reason)
	if opErr != nil {
		return Written{}, opErr
	}
	subject := PageSubject(pageID)
	var out Page
	var read uint64

	result, err := s.onPage(ctx, pageID, func(container string) statelog.Request {
		return statelog.Request{
			Subject: statelog.Subject{Kind: string(KindPage), ID: pageID},
			Scope:   statusScope(op, container).Resolve(subject),
			OpID:    opID,
			Pattern: statelog.PatternArbitrated,
			Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
				head, revision, err := readHeadTx(ctx, tx, pageID)
				if err != nil {
					return statelog.Decision{}, err
				}
				if moved := inContainer(head, container); moved != nil {
					return statelog.Decision{}, moved
				}
				if move.admit != nil {
					if err := move.admit(head); err != nil {
						return statelog.Decision{}, err
					}
				}
				out, read = head, revision
				// THE ANSWER IS THE PAGE THE APPLY WILL PRODUCE, as a save's
				// is: a trash reported the page as it stood before, so the
				// caller that just trashed it was told it was published.
				// The instants are this call's clock, which the apply
				// replaces with the broker's, exactly as a save's are.
				switch op {
				case OpTombstone:
					trashed := at
					out.Status, out.TrashedAt, out.UpdatedAt = StatusTrashed, &trashed, at
				case OpRestore:
					out.Status, out.TrashedAt, out.UpdatedAt = StatusPublished, nil, at
				}
				notify := s.notifyOf(move.quiet, kind, head, "", nil)
				return s.decide(stamp, actor, subject, op, statusScope(op, head.Container),
					opID, StatusPayload{V: DocumentVersion, Reason: reason}, notify, at)
			},
		}
	})
	if err != nil {
		return Written{}, err
	}
	if result.Collapsed {
		return s.landed(ctx, Page{ID: pageID}, opID, result)
	}
	return Written{
		Page: out, Revision: writtenRevision(result, read), ChangeID: opID,
		Outcome: result,
	}, nil
}

// statusScope is what a trash, a restore or a purge of a page in container
// touches.
//
// A PURGE WRITES ITS CHILDREN'S ROWS TOO: each is filed under the purged
// page's own parent, so none is left pointing at a row that is gone (see the
// applier's purge). Which pages those are is decided at the apply, against the
// rows at that position — a child can be filed under this page after this
// snapshot — so the scope is the CONTAINER, which covers every one of them
// whatever arrives. The request and the record state it alike, or the probe
// before the decide would look at one page for a write about a whole space.
func statusScope(op OpKind, container string) ScopeSet {
	if op == OpPurge && container != "" {
		return ScopeSet{Terms: []ScopeTerm{{Kind: TermContainer, ID: container}}}
	}
	return ScopeSet{Subject: true, Container: container}
}

// EnsureContainer creates a space if it is not there, or updates its settings,
// from the configuration activated at activatedAt.
//
// THE SECOND VALUE IS WHETHER ANYTHING WAS WRITTEN: this runs on every boot, so
// the usual outcome is a row already right, which a caller must tell from a
// create. Settings carry their ACTIVATION ([configplane.ActivationStamp]), its
// instant, never the node's clock ([worklet.Writer.ApplyChart] says why). An
// OLDER activation than the row's decides nothing, nor the SAME with the same
// settings; the same with DIFFERENT settings, or a LATER one over unchanged
// settings (a re-stamp), is written. A zero instant is [ErrNoActivation]. The
// operation is FRESH PER CALL, as ApplyChart's; `unknown` is an error, as the
// next apply retries it.
func (s *Store) EnsureContainer(ctx context.Context, activatedAt time.Time,
	key, name, purpose string) (Container, bool, error) {

	key = ContainerKey(key)
	switch {
	case key == "":
		return Container{}, false, invalid("container", "a container needs a key")
	case activatedAt.IsZero():
		return Container{}, false, fmt.Errorf("%w (container %s)", ErrNoActivation, key)
	}
	epoch := configplane.ActivationStamp(activatedAt)
	at := s.now()
	opID := s.newSeqID()
	subject := ContainerSubject(key)
	var (
		changed bool
		out     Container
	)

	result, err := s.publish(ctx, statelog.Request{
		Subject: statelog.Subject{Kind: string(KindContainer), ID: key},
		Scope:   ScopeSet{Subject: true}.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET ON EVERY ROUND. A round that decided to write and
			// lost the broker's arbitration is followed by one that
			// finds the winner's value already there, and only the last
			// round says what this call did.
			changed = false
			out = Container{V: DocumentVersion, Key: key, Name: name,
				Purpose: purpose, ChartEpoch: epoch, CreatedAt: at}
			var document []byte
			err := tx.QueryRowContext(ctx,
				`SELECT document FROM pages_containers WHERE key = ?`, key).
				Scan(&document)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return statelog.Decision{}, fmt.Errorf(
					"doclet: read the container %s: %w", key, err)
			default:
				held, err := DecodeContainer(document)
				if err != nil {
					return statelog.Decision{}, err
				}
				switch {
				case held.ChartEpoch > epoch:
					// A LATER CONFIGURATION ALREADY WON. Two nodes
					// applying two revisions is ordinary during a
					// rollout, and the newer one must not be walked
					// back by the apply of a node holding the older
					// activation arriving second.
					out = held
					return statelog.Decision{}, nil
				case held.ChartEpoch == epoch && held.Name == name &&
					held.Purpose == purpose:
					// UNCHANGED IS A NO-OP. This runs on every boot
					// for every unit's space, and a record per boot
					// per space is a log that grows with restarts
					// rather than with edits.
					out = held
					return statelog.Decision{}, nil
				}
				out.CreatedAt = held.CreatedAt
			}
			changed = true
			// A LATER ACTIVATION OVER THE SAME SETTINGS IS STILL WRITTEN,
			// stamp and all — a re-stamp — because a row at the older
			// stamp is open to any activation between the two.
			return s.decide(stamp, SystemActor("system"),
				subject, OpPatch, ScopeSet{Subject: true}, opID,
				ContainerPayload{
					V: DocumentVersion, Key: key, Name: name, Purpose: purpose,
					ChartEpoch: epoch,
				}, nil, at)
		},
	})
	if err != nil {
		return Container{}, false, err
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return Container{}, false, fmt.Errorf("doclet: whether container %s's "+
			"settings landed is unknown (operation %s); the next apply decides "+
			"them again", key, opID)
	}
	return out, changed, nil
}

// ErrNoActivation refuses a container write that does not name the activation
// its settings came from. See [Store.EnsureContainer].
var ErrNoActivation = errors.New("doclet: a container's settings must name the " +
	"instant their configuration was activated")

// patchOf turns a save into a record's payload and reports what changed.
//
// IT MUTATES THE HEAD IT WAS GIVEN, so the caller's returned page is what the
// apply will produce — and it reports the DOMINANT change kind, because a card
// renders one verb and an edit that touched a body and a label is a save.
func (s *Store) patchOf(actor Actor, head *Page, save Save, at time.Time) (
	PagePatch, ChangeKind, bool) {

	patch := PagePatch{V: DocumentVersion}
	kinds := map[ChangeKind]bool{}

	if save.Body != nil && *save.Body != head.Body {
		patch.Body = save.Body
		if save.Message != "" {
			message := save.Message
			patch.Message = &message
		}
		head.Body = *save.Body
		head.Version++
		kinds[ChangeSaved] = true
		patch.RetiredRevisions = retiredRevisions(head.Version)
	}
	if save.ParentID != nil && *save.ParentID != head.ParentID {
		patch.ParentID = save.ParentID
		head.ParentID = *save.ParentID
		kinds[ChangeMoved] = true
	}
	if save.Status != nil && *save.Status != head.Status {
		patch.Status = save.Status
		head.Status = *save.Status
		kinds[ChangeStatus] = true
	}
	if save.Labels != nil {
		labels := cleanList(*save.Labels)
		if !sameSet(labels, head.Labels) {
			patch.Labels = labels
			head.Labels = labels
			kinds[ChangeLabels] = true
		}
	}
	// THE MUTATOR REPORTS WHAT IT MOVED, as [subscribeMentions] does, because
	// set sizes cannot answer this: MUTED IS NOT A SUBSET OF WATCHERS (a
	// `watch: false` from a non-follower adds to Muted alone), so their later
	// `watch: true` takes a handle OUT of Muted and INTO Watchers and the
	// total is unchanged. An operator has no seat ([Actor.subscriber]), so
	// its `watch: false` must move nothing, not publish a record and wake
	// every watcher for nobody's subscription.
	if save.Watch != nil && applyWatch(head, actor.subscriber(), *save.Watch) {
		patch.Watchers = head.Watchers
		patch.Muted = head.Muted
		kinds[ChangeWatchers] = true
	}
	if len(kinds) == 0 {
		return patch, "", false
	}
	head.UpdatedAt = at
	return patch, dominantKind(kinds), true
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
	err := s.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		page, revision, err = readHeadTx(ctx, tx, pageID)
		return err
	})
	return page, revision, err
}

// watcherSet is the author plus whoever else was named, de-duplicated and
// sorted so two nodes forming the same set write the same rows. An empty
// author — an operator's, which subscribes nobody — adds nothing.
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
// IT ANSWERS ITS OWN QUESTION because a caller cannot safely compare the sets
// afterwards: [slicesSort] runs in place and the append may keep the head's
// backing array, so a slice saved before the call can be reordered under its
// holder. An EMPTY HANDLE moves nothing and says so: an operator names no seat
// ([Actor.subscriber]).
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

// dominantKind is the one verb a card renders for an edit that touched
// several things.
//
// THE ORDER IS THE POINT: a save that also moved a page is a save, and a write
// that only changed watchers is a watch. Rendering the last-set kind would
// make the verb depend on field order in a struct.
func dominantKind(kinds map[ChangeKind]bool) ChangeKind {
	for _, kind := range []ChangeKind{
		ChangeSaved, ChangeMoved, ChangeStatus, ChangeLabels, ChangeWatchers,
	} {
		if kinds[kind] {
			return kind
		}
	}
	return ChangeSaved
}

// excerptOfSave is what a card shows for one edit: what the writer SAID the
// change was, and nothing when they said nothing.
//
// NEVER A LINE OF THE PAGE. This used to fall back to the body's first line
// (or the title), and every save without a message then showed the page's
// opening heading — "Paging rules" — under the saver's name, on the page's
// activity and in every watcher's wake, reading as a change note somebody
// wrote. It is not a summary of the change either: the first line is the
// same before and after an edit anywhere else. The kind, the actor and the
// title already say a page was saved; the revision says what changed.
func excerptOfSave(save Save) string {
	return excerpt(save.Message)
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
