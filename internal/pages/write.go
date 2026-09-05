package pages

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// NewPage is what a caller supplies to write a page.
type NewPage struct {
	Container string
	Title     string
	Body      string

	ParentID string
	Labels   []string

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
}

// Create writes a new page.
//
// THREE KEYS, IN ORDER: the title claim, then the page, then the change. A
// crash after the claim leaves an ORPHAN CLAIM, which the grace rule below
// lets the next writer step over and the hourly sweep removes — the other
// order would let two pages share a title, and a title is an address.
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
	page := Page{
		V: DocumentVersion, ID: s.newID(), Container: container,
		ParentID: strings.TrimSpace(in.ParentID),
		Title:    strings.Join(strings.Fields(in.Title), " "),
		Body:     in.Body, Status: in.Status, Labels: labels,
		Version: 1, Author: actor.Name(), CreatedAt: at, UpdatedAt: at,
	}
	addWatcher(&page, actor.Handle)
	for _, w := range cleanList(in.Watchers) {
		addWatcher(&page, w)
	}

	if err := s.claimTitle(ctx, container, page.Title, page.ID, at); err != nil {
		return Written{}, err
	}

	change := s.change(actor, page, ChangeCreated, at)
	change.Quiet = in.Quiet
	change.Excerpt = excerpt(firstLine(in.Body, page.Title))
	page.LastChange = &change

	// The first revision, so a history starts at version 1 rather than at
	// the first EDIT — a page whose original text was never a revision has
	// no way back to what it said when it was written.
	if err := s.writeRevision(ctx, Revision{
		V: DocumentVersion, ID: s.newID(), PageID: page.ID, Version: 1,
		Title: page.Title, Body: page.Body, Message: in.Message,
		Author: actor.Name(), CreatedAt: at,
	}); err != nil {
		return Written{}, err
	}

	data, err := EncodePage(page)
	if err != nil {
		return Written{}, err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyPages, PageKey(page.ID), data)
	if err != nil {
		return Written{}, fmt.Errorf("pages: create %q: %w", page.Title, err)
	}
	if !created {
		return Written{}, fmt.Errorf("pages: page id %s already exists", page.ID)
	}
	revision, err := s.revisionOf(ctx, PageKey(page.ID))
	if err != nil {
		return Written{}, err
	}
	change.HeadRevision = revision
	if err := s.writeChange(ctx, change); err != nil {
		return Written{}, err
	}
	return Written{Page: page, Revision: revision, ChangeID: change.ID}, nil
}

// claimTitle takes a container's hold on a title, first-writer-wins.
//
// # The grace rule, and why a crash never locks a title
//
// A claim whose page does not exist is either a create in flight or one that
// died between its two keys, and within [OrphanGrace] those look identical —
// so the claim is honoured. Past it, only a crash explains it, and the claim
// is stepped over. Without this, a node dying mid-create would make a title
// unusable until the hourly sweep ran, and the person retrying would be told
// their own half-written page owns the name.
func (s *Store) claimTitle(ctx context.Context, container, title, pageID string, at time.Time) error {
	key := TitleKey(container, title)
	claim := TitleClaim{
		V: DocumentVersion, Container: container,
		Title: NormalizeTitle(title), PageID: pageID, CreatedAt: at,
	}
	data, err := EncodeClaim(claim)
	if err != nil {
		return err
	}
	for range casRounds {
		created, err := s.docs.CreateDocument(ctx, coord.FamilyPages, key, data)
		if err != nil {
			return fmt.Errorf("pages: claim the title %q in %s: %w", title, container, err)
		}
		if created {
			return nil
		}
		rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
		if err != nil {
			return fmt.Errorf("pages: read the claim on %q in %s: %w", title, container, err)
		}
		if !found {
			// Released between the create and the read. Try again.
			continue
		}
		existing, err := DecodeClaim(rec.Value)
		if err != nil {
			return err
		}
		if existing.PageID == pageID {
			// Ours already — a retry of the same create.
			return nil
		}
		stale, err := s.claimIsOrphan(ctx, existing, at)
		if err != nil {
			return err
		}
		if !stale {
			return fmt.Errorf("%w: %q in %s belongs to page %s",
				ErrTitleTaken, title, container, existing.PageID)
		}
		// AN ORPHAN. Step over it at its own revision, so a claim written
		// between the read and the write wins and this loop re-reads.
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyPages, key, data, rec.Version)
		if err != nil {
			return fmt.Errorf("pages: take over the claim on %q: %w", title, err)
		}
		if ok {
			log.InfoContext(ctx, "pages_orphan_title_claim_taken",
				"container", container, "title", title,
				"was", existing.PageID, "now", pageID,
				"detail", "the claim's page never landed and it is past the grace")
			return nil
		}
	}
	return fmt.Errorf("%w: the claim on %q in %s", ErrConflict, title, container)
}

// claimIsOrphan reports whether a claim's page never landed and the grace has
// passed.
func (s *Store) claimIsOrphan(ctx context.Context, claim TitleClaim, at time.Time) (bool, error) {
	if at.Sub(claim.CreatedAt) < OrphanGrace {
		return false, nil
	}
	_, found, err := s.docs.Document(ctx, coord.FamilyPages, PageKey(claim.PageID))
	if err != nil {
		// UNKNOWN IS NOT ORPHANED. A store that could not be reached must
		// never let one page take another's title: the claim is honoured
		// and the caller is told the title is held, which is recoverable,
		// where taking it over is not.
		return false, fmt.Errorf("pages: check the page behind a title claim: %w", err)
	}
	return !found, nil
}

// writeRevision creates one immutable revision, first-writer-wins on its
// version.
//
// THE VERSION IS THE KEY, so two nodes saving at once both try the same one
// and exactly one wins — the loser knows to re-read rather than having
// written a second revision nobody can order.
func (s *Store) writeRevision(ctx context.Context, rev Revision) error {
	data, err := EncodeRevision(rev)
	if err != nil {
		return err
	}
	created, err := s.docs.CreateDocument(ctx, coord.FamilyPages,
		RevisionKey(rev.PageID, rev.Version), data)
	if err != nil {
		return fmt.Errorf("pages: write revision %d of %s: %w", rev.Version, rev.PageID, err)
	}
	if !created {
		return fmt.Errorf("%w: revision %d of this page already exists",
			ErrStaleVersion, rev.Version)
	}
	return nil
}

// Save is what a caller supplies to edit a page.
type Save struct {
	// BaseVersion is the version the editor read. REQUIRED, unlike a work
	// item's optional If-Match: a wiki's worst failure is silently
	// overwriting a paragraph somebody else just wrote, and there is no
	// per-field merge that makes that safe for prose.
	BaseVersion int

	Title *string
	Body  *string

	ParentID *string
	Labels   *[]string
	Status   *Status

	// Message is the one-line note recorded with this revision.
	Message string

	// Watch adds or removes the actor, setting the mute on a removal.
	Watch *bool

	Quiet bool
}

// SavePage applies an edit, compare-and-set against the head.
//
// THREE KEYS, IN ORDER: the revision, then the head, then the change. A crash
// after the revision leaves one above the page's version, which the next
// writer treats as an orphan past the grace and overwrites — so a crash never
// locks a page until the hourly sweep.
func (s *Store) SavePage(ctx context.Context, actor Actor, pageID string, save Save) (Written, error) {
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

	key := PageKey(pageID)
	for range casRounds {
		rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
		if err != nil {
			return Written{}, fmt.Errorf("pages: read %s: %w", pageID, err)
		}
		if !found {
			return Written{}, fmt.Errorf("%w: page %s", ErrNotFound, pageID)
		}
		page, err := DecodePage(rec.Value)
		if err != nil {
			return Written{}, err
		}
		if page.Version != save.BaseVersion {
			return Written{}, fmt.Errorf(
				"%w: you edited version %d and it is now at version %d. Read it "+
					"again and re-apply your change",
				ErrStaleVersion, save.BaseVersion, page.Version)
		}

		before := page
		change, err := s.applySave(ctx, actor, &page, save)
		if err != nil {
			return Written{}, err
		}
		if change.Kind == "" {
			return Written{Page: page, Revision: rec.Version}, nil
		}

		at := change.CreatedAt
		page.Version++
		page.UpdatedAt = at
		change.Snapshot = snapshotOf(page)
		page.LastChange = &change

		// THE REVISION FIRST, at the NEW version. A refusal here means
		// another writer took this version — this save loses, re-reads and
		// re-applies, which is exactly what its base-version check would
		// have told it a moment later.
		if err := s.writeRevisionOverOrphan(ctx, Revision{
			V: DocumentVersion, ID: s.newID(), PageID: page.ID, Version: page.Version,
			Title: page.Title, Body: page.Body, Message: save.Message,
			Author: actor.Name(), CreatedAt: at,
		}, at); err != nil {
			return Written{}, err
		}

		data, err := EncodePage(page)
		if err != nil {
			return Written{}, err
		}
		ok, err := s.docs.UpdateDocument(ctx, coord.FamilyPages, key, data, rec.Version)
		if err != nil {
			return Written{}, fmt.Errorf("pages: write %s: %w", pageID, err)
		}
		if !ok {
			// The head moved between the read and the write. The base
			// version check on the next round is what reports it.
			continue
		}

		// The title claim moves LAST among the page's own keys, so a crash
		// leaves the old claim held — which blocks only a third page
		// taking that name, where releasing first would let one take it
		// and leave this page unreachable by title.
		if before.Title != page.Title {
			s.releaseTitle(ctx, before.Container, before.Title, page.ID)
		}

		revision, err := s.revisionOf(ctx, key)
		if err != nil {
			return Written{}, err
		}
		change.HeadRevision = revision
		if err := s.writeChange(ctx, change); err != nil {
			return Written{}, err
		}
		return Written{Page: page, Revision: revision, ChangeID: change.ID}, nil
	}
	return Written{}, fmt.Errorf("%w: %s lost %d races in a row", ErrConflict, pageID, casRounds)
}

// writeRevisionOverOrphan writes a revision, stepping over one a crashed
// writer left behind.
//
// A revision at this version whose page is still BELOW it can only be a
// writer that died between its two keys — the head is the authority on which
// versions exist. Within the grace that is indistinguishable from a save in
// flight and the refusal stands; past it, the orphan is overwritten, so a
// crash never locks a page until the sweep.
func (s *Store) writeRevisionOverOrphan(ctx context.Context, rev Revision, at time.Time) error {
	err := s.writeRevision(ctx, rev)
	if err == nil {
		return nil
	}
	rec, found, readErr := s.docs.Document(ctx, coord.FamilyPages,
		RevisionKey(rev.PageID, rev.Version))
	if readErr != nil || !found {
		return err
	}
	existing, decodeErr := DecodeRevision(rec.Value)
	if decodeErr != nil || at.Sub(existing.CreatedAt) < OrphanGrace {
		return err
	}
	data, encodeErr := EncodeRevision(rev)
	if encodeErr != nil {
		return encodeErr
	}
	ok, updateErr := s.docs.UpdateDocument(ctx, coord.FamilyPages,
		RevisionKey(rev.PageID, rev.Version), data, rec.Version)
	if updateErr != nil || !ok {
		return err
	}
	log.InfoContext(ctx, "pages_orphan_revision_overwritten",
		"page", rev.PageID, "version", rev.Version,
		"detail", "a writer died between its revision and its head write, past "+
			"the grace; the page is editable again rather than locked until the sweep")
	return nil
}

// applySave decides what a save changes, and refuses what it may not.
func (s *Store) applySave(ctx context.Context, actor Actor, page *Page, save Save) (Change, error) {
	at := s.now()
	fields := map[string]Delta{}
	kinds := map[ChangeKind]bool{}

	if save.Title != nil {
		title := strings.Join(strings.Fields(*save.Title), " ")
		if err := s.checkTitle(title); err != nil {
			return Change{}, err
		}
		if NormalizeTitle(title) != NormalizeTitle(page.Title) {
			// THE NEW CLAIM IS TAKEN FIRST. A rename that released before
			// claiming would leave the page unreachable by title for as
			// long as the two writes take, and permanently if the second
			// one lost.
			if err := s.claimTitle(ctx, page.Container, title, page.ID, at); err != nil {
				return Change{}, err
			}
			kinds[ChangeRenamed] = true
		}
		if title != page.Title {
			fields["title"] = Delta{From: page.Title, To: title}
			kinds[ChangeRenamed] = true
			page.Title = title
		}
	}
	if save.Body != nil {
		if err := s.checkBody(*save.Body); err != nil {
			return Change{}, err
		}
		if *save.Body != page.Body {
			// THE BODIES ARE NOT IN THE DELTA. Two 512 KiB copies per save
			// in an ageless bucket, for ever, to render a line nobody
			// reads — the revision history is where the text lives.
			fields["body"] = Delta{From: "(previous)", To: excerpt(firstLine(*save.Body, ""))}
			kinds[ChangeSaved] = true
			page.Body = *save.Body
		}
	}
	if save.ParentID != nil && *save.ParentID != page.ParentID {
		fields["parent"] = Delta{From: page.ParentID, To: *save.ParentID}
		kinds[ChangeMoved] = true
		page.ParentID = strings.TrimSpace(*save.ParentID)
	}
	if save.Labels != nil {
		labels := cleanList(*save.Labels)
		if err := s.checkLabels(labels); err != nil {
			return Change{}, err
		}
		if !slices.Equal(labels, page.Labels) {
			fields["labels"] = Delta{
				From: strings.Join(page.Labels, ", "), To: strings.Join(labels, ", "),
			}
			kinds[ChangeLabels] = true
			page.Labels = labels
		}
	}
	if save.Status != nil && *save.Status != page.Status {
		if !save.Status.Valid() {
			return Change{}, invalid("status", "%q is not one of %v", *save.Status, Statuses())
		}
		fields["status"] = Delta{From: string(page.Status), To: string(*save.Status)}
		kinds[ChangeStatus] = true
		page.Status = *save.Status
		if page.Status == StatusTrashed {
			trashed := at
			page.TrashedAt = &trashed
		} else {
			page.TrashedAt = nil
		}
	}
	if save.Watch != nil {
		s.applyWatch(actor, page, *save.Watch, kinds)
	}
	if len(kinds) == 0 {
		return Change{}, nil
	}

	// AN EDIT SUBSCRIBES ITS AUTHOR. The Confluence parser's rule, kept:
	// somebody who wrote a paragraph wants to know when it is rewritten.
	addWatcher(page, actor.Handle)
	page.Author = actor.Name()
	if len(page.Watchers) > MaxWatchers {
		page.Watchers = page.Watchers[len(page.Watchers)-MaxWatchers:]
	}

	change := s.change(actor, *page, dominantKind(kinds), at)
	change.Fields = fields
	change.Quiet = save.Quiet
	change.Excerpt = excerptOfFields(fields, save.Message)
	change.CreatedAt = at
	return change, nil
}

// applyWatch adds or removes the actor, setting the mute on a removal.
func (s *Store) applyWatch(actor Actor, page *Page, watch bool, kinds map[ChangeKind]bool) {
	handle := actor.Handle
	if handle == "" {
		return
	}
	if watch {
		page.Muted = slices.DeleteFunc(page.Muted, func(h string) bool { return h == handle })
		if !slices.Contains(page.Watchers, handle) {
			page.Watchers = append(page.Watchers, handle)
			kinds[ChangeWatchers] = true
		}
		return
	}
	before := len(page.Watchers)
	page.Watchers = slices.DeleteFunc(page.Watchers, func(h string) bool { return h == handle })
	if !slices.Contains(page.Muted, handle) {
		page.Muted = append(page.Muted, handle)
	}
	if before != len(page.Watchers) {
		kinds[ChangeWatchers] = true
	}
}

// releaseTitle drops a claim this page no longer holds.
//
// BEST EFFORT, and deliberately: a failure leaves an orphan claim that blocks
// only a third page taking that name, and the hourly sweep removes it. Making
// the rename fail here would undo an edit that has already landed.
func (s *Store) releaseTitle(ctx context.Context, container, title, pageID string) {
	key := TitleKey(container, title)
	rec, found, err := s.docs.Document(ctx, coord.FamilyPages, key)
	if err != nil || !found {
		return
	}
	claim, err := DecodeClaim(rec.Value)
	if err != nil || claim.PageID != pageID {
		// Somebody else's claim now, which a rename race can produce.
		// Purging it would take a title from the page that holds it.
		return
	}
	if _, err := s.docs.PurgeDocument(ctx, coord.FamilyPages, key, rec.Version); err != nil {
		log.WarnContext(ctx, "pages_title_release_failed", "container", container,
			"title", title, "error", err.Error(),
			"detail", "an orphan claim remains; it blocks only a third page "+
				"taking this name and the sweep removes it")
	}
}

// dominantKind picks the one kind a change is reported as, ordered by what a
// RECIPIENT most needs to be told.
func dominantKind(kinds map[ChangeKind]bool) ChangeKind {
	for _, kind := range []ChangeKind{
		ChangeRemoved, ChangeStatus, ChangeRenamed, ChangeMoved,
		ChangeSaved, ChangeLabels, ChangeWatchers,
	} {
		if kinds[kind] {
			return kind
		}
	}
	return ""
}

// excerptOfFields renders a save into a card's lead line.
//
// THE AUTHOR'S MESSAGE WINS when they wrote one: it is what they chose to say
// about the edit, and a generated "body (previous) → ..." beside it would
// bury it.
func excerptOfFields(fields map[string]Delta, message string) string {
	if message = strings.TrimSpace(message); message != "" {
		return excerpt(message)
	}
	if len(fields) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fields))
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		d := fields[name]
		switch {
		case d.From == "":
			parts = append(parts, fmt.Sprintf("%s set to %s", name, d.To))
		case d.To == "":
			parts = append(parts, fmt.Sprintf("%s cleared", name))
		default:
			parts = append(parts, fmt.Sprintf("%s %s → %s", name, d.From, d.To))
		}
	}
	return excerpt(strings.Join(parts, "; "))
}

// firstLine is a body's opening, for an excerpt, falling back to a title.
func firstLine(body, fallback string) string {
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(line, "#> "))
		if line != "" {
			return line
		}
	}
	return fallback
}
