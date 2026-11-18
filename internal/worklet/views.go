package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Saved views. A view is a query with a name, a rendering and a place in a
// container's tab strip; everything it holds is expressible in the query grammar.
//
// A save parses its params with [ParseQuery] ([checkView]), so a view that cannot
// run is refused at the write, naming the key, rather than found weeks later.
//
// One default per container is the applier's to keep: a default-setting record
// declares its container in its scope, and the apply clears the container's other
// defaults in the same transaction (a companion append clearing the old default
// has a crash window in which two rows claim it); a term, not an enumeration of
// the unbounded siblings ([MaxScopeTerms]). Only a default-setting save declares
// it ([Writer.UpdateTask]'s rule), so a rename does not defer the project's writes.

// ViewPrior is the stored view a save or delete was decided against: whether a
// view was held under the id and, if so, its owner and container.
//
// A save replaces a view whole, by id, so its authority is decided over the
// stored owner and container as well as the new ones; otherwise anyone who may
// write a view of their own could overwrite any id. The decide refuses a save
// whose stored view no longer matches, as [Writer.UpdateTask]'s If-Match does.
//
// A zero prior is a create, under an id the caller derived from the create's
// operation. A view the decide finds under that id is this create made before (a
// rebased re-run, new to the ledger, ADR-0029), answered from its row with
// nothing published rather than refused as a conflict.
type ViewPrior struct {
	// Held says a view existed under the id when the caller read it.
	Held bool

	// Owner and Container are that view's, and empty when none was held.
	Owner     string
	Container Container
}

// ViewPrior reads what a save or delete of view id would replace, for the caller
// to decide its authority on and pass back. It reads outside the snapshot, safe
// by [Writer.db]'s rule because the decide re-verifies it in its own; the save's
// scope is formed from it too ([Writer.WriteView]).
func (w *Writer) ViewPrior(ctx context.Context, id string) (ViewPrior, error) {
	if w.db.IsZero() {
		return ViewPrior{}, fmt.Errorf("worklet: this writer has no store, so it "+
			"cannot read what saving view %s would replace", id)
	}
	var prior ViewPrior
	var deleted sql.NullInt64
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT owner, container_kind, container_id, deleted_at FROM tracker_views
			WHERE id = ?`, id)
		return row.Scan(&prior.Owner, &prior.Container.Kind, &prior.Container.ID, &deleted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ViewPrior{}, nil
	case err != nil:
		return ViewPrior{}, fmt.Errorf("worklet: read what saving view %s would "+
			"replace: %w", id, err)
	}
	// A deleted view is no view to replace or delete again: its row stays only
	// to guard its version ([Writer.DeleteView]).
	if deleted.Valid {
		return ViewPrior{}, nil
	}
	prior.Held = true
	return prior, nil
}

// matches reports whether a view read in a decide's snapshot is still the one
// a prior describes, or the refusal naming what changed.
func (p ViewPrior) matches(id string, current View,
	held bool) error {

	switch {
	case !held && p.Held:
		return fmt.Errorf("worklet: view %s is no longer in this node's rows, so "+
			"the authority to replace it was decided on a view that is gone — "+
			"read it again and decide again: %w", id, statelog.ErrConflict)
	case held && (current.Owner != p.Owner || current.Container != p.Container):
		return fmt.Errorf("worklet: view %s changed hands or moved while this "+
			"save was being decided (it is %s's on %s %s now), so the authority "+
			"to replace it was decided on another view — read it again and "+
			"decide again: %w", id,
			ownerOrShared(current.Owner),
			current.Container.Kind, current.Container.ID, statelog.ErrConflict)
	}
	return nil
}

// ownerOrShared renders a view's owner for a refusal.
func ownerOrShared(owner string) string {
	if owner == "" {
		return "shared, nobody"
	}
	return owner
}

// WriteView saves a view, creating it or replacing it whole: the record carries
// the whole post-state, since a saved query is too small to need a per-field
// merge rule. The caller mints the id, so a retry after an `unknown` outcome
// rewrites the same view rather than saving a second. prior is what the caller
// read with [Writer.ViewPrior] and decided its authority on; a save whose stored
// view is no longer that one is refused.
func (w *Writer) WriteView(ctx context.Context, opID string, view View,
	prior ViewPrior) (WriteResult, error) {

	if err := checkView(&view); err != nil {
		return WriteResult{}, err
	}
	subject := ViewSubject(view.ID)
	home := containerScope(view.Container)
	// A move states both ends ([Writer.UpdateTask]'s rule for a project move):
	// naming only the destination would let a write into the strip the view
	// leaves slip past a deferral covering it. Taken from the prior, which the
	// decide verifies, so the scope is never an unchecked under-declaration.
	var from string
	var moved bool
	if prior.Held {
		from = containerScope(prior.Container)
		moved = from != home
	}
	scope := ScopeSet{Subject: true, Container: home}
	switch {
	case view.Default:
		// A default-setting apply clears the container's other defaults, so it
		// declares the container (the file's head); every sibling shares
		// [containerScope], so its path nests under this term.
		terms := []ScopeTerm{{Kind: TermContainer, ID: home}}
		if moved {
			terms = append(terms, ScopeTerm{Kind: TermContainer, ID: from})
		}
		scope = ScopeSet{Terms: terms}
	case moved:
		scope = ScopeSet{Terms: []ScopeTerm{
			{Kind: TermObject, Container: home, ID: subject.ID},
			{Kind: TermObject, Container: from, ID: subject.ID},
		}}
	}
	at := w.Now()
	view.UpdatedAt = at
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readView(ctx, tx, view.ID)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held && !prior.Held {
				// The create, made before ([ViewPrior], "A zero prior is a create"),
				// answered from its row even if deleted since: a re-run of a create
				// brings back no view somebody deleted.
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			// A view deleted since the prior was read is gone to a save, which
			// [ViewPrior.matches] refuses: replaced, it would come back.
			held = held && !current.Deleted()
			// Refused unless the view the authority was decided on is still the one
			// replaced, so the scope formed from the same prior still names the strip
			// it leaves: a record may never carry an under-declared scope, and a save
			// decided on somebody else's view is the takeover the prior stops. The
			// caller's retry re-reads.
			if err := prior.matches(view.ID, current, held); err != nil {
				return statelog.Decision{}, err
			}
			post := view
			switch {
			case !held:
				post.V = DocumentVersion
				post.CreatedAt, post.CreatedBy = at, w.Actor
				if post.Rank == "" {
					rank, err := nextViewRank(ctx, tx, post.Container)
					if err != nil {
						return statelog.Decision{}, err
					}
					post.Rank = rank
				}
			default:
				// A protected view is its owner's: it stops a shared board being
				// rearranged under everybody, and is not a permission scheme. Checked
				// against the actor, the one name a person saves under (iam.ActorFor),
				// whatever credential they held.
				if current.Protected && current.Owner != "" &&
					current.Owner != w.Actor {
					return statelog.Decision{}, forbidden("view %s is "+
						"protected and belongs to %s — ask them to change it, "+
						"or save your own copy", view.ID,
						current.Owner)
				}
				// The creation facts are the stored row's, never the caller's, or a
				// second writer could silently re-attribute somebody else's view.
				post.V = DocumentVersion
				post.CreatedAt, post.CreatedBy = current.CreatedAt, current.CreatedBy
				if post.Rank == "" {
					post.Rank = current.Rank
				}
			}
			return w.decide(ctx, tx, stamp, subject, OpPatch, ChangeViewSaved, scope, opID,
				post, nil, at)
		},
	})
}

// ErrNoView reports a view this node holds no live row for: never saved, or
// deleted. Its own sentinel, as [ErrNoFile] is, so a tool says "no such view"
// and an API answers 404.
var ErrNoView = errors.New("worklet: no such view")

// DeleteView takes a saved view out of every strip, listing and run.
//
// The delete is a stamp and the row stays, as a file removal does (files.go):
// every view upsert is version-guarded, so without the row a redelivered save
// would bring the view back. The record is the post-state with [View.DeletedAt]
// and [View.DeletedBy] set and the default cleared, and every read of the views
// leaves it out: [savedViews], [everySavedView], a run by `view=`, [Writer.ViewPrior].
//
// It is decided like a save: a prior that moved, changed hands or went away is
// refused as [Writer.WriteView] refuses it, and a protected view is its owner's
// to delete. A view already deleted when the decide reads it is answered from
// its row with nothing published (this delete re-run, or a concurrent one).
func (w *Writer) DeleteView(ctx context.Context, opID, id string,
	prior ViewPrior) (WriteResult, error) {

	if !prior.Held {
		return WriteResult{}, fmt.Errorf("%w: %s", ErrNoView, id)
	}
	subject := ViewSubject(id)
	// One row, in its strip: the default it clears is its own, so no sibling is
	// written ([settleDefaultView]); the container is the prior's, verified by
	// the decide.
	scope := ScopeSet{Subject: true, Container: containerScope(prior.Container)}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			current, held, err := readView(ctx, tx, id)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held && current.Deleted() {
				return statelog.Decision{Version: int64(current.Version)}, nil
			}
			if err := prior.matches(id, current, held); err != nil {
				return statelog.Decision{}, err
			}
			// A protected view is its owner's to delete: [Writer.WriteView]'s rule.
			if current.Protected && current.Owner != "" && current.Owner != w.Actor {
				return statelog.Decision{}, forbidden("view %s is protected and "+
					"belongs to %s: ask them to delete it", id, current.Owner)
			}
			post := current
			post.V, post.Version = DocumentVersion, 0
			post.Default = false
			post.UpdatedAt = at
			post.DeletedAt, post.DeletedBy = &at, w.Actor
			return w.decide(ctx, tx, stamp, subject, OpPatch, ChangeViewDeleted, scope, opID,
				post, nil, at)
		},
	})
}

// checkView refuses a view that could not be rendered or could not be run.
func checkView(view *View) error {
	view.Name = strings.TrimSpace(view.Name)
	switch {
	case view.ID == "":
		return invalid("a view write names no view id")
	case view.Name == "":
		return invalidSaying(caveat.ViewUnnamed(), "view %s has no name, and a tab with no "+
			"label is one nobody can pick", view.ID)
	case len(view.Name) > MaxViewName:
		return invalidSaying(caveat.ViewNameTooLong(), "view %s's name is %d bytes and the "+
			"maximum is %d — shorten the name rather than having it cut",
			view.ID, len(view.Name), MaxViewName)
	case !view.Type.Valid():
		return invalidSaying(caveat.ViewShapeUnknown(ViewTypeNames()), "%q is not a view shape: the shapes are "+
			"%s", view.Type, strings.Join(ViewTypeNames(), ", "))
	case !ValidContainerKind(view.Container.Kind):
		return invalid("%q is not a container a view can belong "+
			"to — the four are %s, %s, %s and %s", view.Container.Kind,
			ContainerWorkspace, ContainerProject, ContainerUnit, ContainerPerson)
	case view.Container.Kind == ContainerWorkspace && view.Container.ID != "":
		// The workspace carries no id, as in the scope grammar: two spellings of
		// the top of the company would leave a strip read under the other empty.
		return invalid("view %s names workspace container %q — the "+
			"workspace is the top of the company and carries no id",
			view.ID, view.Container.ID)
	case view.Container.Kind != ContainerWorkspace && view.Container.ID == "":
		return invalid("view %s names a %s container with no id — "+
			"a view belongs to one project, unit or person, and one belonging "+
			"to nothing appears in no strip", view.ID, view.Container.Kind)
	case view.Default && view.Owner != "":
		// The default is the container's landing tab for everybody, and a personal
		// view is in no strip but its owner's: accepted, the apply would clear the
		// shared default on the authority of a caller who needed only their own
		// record. A person's own view first is a pin.
		return invalidSaying(caveat.ViewPersonalDefault(), "view %s is personal (%s's) and cannot be "+
			"its container's default: the default is the landing tab for "+
			"everybody, and nobody else sees a personal view. Share it to "+
			"make it the default, or pin it to put it first for its owner",
			view.ID, view.Owner)
	case view.DeletedAt != nil || view.DeletedBy != "":
		// A save never deletes: the stamp is [Writer.DeleteView]'s, from its own
		// clock, not an instant and a name the caller chose.
		return invalid("view %s carries a deletion, which a save cannot: "+
			"delete it with %s", view.ID, DeleteWorkViewTool)
	case len(view.Params) > MaxViewParamKeys:
		// Bounds the map's size; [ParseQuery] below refuses a key that is not a
		// filter.
		return invalidSaying(caveat.ViewFiltersTooLarge(), "view %s carries %d query parameters and "+
			"the maximum is %d", view.ID, len(view.Params), MaxViewParamKeys)
	case paramsBytes(view.Params) > MaxViewParamsBytes:
		// And its weight: a `q` or `any` branch can be large under few keys, and
		// every data node stores, snapshots and replays the record.
		return invalidSaying(caveat.ViewFiltersTooLarge(), "view %s's query is %d bytes and the "+
			"maximum is %d — a saved view is a filter, not a document",
			view.ID, paramsBytes(view.Params), MaxViewParamsBytes)
	}
	// No key that expands or pages: `view` would expand into itself, `cursor`
	// resume a page nobody asked for, and a stored `read_level` silently
	// downgrade a seat's own read ([expansionRefused]).
	for _, key := range expansionRefused {
		if _, held := view.Params[key]; held {
			return invalid("view %s carries %q, which a saved view "+
				"cannot: it is about the CALLER's own read — where it resumes, "+
				"how fresh it must be, or which view it came from — rather "+
				"than about the rows", view.ID, key)
		}
	}
	// Parsed, not measured: the grammar must accept every key. Against UTC, not
	// the company's zone: date tokens parse alike in every zone and only resolve
	// differently, the resolved query is discarded, and a validator needing the
	// company's clock could answer differently on two nodes.
	params := make(MapParams, len(view.Params))
	for key, value := range view.Params {
		params[key] = value
	}
	if _, err := ParseQuery(params, time.Now().UTC(), time.UTC); err != nil {
		return invalidSaying(caveat.ViewFilterUnreadable(), "view %s's query does not parse, so saving "+
			"it would store a view nobody can open: %w", view.ID, err)
	}
	return nil
}

// Valid reports whether this is one of the shapes [ViewTypes] names. The set is
// closed: a view naming another shape is refused at the write rather than
// rendered as a blank tab.
func (t ViewType) Valid() bool { return slices.Contains(ViewTypes, t) }

// ViewTypeNames is [ViewTypes] as the strings a schema enum and a refusal use.
func ViewTypeNames() []string {
	out := make([]string, len(ViewTypes))
	for i, t := range ViewTypes {
		out[i] = string(t)
	}
	return out
}

// readView reads one saved view inside a write's own snapshot.
func readView(ctx context.Context, tx *sql.Tx, id string) (View, bool, error) {
	return readDocument(ctx, tx, ViewSubject(id),
		func(v *View, version uint64) { v.Version = version })
}

// nextViewRank mints a rank after the container's last view, so a new view lands
// at the end of the strip rather than in front of what the person arranged. An
// empty container starts at [RankOrigin].
func nextViewRank(ctx context.Context, tx *sql.Tx, container Container) (Rank, error) {
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(rank) FROM tracker_views
		WHERE container_kind = ? AND container_id = ?`,
		container.Kind, container.ID).Scan(&last); err != nil {
		return "", fmt.Errorf("worklet: read %s %s's last view rank: %w",
			container.Kind, container.ID, err)
	}
	if !last.Valid || last.String == "" {
		return RankOrigin, nil
	}
	rank, err := KeyBetween(Rank(last.String), "")
	if err != nil {
		return "", fmt.Errorf("worklet: mint a rank after %q: %w", last.String, err)
	}
	return rank, nil
}

// containerScope is where a view's container sits in the scope alphabet: a
// project's own term, else the workspace's (a unit or a person is no container
// term). [viewScope] MUST agree for the read: a record filed under a path no
// probe reaches is a deferral no read waits for, so the read reports itself
// complete while missing the write.
func containerScope(container Container) string {
	if container.Kind == ContainerProject && container.ID != "" {
		return container.ID
	}
	return WorkspaceContainer
}

// paramsBytes is what a saved query weighs, keys included: long keys with empty
// values write as much as long values.
func paramsBytes(params map[string]string) int {
	total := 0
	for key, value := range params {
		total += len(key) + len(value)
	}
	return total
}
