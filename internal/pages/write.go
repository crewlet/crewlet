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
	Page     Page
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
		Decide: func(*sql.Tx) (statelog.Decision, error) {
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
	return Written{
		Page: page, Revision: result.Position.Seq, ChangeID: opID,
		Outcome: result,
	}, nil
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

	at := s.now()
	opID := s.newSeqID()
	var out Page
	subject := PageSubject(pageID)

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Version != save.BaseVersion {
				return statelog.Decision{}, fmt.Errorf(
					"%w: this edit is against version %d and the page is at %d",
					ErrStaleVersion, save.BaseVersion, head.Version)
			}
			patch, kind, changed := s.patchOf(actor, &head, save, at)
			if !changed {
				// A NO-OP IS A SUCCESS, not an error: an update that
				// changes no field is one the caller should be told
				// landed.
				return statelog.Decision{}, nil
			}
			out = head
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
		Page: out, Revision: result.Position.Seq, ChangeID: opID,
		Outcome: result,
	}, nil
}

// Rename moves a page to a new address, arbitrated on the NEW title.
//
// ITS OWN OPERATION rather than a field of a save, because the address is what
// it contends for. It stamps the head's `scoped_through` and never its
// `version`: the row's broker expectation still matches its subject's last
// message and cannot be poisoned into permanent unwritability, and a read
// barrier compares the MAX of the two.
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
	if NormalizeTitle(head.Title) == NormalizeTitle(title) &&
		head.Container != "" {
		// THE SAME ADDRESS IS A NO-OP, and it is settled here rather
		// than by a create that would lose to its own claim: publishing
		// would take the title this page already holds and be refused
		// as taken, which reads as somebody else holding it.
		return Written{Page: head, ChangeID: opID}, nil
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
			current, err := readHeadTx(ctx, tx, pageID)
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
	return Written{
		Page: out, Revision: result.Position.Seq, ChangeID: opID,
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

	result, err := s.publish(ctx, statelog.Request{
		Subject:  statelog.Subject{Kind: string(KindPage), ID: pageID},
		Scope:    ScopeSet{Subject: true}.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			head, err := readHeadTx(ctx, tx, pageID)
			if err != nil {
				return statelog.Decision{}, err
			}
			out = head
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
		Page: out, Revision: result.Position.Seq, ChangeID: opID,
		Outcome: result,
	}, nil
}

// EnsureContainer creates a space if it is not there, or updates its settings.
func (s *Store) EnsureContainer(ctx context.Context, key, name, purpose string) (
	Container, error) {

	key = strings.ToUpper(strings.TrimSpace(key))
	if key == "" {
		return Container{}, invalid("container", "a container needs a key")
	}
	at := s.now()
	opID := s.newSeqID()
	subject := ContainerSubject(key)
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
		return Container{}, err
	}
	return out, nil
}

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
	if save.Watch != nil {
		before := len(head.Watchers) + len(head.Muted)
		applyWatch(head, actor.Handle, *save.Watch)
		if before != len(head.Watchers)+len(head.Muted) ||
			*save.Watch != !contains(head.Muted, actor.Handle) {
			patch.Watchers = head.Watchers
			patch.Muted = head.Muted
			kinds[ChangeWatchers] = true
		}
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
		Recipients: recipientsOf(page.Watchers, page.Muted),
		Mentions:   mentions,
		Excerpt:    text,
		Container:  page.Container,
		Title:      page.Title,
	}
}

// head reads one page outside a decision, for the checks a caller makes before
// publishing at all.
func (s *Store) head(ctx context.Context, pageID string) (Page, error) {
	var page Page
	err := s.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		got, err := readHeadTx(ctx, tx, pageID)
		page = got
		return err
	})
	return page, err
}

// watcherSet is the author plus whoever else was named, de-duplicated and
// sorted so two nodes forming the same set write the same rows.
func watcherSet(author string, extra []string) []string {
	out := cleanList(append([]string{author}, extra...))
	slicesSort(out)
	return out
}

// applyWatch adds or removes one handle, setting the mute on a removal.
//
// A REMOVAL IS A MUTE rather than a delete, because a page's watcher list is
// also its history of who cared: somebody who unwatches and is later mentioned
// should not be re-subscribed silently.
func applyWatch(page *Page, handle string, watch bool) {
	if handle == "" {
		return
	}
	if watch {
		page.Muted = without(page.Muted, handle)
		if !contains(page.Watchers, handle) {
			page.Watchers = append(page.Watchers, handle)
			slicesSort(page.Watchers)
		}
		return
	}
	if !contains(page.Muted, handle) {
		page.Muted = append(page.Muted, handle)
		slicesSort(page.Muted)
	}
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
