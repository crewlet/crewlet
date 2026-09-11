package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Saved views: the write, and the one invariant a container has.
//
// A view is a QUERY WITH A SHAPE, kept where a person will look for it again.
// Everything it holds is already expressible in the query grammar — what it
// adds is a name, a rendering and a place in a container's tab strip.
//
// # Why the params are PARSED at save and not merely stored
//
// A saved view that cannot be run is discovered by whoever opens it, weeks
// later, with no way to tell a typo from a grammar change. So a save runs the
// view's own parameters through [ParseQuery] and refuses the ones that do not
// parse, naming the key — which is the same refusal the caller would have got
// had they run the query instead of saving it.
//
// It is parsed against UTC rather than the company's zone deliberately: the
// date tokens (`today`, `this_week`) PARSE identically in every zone and only
// RESOLVE differently, and the resolved query is thrown away. A validator that
// needed the company's clock would be a validator that could answer differently
// on two nodes.
//
// # ONE DEFAULT PER CONTAINER, and why the applier is what makes that true
//
// The rule is about the ROWS rather than about any one write: "one per
// container can be the default" is false the moment two views say they are.
// The alternative — a companion append clearing the previous default — has a
// crash window in which both rows claim it, and a reader would then have to
// break the tie with a rule nobody wrote down.
//
// So a default-setting record declares its CONTAINER in its scope, and the
// applier clears the container's other defaults in the same transaction. The
// fan-out is unbounded in principle (a container may hold any number of views)
// which is exactly why the term is a container rather than an enumeration —
// [MaxScopeTerms]'s own rule.
//
// The container term rides a DEFAULT-SETTING write and nothing else, which is
// [Writer.UpdateTask]'s own rule for a project move: a scope states what the
// apply will write, and every other view save writes one row. Declaring the
// container on all of them would make an ordinary rename defer every write in
// the project behind it.

// WriteView saves a view, creating it or replacing it whole.
//
// WHOLE POST-STATE, like every other document object here: the record carries
// the view as it should be, and the applier's row is that document. A patch
// shape would need a merge rule per field, and a saved query is small enough
// that re-stating it costs nothing.
//
// The CALLER mints the id, which is what makes a retry idempotent: a verb that
// minted one would write a second view every time an `unknown` outcome was
// retried.
func (w *Writer) WriteView(ctx context.Context, opID string, view View) (WriteResult, error) {
	if err := checkView(&view); err != nil {
		return WriteResult{}, err
	}
	subject := ViewSubject(view.ID)
	home := containerScope(view.Container)
	// WHERE THE VIEW LIVES NOW, so a MOVE states both ends. A save may
	// change the container, and then the apply writes a row OUT of one
	// strip and INTO another — a scope naming only the destination would
	// let a write into the strip it left slip past a deferral that covers
	// it, which is [Writer.UpdateTask]'s own rule for a project move said
	// about a view.
	from, moved, err := w.viewHome(ctx, view.ID, home)
	if err != nil {
		return WriteResult{}, err
	}
	scope := ScopeSet{Subject: true, Container: home}
	switch {
	case view.Default:
		// A DEFAULT-SETTING APPLY CLEARS THE CONTAINER'S OTHERS, so it
		// states the container — see the file's head. Every sibling's
		// own path nests under this term by construction, because they
		// share a container and therefore share [containerScope].
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			current, held, err := readView(ctx, tx, view.ID)
			if err != nil {
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
				// AND THE SCOPE'S OWN PRE-READ IS VERIFIED HERE, in
				// the snapshot: it was taken outside one, so a view
				// that moved under this write would have been scoped
				// against a container it no longer lives in. Refused
				// rather than published, because an UNDER-declared
				// scope is the one thing a record may never carry —
				// and the caller's retry re-reads.
				if stored := containerScope(current.Container); stored != home &&
					(!moved || stored != from) {
					return statelog.Decision{}, fmt.Errorf("tracker: view %s "+
						"moved to %s while this save was being prepared, so "+
						"the record would not name the strip it is leaving — "+
						"read it again and save: %w",
						view.ID, stored, statelog.ErrConflict)
				}
				// A PROTECTED VIEW IS ITS OWNER'S, and that is the
				// whole of what "protected" means here: it stops a
				// shared board being rearranged under everybody,
				// which is a thing that happens rather than a
				// permission scheme this tracker deliberately does
				// not have.
				if current.Protected && current.Owner != "" &&
					current.Owner != w.Actor {
					return statelog.Decision{}, fmt.Errorf("tracker: view %s "+
						"is protected and belongs to %s — ask them to change "+
						"it, or save your own copy: %w",
						view.ID, current.Owner, statelog.ErrConflict)
				}
				// THE CREATION FACTS ARE THE STORED ROW'S, never the
				// caller's. A save that carried them would let a
				// second writer re-attribute a view somebody else
				// made, and nothing downstream could tell.
				post.V = DocumentVersion
				post.CreatedAt, post.CreatedBy = current.CreatedAt, current.CreatedBy
				if post.Rank == "" {
					post.Rank = current.Rank
				}
			}
			return w.decide(subject, OpPatch, scope, opID, post, nil, at)
		},
	})
}

// viewHome is the container a saved view lives in NOW, and whether that is a
// different one from where this save would put it.
//
// A SCOPE, NEVER AN EXPECTATION — [Writer.db]'s own rule. Nothing decided from
// this read is paired with a broker expectation: it only WIDENS the declared
// scope, and an over-declared scope is always safe where an under-declared one
// is the single claim a record may not make. It is verified inside the decide
// snapshot all the same, because a scope formed from a stale read and never
// checked is an under-declaration waiting for a race.
//
// A view this node does not hold reports no move: there is no strip for it to
// be leaving.
func (w *Writer) viewHome(ctx context.Context, id, home string) (string, bool, error) {
	if w.db == nil {
		return "", false, nil
	}
	var kind, container string
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT container_kind, container_id FROM tracker_views WHERE id = ?`, id)
		return row.Scan(&kind, &container)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("tracker: read where view %s lives: %w", id, err)
	}
	from := containerScope(Container{Kind: kind, ID: container})
	return from, from != home, nil
}

// checkView refuses a view that could not be rendered or could not be run.
func checkView(view *View) error {
	view.Name = strings.TrimSpace(view.Name)
	switch {
	case view.ID == "":
		return fmt.Errorf("tracker: a view write names no view id")
	case view.Name == "":
		return fmt.Errorf("tracker: view %s has no name, and a tab with no "+
			"label is one nobody can pick", view.ID)
	case len(view.Name) > MaxViewName:
		return fmt.Errorf("tracker: view %s's name is %d bytes and the "+
			"maximum is %d — shorten the name rather than having it cut",
			view.ID, len(view.Name), MaxViewName)
	case !view.Type.Valid():
		return fmt.Errorf("tracker: %q is not a view shape — the three are "+
			"%s, %s and %s", view.Type, ViewList, ViewBoard, ViewCalendar)
	case !ValidContainerKind(view.Container.Kind):
		return fmt.Errorf("tracker: %q is not a container a view can belong "+
			"to — the four are %s, %s, %s and %s", view.Container.Kind,
			ContainerWorkspace, ContainerProject, ContainerUnit, ContainerPerson)
	case view.Container.Kind == ContainerWorkspace && view.Container.ID != "":
		// THE WORKSPACE IS THE ONE CONTAINER WITH NO ID, exactly as it
		// has none in the scope grammar. An id here would make two
		// spellings of the top of the company, and a strip read with
		// the other spelling would come back empty.
		return fmt.Errorf("tracker: view %s names workspace container %q — the "+
			"workspace is the top of the company and carries no id",
			view.ID, view.Container.ID)
	case view.Container.Kind != ContainerWorkspace && view.Container.ID == "":
		return fmt.Errorf("tracker: view %s names a %s container with no id — "+
			"a view belongs to one project, unit or person, and one belonging "+
			"to nothing appears in no strip", view.ID, view.Container.Kind)
	case len(view.Params) > MaxViewParamKeys:
		// A GUARD ON THE MAP'S SIZE. What refuses a key that is not a
		// filter is [ParseQuery] below, which rejects an unknown
		// parameter rather than ignoring it.
		return fmt.Errorf("tracker: view %s carries %d query parameters and "+
			"the maximum is %d", view.ID, len(view.Params), MaxViewParamKeys)
	case paramsBytes(view.Params) > MaxViewParamsBytes:
		// AND ONE ON ITS WEIGHT, because thirty-two keys say nothing
		// about the size of a `q` or an `any` branch: one record with a
		// megabyte of saved query is a record every node stores, ships
		// in every snapshot and re-reads on every replay.
		return fmt.Errorf("tracker: view %s's query is %d bytes and the "+
			"maximum is %d — a saved view is a filter, not a document",
			view.ID, paramsBytes(view.Params), MaxViewParamsBytes)
	}
	// AND A VIEW MAY NOT CARRY A KEY THAT EXPANDS OR PAGES. `view` would
	// expand into itself, `cursor` would resume a page nobody asked for,
	// and a stored `read_level` would let a board silently downgrade a
	// seat's own read — see [expansionRefused].
	for _, key := range expansionRefused {
		if _, held := view.Params[key]; held {
			return fmt.Errorf("tracker: view %s carries %q, which a saved view "+
				"cannot: it is about the CALLER's own read — where it resumes, "+
				"how fresh it must be, or which view it came from — rather "+
				"than about the rows", view.ID, key)
		}
	}
	// THE PARAMS ARE THE QUERY, so they are parsed rather than measured.
	// The instant is this parse's own and is thrown away with the query:
	// what is being established is that the grammar accepts every key.
	params := make(MapParams, len(view.Params))
	for key, value := range view.Params {
		params[key] = value
	}
	if _, err := ParseQuery(params, time.Now().UTC(), time.UTC); err != nil {
		return fmt.Errorf("tracker: view %s's query does not parse, so saving "+
			"it would store a view nobody can open: %w", view.ID, err)
	}
	return nil
}

// Valid reports whether this is one of the three shapes.
//
// A CLOSED SET, and the type's own comment says there are no others: a fourth
// rendering is a screen that does not exist, so a view naming one is refused
// at the write rather than rendered as a blank tab.
func (t ViewType) Valid() bool {
	switch t {
	case ViewList, ViewBoard, ViewCalendar:
		return true
	}
	return false
}

// readView reads one saved view inside a write's own snapshot.
func readView(ctx context.Context, tx *sql.Tx, id string) (View, bool, error) {
	return readDocument(ctx, tx, ViewSubject(id),
		func(v *View, version uint64) { v.Version = version })
}

// nextViewRank mints a rank after the container's last view.
//
// AFTER, so a new view lands at the end of the strip rather than in front of
// whatever the person arranged. An empty container starts at [RankOrigin],
// which is the same origin every other order in this package starts from.
func nextViewRank(ctx context.Context, tx *sql.Tx, container Container) (Rank, error) {
	var last sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT MAX(rank) FROM tracker_views
		WHERE container_kind = ? AND container_id = ?`,
		container.Kind, container.ID).Scan(&last); err != nil {
		return "", fmt.Errorf("tracker: read %s %s's last view rank: %w",
			container.Kind, container.ID, err)
	}
	if !last.Valid || last.String == "" {
		return RankOrigin, nil
	}
	rank, err := KeyBetween(Rank(last.String), "")
	if err != nil {
		return "", fmt.Errorf("tracker: mint a rank after %q: %w", last.String, err)
	}
	return rank, nil
}

// containerScope is where a view's container sits in the scope alphabet.
//
// A PROJECT RESOLVES TO ITS OWN TERM and everything else to the workspace's,
// because the alphabet's container term is a project key or the workspace — a
// unit or a person is not one. It is the write's half of the rule [viewScope]
// states for the read, and the two MUST agree: a record filed under a path no
// probe reaches is a deferral a read never waits for, and such a read reports
// itself complete while missing the write.
func containerScope(container Container) string {
	if container.Kind == ContainerProject && container.ID != "" {
		return container.ID
	}
	return WorkspaceContainer
}

// paramsBytes is what a saved query weighs, keys included.
//
// THE KEYS COUNT because a caller stuffing thirty-two long keys with empty
// values writes exactly as much as one stuffing the values.
func paramsBytes(params map[string]string) int {
	total := 0
	for key, value := range params {
		total += len(key) + len(value)
	}
	return total
}
