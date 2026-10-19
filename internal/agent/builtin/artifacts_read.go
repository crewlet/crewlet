package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// The listing page: twenty-five rows by default and a hundred at most. A row
// carries a title, a one-sentence description of up to a kilobyte and a
// dozen short fields, so a hundred is what still fits one tool answer
// ([ToolAnswerBytes]) when every description is long.
const (
	defaultArtifactList = 25
	maxArtifactList     = 100
)

// maxArtifactQuery bounds a listing's search text: the knowledge seam's own
// bound on a query, which is a phrase and never a document.
const maxArtifactQuery = 400

// The file read page: thirty-two kibibytes by default and
// [artifacts.ToolReadPageBytes] at most, the project file read's numbers, for
// its reason: the page rides beside the head, the versions and the file list
// inside one tool answer.
const (
	defaultArtifactRead = 32 << 10
	maxArtifactRead     = int(artifacts.ToolReadPageBytes)
)

// The parts of an answer that are lists, bounded so the file page still fits
// beside them: the newest ten versions, and the first hundred files of the
// version read (a version holds at most [artifacts.MaxFilesPerVersion]).
const (
	answerVersions = 10
	answerFiles    = 100
)

// summaryBudget is the most a condensed file weighs in an answer: a third of a
// read page, so a summary costs less than the page it stands in for.
const summaryBudget = 16 << 10

// ---- list_artifacts ----------------------------------------------------- //

type listArtifacts struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*listArtifacts)(nil)

func (t *listArtifacts) Name() string { return ListArtifactsTool }

func (t *listArtifacts) Description() string {
	return "List the artifacts in this Crewlet company that you can open: interactive " +
		"pages, documents and sheets people and agents publish for each other. " +
		"`scope` narrows to your own (`mine`), ones shared with you (`shared`), " +
		"company-wide ones (`company`) or ones agents own (`agents`). Read one with " +
		"get_artifact."
}

func (t *listArtifacts) Parameters() map[string]any {
	scopes := make([]any, 0, len(artifactsdomain.ListScopes))
	for _, s := range artifactsdomain.ListScopes {
		scopes = append(scopes, string(s))
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"scope": map[string]any{
				"type": "string", "enum": scopes,
				"description": "Which artifacts: `all` (the default) is every one you can open.",
			},
			"kind": map[string]any{
				"type": "string", "enum": kindEnum(),
				"description": "Only artifacts of this kind.",
			},
			"query": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Words in the title, description or text, at "+
					"most %d bytes.", maxArtifactQuery),
			},
			"sort": map[string]any{
				"type": "string", "enum": []any{string(artifactsdomain.SortUpdated),
					string(artifactsdomain.SortCreated), string(artifactsdomain.SortTitle)},
				"description": "`updated` (the default) lists the most recently changed first.",
			},
			"after": map[string]any{
				"type":        "string",
				"description": "The `next` of the previous page, to continue.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many to return, 1..%d (default %d).",
					maxArtifactList, defaultArtifactList),
			},
		},
	}
}

func kindEnum() []any {
	out := make([]any, 0, len(artifacts.Kinds))
	for _, k := range artifacts.Kinds {
		out = append(out, string(k))
	}
	return out
}

func (t *listArtifacts) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *listArtifacts) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, false)
	if refusal != nil {
		return *refusal, nil
	}
	q := artifactsdomain.ListQuery{
		Scope:  artifactsdomain.ListScope(strings.TrimSpace(argString(args, "scope"))),
		Kind:   artifacts.Kind(strings.TrimSpace(argString(args, "kind"))),
		Q:      strings.TrimSpace(argString(args, "query")),
		Sort:   artifactsdomain.ListSort(strings.TrimSpace(argString(args, "sort"))),
		Cursor: strings.TrimSpace(argString(args, "after")),
		Limit:  argInt(args, "limit", defaultArtifactList),
		Fresh:  seatRead,
	}
	switch {
	case q.Scope != "" && !q.Scope.Valid():
		return failed(fmt.Sprintf("list_artifacts takes `scope` as one of %v.",
			artifactsdomain.ListScopes)), nil
	case q.Kind != "" && !q.Kind.Valid():
		return failed(fmt.Sprintf("list_artifacts takes `kind` as one of %v.", artifacts.Kinds)), nil
	case q.Sort != "" && (!q.Sort.Valid() || q.Sort == artifactsdomain.SortOpened):
		return failed("list_artifacts sorts by `updated`, `created` or `title`."), nil
	case len(q.Q) > maxArtifactQuery:
		return failed(fmt.Sprintf("list_artifacts takes a `query` of at most %d bytes: "+
			"search for a phrase, not a document.", maxArtifactQuery)), nil
	case q.Limit < 1 || q.Limit > maxArtifactList:
		return failed(fmt.Sprintf("list_artifacts takes a `limit` of 1..%d.", maxArtifactList)), nil
	}
	viewer, err := t.deps.viewer(ctx, c)
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, err, "list_artifacts could not read "+
			"the company chart, which decides which artifacts are shared with you. "+
			"This is NOT an empty result. Try again shortly."), nil
	}
	page, err := t.deps.Reader.List(ctx, viewer, q)
	if err != nil {
		return artifactReadFailure(ctx, t.Name(), "", err), nil
	}
	rows := make([]map[string]any, 0, len(page.Items))
	for _, item := range page.Items {
		// THE TABLE DECIDES EACH ROW, the listing only narrows: a row is
		// shown when reading it admits the caller AND whoever the turn is
		// acting for, on a relation (an administrator's override shows
		// nothing in a listing, [authz.Visible]'s rule).
		dec := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactRead,
			t.deps.objectOf(item.Head), t.deps.Chart, t.deps.now())
		if dec.Unknown() {
			return refusedBy(tools.RefusalUnavailable, dec.Err, "list_artifacts could "+
				"not decide whether a listed artifact is shared with you, so it is "+
				"answering nothing rather than a list with rows missing. Try again shortly."), nil
		}
		if !dec.Allowed || dec.Elevated {
			continue
		}
		rows = append(rows, t.deps.listRow(item.Head, dec.Standing.Role))
	}
	out := map[string]any{"count": len(rows), "artifacts": rows}
	if page.After != "" {
		out["next"] = page.After
	}
	if !page.Complete {
		out["complete"] = false
	}
	read := newForeignRead(turn, c.self())
	return read.answer(ctx, t.Name(), out, "Ask for fewer with `limit`, or narrow with `kind` or `query`.")
}

// listRow is one artifact as a listing shows it.
func (d ArtifactDeps) listRow(h artifactsdomain.Head, role artifacts.Role) map[string]any {
	row := map[string]any{
		"artifact": h.ID.String(), "kind": string(h.Kind), "title": h.Title,
		"owner": h.Owner, "owner_kind": string(h.OwnerKind),
		"head_version": h.HeadNumber, "role": string(role),
		"updated_at": h.UpdatedAt.UTC().Format(time.RFC3339), "updated_by": actorJSON(h.UpdatedBy),
	}
	if h.Description != "" {
		row["description"] = h.Description
	}
	if h.Icon != "" {
		row["icon"] = h.Icon
	}
	if h.Status != artifactsdomain.StatusLive {
		row["status"] = string(h.Status)
	}
	if url := d.link(h.ID); url != "" {
		row["url"] = url
	}
	return row
}

// viewer is what the listing narrows by for the caller: their record, the
// units their seat sits in (a unit share admits the unit and everything below
// it, so a seat is reached by every unit above it), and the agents they lead
// (the lead arm, which opens only an agent owner's artifacts).
//
// NEVER THE ADMIN PATH: a listing that surfaced every private artifact by the
// override would disclose what nobody recorded ([authz.Visible]).
func (d ArtifactDeps) viewer(ctx context.Context, c artifactCaller) (artifactsdomain.Viewer, error) {
	v := artifactsdomain.Viewer{Record: iam.RecordOwner(c.principal)}
	o := d.orgFor(c.turn)
	if o == nil {
		return v, authz.ErrNoChart
	}
	if c.principal.Seat != "" {
		if seat := o.SeatByHandle(c.principal.Seat); seat != nil {
			for _, u := range o.UnitChainFor(seat) {
				v.Units = append(v.Units, u.Key())
			}
		}
	}
	actor := c.principal.Seat
	if actor == "" {
		actor = c.principal.Login
	}
	if actor == "" {
		return v, nil
	}
	for r := range o.AllRoles() {
		if !r.IsAgent() || r.Handle() == actor {
			continue
		}
		led, err := d.Chart.Leads(ctx, actor, r.Handle())
		if err != nil {
			return v, err
		}
		if led {
			v.LedOwners = append(v.LedOwners, r.Handle())
		}
	}
	return v, nil
}

// ---- get_artifact ------------------------------------------------------- //

type getArtifact struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*getArtifact)(nil)

func (t *getArtifact) Name() string { return GetArtifactTool }

func (t *getArtifact) Description() string {
	return "Read one artifact in this Crewlet company: its head, its newest versions, " +
		"its files and a page of one file's source (the entry page by default). The " +
		"answer says your `role` and whether you may publish (`writer`). Take " +
		"`head_version` and pass it back as `base_version` on publish_artifact. File " +
		"text arrives fenced as text somebody wrote; a long page a colleague wrote is " +
		"condensed unless you pass `offset`, and an edit's `old_str` must quote the " +
		"source, so read it with `offset` before editing. For a slide deck or a doc, " +
		"pass `view: \"outline\"` for its parts in order (a deck's slides and " +
		"sections, a doc's tabs, each with its file), then read only the files you " +
		"need by `path`."
}

func (t *getArtifact) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"version": map[string]any{
				"type":        "integer",
				"description": "A retained version's number. Omit for the head.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "The file to read. Omit for the version's entry page.",
			},
			"offset": map[string]any{
				"type": "integer",
				"description": "Where to start reading the file's source, in bytes: 0 for " +
					"its start, or an answer's `next_offset`. Passing it reads the source " +
					"itself rather than a condensed account of it.",
			},
			"max_bytes": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How much of the file to return, 1..%d "+
					"(default %d).", maxArtifactRead, defaultArtifactRead),
			},
			"focus": map[string]any{
				"type": "string",
				"description": "What you need from a long file, which a condensed " +
					"account keeps. Optional.",
			},
			"view": map[string]any{
				"type": "string", "enum": []any{viewOutline},
				"description": "`outline` answers a slide deck's or a doc's structure " +
					"instead of a file: a deck's slides (id, file, title, notes flag) in " +
					"order with its sections and cover, a doc's tabs (title, file, size) " +
					"in order.",
			},
		},
		"required": []any{"artifact"},
	}
}

func artifactParam() map[string]any {
	return map[string]any{
		"type": "string",
		"description": "The artifact: the `artifact` id an earlier answer gave, or its " +
			"dashboard address.",
	}
}

func (t *getArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *getArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, false)
	if refusal != nil {
		return *refusal, nil
	}
	maxBytes := argInt(args, "max_bytes", defaultArtifactRead)
	if maxBytes < 1 || maxBytes > maxArtifactRead {
		return failed(fmt.Sprintf("get_artifact takes `max_bytes` of 1..%d.",
			maxArtifactRead)), nil
	}
	_, offsetGiven := args["offset"]
	offset := argInt(args, "offset", 0)
	if offset < 0 {
		return failed("get_artifact takes an `offset` of 0 or more."), nil
	}
	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactRead, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	if h.Status == artifactsdomain.StatusTrashed {
		return artifactTrashed(t.Name(), h), nil
	}
	number := argInt(args, "version", h.HeadNumber)
	if number < 1 || number > h.HeadNumber {
		return failed(fmt.Sprintf("get_artifact: artifact %s has versions 1..%d.",
			h.ID, h.HeadNumber)), nil
	}
	t.deps.present(c, h.ID, artifacts.PresenceViewing, artifacts.Focus{Version: number})
	version, err := t.deps.Reader.Version(ctx, h.ID, number)
	if err != nil {
		if errors.Is(err, artifactsdomain.ErrNotFound) {
			return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("get_artifact: "+
				"artifact %s no longer keeps version %d. Read the head, or one of the "+
				"versions its answer lists.", h.ID, number)), nil
		}
		return artifactReadFailure(ctx, t.Name(), ref, err), nil
	}
	// AN ADMINISTRATOR'S OVERRIDE IS RECORDED BEFORE ANYTHING IS SHOWN: the
	// owner and the editors see "read by an administrator" in the artifact's
	// own history, and a read whose record could not be written is not made.
	if dec.Elevated {
		if err := t.deps.readElevated(ctx, c, dec, h, number); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return elevatedUnrecorded(t.Name(), err), nil
		}
	}
	versions, err := t.deps.Reader.Versions(ctx, h.ID, "", artifactsdomain.MaxListLimit)
	if err != nil {
		return artifactReadFailure(ctx, t.Name(), ref, err), nil
	}

	read := newForeignRead(turn, c.self())
	writers, partial := versionWriters(read, versions, number)
	writer := false
	if pub := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactPublish,
		t.deps.objectOf(h), t.deps.Chart, t.deps.now()); pub.Allowed && !pub.Elevated {
		writer = true
	}
	out := t.headAnswer(h, version, dec.Standing.Role, writer)
	out["versions"] = versionRows(versions, answerVersions)
	files := make([]map[string]any, 0, min(len(version.Manifest.Files), answerFiles))
	for i, e := range version.Manifest.Files {
		if i == answerFiles {
			out["files_total"] = len(version.Manifest.Files)
			break
		}
		files = append(files, map[string]any{"path": e.Path, "content_type": e.ContentType,
			"size": e.Object.Size})
	}
	out["files"] = files

	path := strings.TrimSpace(argString(args, "path"))
	switch view := strings.TrimSpace(argString(args, "view")); view {
	case "":
	case viewOutline:
		if path != "" {
			return failed("get_artifact takes `view` or `path`, not both: the outline names " +
				"each part's file to read next."), nil
		}
		outline, refusal := t.outline(ctx, read, h, version, writers, partial)
		if refusal != nil {
			return *refusal, nil
		}
		out["outline"] = outline
		return read.answer(ctx, t.Name(), out, "Read the outline in parts by its files.")
	default:
		return failed(fmt.Sprintf("get_artifact takes `view` as %q.", viewOutline)), nil
	}
	if path == "" {
		path = version.Manifest.Entry
	}
	if path != "" {
		file, refusal := t.readFile(ctx, read, c, h, version, path, offset, offsetGiven,
			maxBytes, strings.TrimSpace(argString(args, "focus")), writers, partial)
		if refusal != nil {
			return *refusal, nil
		}
		out["file"] = file
	}
	return read.answer(ctx, t.Name(), out, "Ask for a smaller `max_bytes`.")
}

// headAnswer is the head and the version read, as get_artifact states them.
func (t *getArtifact) headAnswer(h artifactsdomain.Head, v artifactsdomain.Version,
	role artifacts.Role, writer bool) map[string]any {

	out := map[string]any{
		"artifact": h.ID.String(), "kind": string(h.Kind), "title": h.Title,
		"owner": h.Owner, "owner_kind": string(h.OwnerKind), "status": string(h.Status),
		"head_version": h.HeadNumber, "version": v.Number, "role": string(role),
		"writer": writer, "entry": v.Manifest.Entry,
		"updated_at": h.UpdatedAt.UTC().Format(time.RFC3339), "updated_by": actorJSON(h.UpdatedBy),
	}
	if h.Description != "" {
		out["description"] = h.Description
	}
	if h.Icon != "" {
		out["icon"] = h.Icon
	}
	if v.Contract != "" {
		out["contract"] = v.Contract
	}
	if len(v.Capabilities) > 0 {
		out["capabilities"] = v.Capabilities
	}
	if v.Label != "" {
		out["label"] = v.Label
	}
	if url := t.deps.link(h.ID); url != "" {
		out["url"] = url
	}
	// WHO IT IS SHARED WITH IS THE OWNER'S TO SEE: an audience list tells
	// a viewer who else holds the artifact, which a share never granted.
	if role.Includes(artifacts.RoleOwner) {
		shares := make([]map[string]any, 0, len(h.Shares))
		for _, s := range h.Shares {
			shares = append(shares, map[string]any{"to": shareTarget(s.Audience),
				"role": string(s.Role)})
		}
		out["shares"] = shares
	}
	return out
}

// readFile is one page of a version's file, fenced under its writers, or a
// condensed account of a long one a colleague wrote.
func (t *getArtifact) readFile(ctx context.Context, read *foreignRead, c artifactCaller,
	h artifactsdomain.Head, v artifactsdomain.Version, path string, offset int,
	offsetGiven bool, maxBytes int, focus string, writers []provenance.Author,
	partial bool) (map[string]any, *tools.Result) {

	clean, err := manifest.CleanPath(path)
	if err != nil {
		r := failed(fmt.Sprintf("get_artifact: %q is not a published path: %v", clip(path), err))
		return nil, &r
	}
	e, ok := manifest.EntryAt(v.Manifest, clean)
	if !ok {
		r := refusedBy(tools.RefusalNotFound, artifactsdomain.ErrNotFound, fmt.Sprintf(
			"get_artifact: version %d of artifact %s has no file %q; its `files` list "+
				"what it holds.", v.Number, h.ID, clean))
		return nil, &r
	}
	file := map[string]any{"path": e.Path, "content_type": e.ContentType, "size": e.Object.Size}
	typ, err := mediatype.Resolve(e.Path, e.ContentType)
	if err != nil || !typ.Text {
		// A FILE THAT IS NOT TEXT IS DESCRIBED, never dumped: the page that
		// references it names it by its path.
		file["binary"] = true
		read.attribute(writers, partial)
		return file, nil
	}
	trust := read.trustOf(writers, partial)
	at := fieldPath{}.key("file")
	// ANOTHER PRINCIPAL'S PAGE PAST ONE READ PAGE IS CONDENSED, unless the
	// call asked for the source by its offset: a page is markup a model reads
	// to learn what it shows, and the source an edit quotes is one `offset`
	// away. Data and code are never condensed, since what they say is
	// exactly their bytes.
	if !offsetGiven && trust != provenance.TrustOwn && typ.Essence() == "text/html" &&
		e.Object.Size > artifacts.ToolReadPageBytes &&
		c.turn != nil && c.turn.Seat != nil && t.deps.compact != nil {
		if summary, ok := t.condense(ctx, c, e, focus); ok {
			file["summary_note"] = summary.Note() + " Pass `offset` to read the source, " +
				"which an edit's `old_str` must quote exactly."
			read.fence(at.key("summary"), provenance.SourceArtifactFile, writers, partial, summary.Text)
			file["summary"] = summary.Text
			file["next_offset"] = 0
			return file, nil
		}
	}
	text, complete, err := t.deps.Publisher.SourceText(ctx, e, int64(offset+maxBytes))
	if err != nil {
		r := sourceFailure(ctx, t.Name(), e.Path, err)
		return nil, &r
	}
	start := min(offset, len(text))
	// AN OFFSET A CALLER CHOSE MAY SPLIT A CHARACTER: the page starts at the
	// next whole one, so it is always valid text. The read itself ends on a
	// whole character ([artifactupload.Service.SourceText]).
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	page := string(text[start:])
	file["offset"] = start
	if complete {
		file["complete"] = true
	} else {
		file["next_offset"] = len(text)
	}
	read.fence(at.key("content"), provenance.SourceArtifactFile, writers, partial, page)
	file["content"] = page
	return file, nil
}

// viewOutline is get_artifact's view of a deck's or a doc's structure.
const viewOutline = "outline"

// outline is a version's structure as its kind's format reads it, as one text
// an agent reads, fenced under the version's writers: a slide's or a tab's
// title is text its author wrote.
func (t *getArtifact) outline(ctx context.Context, read *foreignRead, h artifactsdomain.Head,
	v artifactsdomain.Version, writers []provenance.Author, partial bool) (map[string]any, *tools.Result) {

	src := toolVersionSource{ctx: ctx, m: v.Manifest, deps: t.deps}
	var (
		text string
		out  map[string]any
	)
	switch spec, _ := h.Kind.Spec(); spec.Format {
	case artifacts.FormatSlides:
		deck, err := slideformat.Read(src)
		if err != nil {
			r := sourceFailure(ctx, t.Name(), slideformat.IndexPath, err)
			return nil, &r
		}
		text = deckOutline(deck.Outline())
		out = map[string]any{"slides": len(deck.Slides)}
	case artifacts.FormatDoc:
		doc, err := docindex.Read(src, v.Manifest.Entry)
		if err != nil {
			r := sourceFailure(ctx, t.Name(), v.Manifest.Entry, err)
			return nil, &r
		}
		text = docOutline(doc.Outline())
		out = map[string]any{"tabs": len(doc.Tabs)}
	default:
		r := failed(fmt.Sprintf("get_artifact: a %s artifact has no outline; read its files "+
			"by `path`.", h.Kind))
		return nil, &r
	}
	read.fence(fieldPath{}.key("outline").key("text"), provenance.SourceArtifactFile, writers, partial, text)
	out["text"] = text
	return out, nil
}

// docOutline is a doc's tabs as one text, each its title, file and size.
func docOutline(o docindex.Outline) string {
	var b strings.Builder
	for i, tab := range o.Tabs {
		fmt.Fprintf(&b, "%d. %s (%s, %d bytes)\n", i+1, tab.Title, tab.Path, tab.Bytes)
	}
	return b.String()
}

// deckOutline is a deck's slides and sections as one text.
func deckOutline(o slideformat.Outline) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Deck: %s\nCover: %s\n", o.Title, o.Cover)
	for _, sec := range o.Sections {
		fmt.Fprintf(&b, "Section from %s: %s\n", sec.Start, sec.Description)
	}
	for i, sl := range o.Slides {
		fmt.Fprintf(&b, "%d. %s (%s): %s", i+1, sl.ID, sl.Path, sl.Title)
		if sl.Hidden {
			b.WriteString(" [hidden]")
		}
		if sl.Notes {
			b.WriteString(" [notes]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// toolVersionSource is a version as a format reads it, through the
// publisher's verified reads.
type toolVersionSource struct {
	ctx  context.Context
	m    artifacts.Manifest
	deps ArtifactDeps
}

func (s toolVersionSource) Paths() []string {
	out := make([]string, len(s.m.Files))
	for i, e := range s.m.Files {
		out[i] = e.Path
	}
	return out
}

func (s toolVersionSource) Essence(p string) (string, bool) {
	e, ok := manifest.EntryAt(s.m, p)
	if !ok {
		return "", false
	}
	typ, err := mediatype.Resolve(e.Path, e.ContentType)
	if err != nil {
		return "", false
	}
	return typ.Essence(), true
}

func (s toolVersionSource) Read(p string) ([]byte, error) {
	e, ok := manifest.EntryAt(s.m, p)
	if !ok {
		return nil, artifactsdomain.ErrNotFound
	}
	text, _, err := s.deps.Publisher.SourceText(s.ctx, e, artifacts.MaxTextFileBytes)
	return text, err
}

// condense is a long text file a colleague wrote, rewritten to fit by the
// seat's auxiliary model, or false where it could not be (a node with no
// auxiliary model, a spent budget): the caller then answers the page itself.
func (t *getArtifact) condense(ctx context.Context, c artifactCaller, e artifacts.Entry,
	focus string) (compact.Result, bool) {

	text, _, err := t.deps.Publisher.SourceText(ctx, e, artifacts.MaxConflictEntryText)
	if err != nil {
		return compact.Result{}, false
	}
	if focus == "" {
		focus = "what this page shows and how it is built"
	}
	res, err := t.deps.compact.For(c.turn.Seat, c.turn.Aux()).Focused(ctx, compact.KindSource,
		string(text), summaryBudget, focus)
	if err != nil || !res.Compacted {
		return compact.Result{}, false
	}
	return res, true
}

// sourceFailure explains a version file's text that could not be read: bytes
// that are not what the version records, or a file row naming none, are
// damage no retry reads past; anything else is the object store not answering.
func sourceFailure(ctx context.Context, tool, path string, err error) tools.Result {
	if errors.Is(err, objstore.ErrCorrupt) || errors.Is(err, objstore.ErrNotFound) {
		return objectFailure(ctx, tool, tools.RefusalInternalError, err, fmt.Sprintf(
			"%s could not read %q: the stored bytes are not the ones the version "+
				"records. %s", tool, path, faultSaid))
	}
	return objectFailure(ctx, tool, tools.RefusalUnavailable, err, fmt.Sprintf(
		"%s could not read %q from the object store just now. Try again shortly.", tool, path))
}

// versionWriters is every writer the artifact's retained versions up to number
// name, and whether writers it cannot name had a hand in that version: a
// version past one whose earlier versions were not all retained (or not all in
// the page read) carries files they may have written.
func versionWriters(read *foreignRead, page artifactsdomain.VersionPage, number int) (
	[]provenance.Author, bool) {

	var writers []provenance.Author
	lowest := 0
	for _, v := range page.Versions {
		if v.Number > number {
			continue
		}
		w := read.writer(v.Author.Name, v.Author.Kind)
		if !slices.Contains(writers, w) {
			writers = append(writers, w)
		}
		if lowest == 0 || v.Number < lowest {
			lowest = v.Number
		}
	}
	return writers, lowest != 1
}

// versionRows is the newest n versions as an answer lists them.
func versionRows(page artifactsdomain.VersionPage, n int) []map[string]any {
	rows := make([]map[string]any, 0, min(n, len(page.Versions)))
	for i, v := range page.Versions {
		if i == n {
			break
		}
		row := map[string]any{"number": v.Number, "author": actorJSON(v.Author),
			"created_at": v.CreatedAt.UTC().Format(time.RFC3339), "files": v.Files,
			"bytes": v.Bytes}
		if v.Label != "" {
			row["label"] = v.Label
		}
		if v.RestoredFrom != 0 {
			row["restored_from"] = v.RestoredFrom
		}
		rows = append(rows, row)
	}
	return rows
}

// shareTarget is an audience as a tool names it: a seat's bare handle,
// `unit:<key>`, or `company`.
func shareTarget(a artifacts.Audience) string {
	if a.Kind == artifacts.AudienceSeat {
		return a.ID
	}
	return a.String()
}
