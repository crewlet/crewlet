package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
)

// THE GENERIC CHANGE'S PREVIEW: what a [ProposeAPIChangeTool] call would do,
// read by the server from the resource as it is now, never from what the model
// said about it (ADR-0051).
//
//  1. The request is built from the call ([generic.request]), so the card and
//     the approval's request are one value.
//  2. A PUT, a PATCH or a DELETE on a path the table also reads (a GET on the
//     same path) reads that representation through the turn's grant.
//  3. What the change makes of it: a PUT's body replaces it, a PATCH's body is
//     merged onto it (RFC 7396, the API's own patch format), and a DELETE
//     takes it away. A POST does what its route declares ([authz.Effect]): a
//     create adds what its body states, and a call on the act route, a
//     rewrite, a destruction or a question shows its body and its query as
//     sent, since what it does to what is there is its route's or its
//     tool's, titled by its action ([describe]).
//  4. The rows are the fields that move and nothing else: a structural diff,
//     a list of records matched by the identity each carries (a seat's
//     handle, a unit's id) so one new seat is one added member with its
//     fields, long text as a word diff, a removal as the values it removes,
//     and a field a write sets to null, which no value shows, as cleared.
//     The request itself, its body redacted, is the card's technical details
//     for an approver who administers the company, never a row.
//  5. The proposal is bound to the representation it was shown: a strong
//     entity tag becomes the write's If-Match ([proposal.RevisionEntity]),
//     and a representation stating its own version field is re-read by the
//     approval ([proposal.RevisionResource]), so a stale proposal is refused.
//     A PUT whose read found nothing is a create, bound to that absence
//     ([proposal.RevisionCreate]) and sent under `If-None-Match: *`, every
//     field drawn as added; a PATCH or a DELETE of nothing is refused before
//     any card, in the read's own words ([PreviewRefusal]).
//     [proposal.Revision.Precondition] is the one rule from the revision to
//     the headers.
//  6. The verb's own step-up is asked of the approver, shown on the card.
//  7. A route that answers `?dry_run=true` ([Route.DryRun]) is asked it, with
//     the exact request and precondition the approval would send, before any
//     card is drawn (ADR-0055): a body the route would refuse is the step's
//     refusal in the route's own words, so the model corrects it within the
//     turn, and the check's warnings are rows on the card. The route's check
//     is the one source of what it accepts; no rule of it is copied here.
//  8. A route that records a revision summary ([Route.Summary]) carries it as
//     `_summary` in the body ([summarised]), shown on the card as the history
//     line and kept out of the field rows.
//  9. A route that replaces the whole object ([Route.Replaces]) removes only
//     what the call's `removes` names: a field the body drops by omission is
//     the step's refusal, and a declared removal heads the card
//     ([removalCheck]).

// diffRows bounds how many field rows one card carries: past it, the rest is
// counted in one row, and the whole change is still the request the card
// shows.
const diffRows = 60

// versionFields are the top-level fields a representation states its own
// version in, in the order they are asked for.
var versionFields = []string{"version", "revision", "revision_id", "head_number"}

// identityKeys are the fields a record in a list is known by, in the order
// they are asked for: what matches a seat, a unit or a policy across the
// before and the after.
var identityKeys = []string{"handle", "id", "key", "slug", "name"}

// preview is the generic change's previewer.
func (g generic) preview(ctx context.Context, read proposal.Read, _ time.Time,
	args map[string]any) (proposal.Preview, error) {

	req, route, err := g.request(Args(args), false)
	if err != nil {
		return proposal.Preview{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
	}
	if req.Method == http.MethodPost && !route.Effect.Valid() {
		// A POST THAT SAYS NOTHING OF WHAT IT DOES is never a card: its
		// method does not say, and a title guessed for it would be what the
		// person approves ([authz.Effect]).
		return proposal.Preview{}, fmt.Errorf("%w: %s does not say what it does, so no "+
			"card can show it truthfully", ErrInvalidArguments, route.Pattern)
	}
	act := string(route.Action)
	if route.Pattern == ActRoute {
		act = strings.TrimPrefix(req.Path, ActPrefix)
	}
	var body any
	if len(req.Body) > 0 {
		if body, err = cards.Decode(req.Body); err != nil {
			return proposal.Preview{}, fmt.Errorf("%w: %w", ErrInvalidArguments, err)
		}
	}
	// THE HISTORY LINE IS NOT A FIELD of the resource: the route lifts it
	// out before it reads the document, so the rows are drawn without it.
	changed, summary := body, ""
	if record, ok := body.(map[string]any); ok && route.Summary {
		summary = strings.TrimSpace(cards.Text(record[SummaryKey]))
		without := clone(record).(map[string]any)
		delete(without, SummaryKey)
		changed = without
	}

	var before any
	revision, creates := "", false
	if req.Method != http.MethodPost {
		if reader, ok := g.table.readerOf(req.Path); ok {
			rep, readErr := readRepresentation(ctx, read, reader, req.Path)
			switch {
			case readErr != nil:
				return proposal.Preview{}, readErr
			case rep.absent != nil && req.Method == http.MethodPut && route.createsWhenAbsent():
				// NOTHING IS THERE, SO THE PUT IS A CREATE, and says so:
				// sent under `If-None-Match: *` ([proposal.RevisionCreate]),
				// the API's one spelling of "add this", which a route that
				// adds by address requires (a plain PUT never creates under
				// `/config`) and which refuses the write once something holds
				// the path rather than replacing it.
				revision = proposal.Revision{Kind: proposal.RevisionCreate, ID: req.Path,
					Version: proposal.VersionAbsent}.String()
				creates = true
			case rep.absent != nil && req.Method == http.MethodPut:
				// A ROUTE THAT DOES NOT CREATE BY ADDRESS is never a card for
				// an address that holds nothing: the person would approve a
				// write the API is certain to refuse. The step is refused
				// with where one is added, as the route declares it.
				return proposal.Preview{}, notCreatable(route, req.Path)
			case rep.absent != nil:
				// A PATCH OR A DELETE OF NOTHING cannot apply, so it is never
				// a card: the read's own 404 is the step's refusal.
				return proposal.Preview{}, rep.absent
			default:
				before, revision = rep.doc, rep.revision
			}
		}
	}

	// A REPLACE REMOVES ONLY WHAT THE CALL SAYS IT REMOVES ([removalCheck]),
	// asked before the route's own check: a field dropped by omission is the
	// call's to correct whether or not the route would take the body.
	address, _ := pathValues(args["path"])
	removals, removalErr := removalCheck(route, req.Method, before, changed, address,
		Args(args).Strings("removes"), lowerFirst(callTitle(Args(args))))
	if removalErr != nil {
		return proposal.Preview{}, removalErr
	}

	var warnings []string
	if route.DryRun {
		found, checkErr := checked(ctx, read, req, revision)
		if checkErr != nil {
			return proposal.Preview{}, checkErr
		}
		warnings = found
	}

	var after any
	switch req.Method {
	case http.MethodDelete:
	case http.MethodPatch:
		after = mergePatch(clone(before), changed)
	default:
		after = changed
	}
	l := newLabeller(before, after)
	record, _ := body.(map[string]any)
	said := describe(route, req.Method, creates, before, after, subject{body: record,
		query: req.Query, named: g.named(ctx, read, route, req.Method, address)},
		removals, address, l)
	d := differ{l: l}
	if len(removals) > 0 {
		// THE REMOVALS LEAD THE ROWS as one line, beside the warning the
		// card heads itself with ([proposal.Preview.Removes]).
		d.c.fact("request.removes", "Removes", strings.Join(d.removalLabels(removals), ", "))
	}
	switch {
	case req.Method == http.MethodDelete && before == nil:
		d.c.fact("removes", "Deletes", capitalised(said.Object))
	case req.Method == http.MethodDelete:
		d.leaves("", before, proposal.Removed)
	case route.Effect == authz.EffectCalls:
		// A CALL'S ARGUMENTS travel in `args`, which labels none of them.
		d.sent(after, "args.", req.Query, "")
	case req.Method == http.MethodPost && acts(route.Effect):
		d.sent(after, "", req.Query, capitalised(said.Object))
	case before == nil:
		d.leaves("", after, func(field, label, v string) wire.Change {
			return proposal.Added(field, label, v, false)
		})
		if req.Method == http.MethodPut && !creates {
			d.cleared("", after)
		}
		if req.Method == http.MethodPost {
			d.queried(req.Query)
		}
	default:
		d.diff("", before, after)
	}
	if len(d.c) == 0 {
		return proposal.Preview{}, fmt.Errorf("%w: the change leaves %s exactly as it is",
			ErrInvalidArguments, req.Path)
	}
	// THE REQUEST ITSELF IS AN ADMINISTRATOR'S TO READ, behind the card's
	// "Technical details": everybody else reviews the rows, which are built
	// from the same request and the resource it changes.
	var technical *wire.TechnicalRequest
	if g.person.Can(iam.GrantAdmin) {
		technical = &wire.TechnicalRequest{Method: req.Method, Path: shownPath(req)}
		if body != nil {
			technical.Body = prettyBody(body)
		}
	}
	c := bounded(d.c, technical != nil)
	for i, warning := range warnings {
		c.fact("check.warning."+strconv.Itoa(i+1), "Warning", warning)
	}
	c.fact("request.summary", "History note", redact.Secrets(summary))
	recency, _ := authz.RecencyOf(authz.Action(act))
	return proposal.Preview{
		Title:        said.Title,
		Target:       req.Path,
		Summary:      said.Summary,
		Note:         said.Note,
		Risk:         said.Risk,
		Technical:    technical,
		ApproveLabel: said.ApproveLabel,
		Changes:      c.rows(),
		StepUp:       recency == iam.RecencyStepUp,
		// ON EVERY METHOD, for a representation that was read: a DELETE or
		// a PATCH prepared against a version the resource has left is as
		// stale as a PUT.
		SourceRevision: revision,
		Request:        req,
		Act:            act,
		Removes:        d.removalLabels(removals),
	}, nil
}

// checked asks a [Route.DryRun] route to check req: the same request, under
// the precondition the proposal will be sent with, with `dry_run=true`, through
// the turn's read grant (which carries a write's method only as such a check,
// ADR-0055). A refusal is the step's, in the route's words ([PreviewRefusal]);
// an admitted check answers the warnings it raised, each one line.
func checked(ctx context.Context, read proposal.Read, req exec.Request,
	revision string) ([]string, error) {

	check := req
	check.Query = maps.Clone(req.Query)
	if check.Query == nil {
		check.Query = url.Values{}
	}
	check.Query.Set(authz.DryRunQuery, "true")
	if r, err := proposal.ParseRevision(revision); err == nil {
		check.IfMatch, check.IfNoneMatch = r.Precondition()
	}
	resp, err := read(ctx, check)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%w: the route's check could not be asked: %w",
			ErrPreviewRead, err)
	case !cards.Permitted(resp.Status):
		return nil, &PreviewRefusal{Status: resp.Status, Refusal: exec.RefusalOf(resp)}
	}
	return checkWarnings(resp.Body), nil
}

// checkWarnings are a check's `warnings` this change introduces, each its
// `message` (or the value itself), as single redacted lines.
//
// ONLY WHAT THE CHANGE INTRODUCES: a config route's check judges the whole
// document it would store, so its warnings include every advisory the company
// already carried (a person's seat with no chat contact, on a card that adds
// an agent). A warning the route marks `"introduced": false` is left off the
// card; one it does not mark is kept, so nothing a route warns about without
// saying is ever dropped.
func checkWarnings(body []byte) []string {
	var answer struct {
		Warnings []any `json:"warnings"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return nil
	}
	var out []string
	for _, w := range answer.Warnings {
		var line string
		if record, ok := w.(map[string]any); ok {
			if introduced, said := record["introduced"].(bool); said && !introduced {
				continue
			}
			line = cards.Text(record["message"])
		} else {
			line = cards.Text(w)
		}
		if line = strings.Join(strings.Fields(redact.Secrets(line)), " "); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// ActPrefix is the act route's path ahead of the tool's name.
const ActPrefix = "/operator/act/"

// shownPath is the path a card names, with the query it carries.
func shownPath(req exec.Request) string {
	if len(req.Query) == 0 {
		return req.Path
	}
	return req.Path + "?" + req.Query.Encode()
}

// representation is what a preview's read of the path it changes found.
type representation struct {
	// doc and revision are the representation and the revision the proposal
	// is bound to ([revisionOf]), for a path that holds one.
	doc      any
	revision string
	// absent is the read's 404, in the route's words, for a path nothing
	// holds: what a PUT creates, and what a PATCH or a DELETE cannot apply to.
	absent *PreviewRefusal
}

// readRepresentation reads path through the grant: the representation and the
// revision the proposal is bound to (the strong entity tag the read answered,
// or the version the representation states, or none), or its absence. A path
// the person cannot read is refused in the route's own words ([PreviewRefusal]).
func readRepresentation(ctx context.Context, read proposal.Read, reader Route,
	path string) (representation, error) {

	resp, err := read(ctx, exec.Request{Method: http.MethodGet, Path: path})
	switch {
	case err != nil:
		return representation{}, fmt.Errorf("%w: %w", ErrPreviewRead, err)
	case resp.Status == http.StatusNotFound:
		return representation{absent: &PreviewRefusal{Status: resp.Status,
			Refusal: exec.RefusalOf(resp)}}, nil
	case !cards.Permitted(resp.Status):
		return representation{}, refusedRead(resp)
	case resp.Truncated:
		return representation{}, fmt.Errorf("%w: %s is larger than a change's preview reads, so the "+
			"change could not be shown against it", ErrPreviewRead, reader.Pattern)
	}
	doc, err := cards.Decode(resp.Body)
	if err != nil {
		return representation{}, fmt.Errorf("%w: %s answered no JSON: %w", ErrPreviewRead,
			reader.Pattern, err)
	}
	return representation{doc: doc, revision: revisionOf(path, resp.Header.Get("ETag"), doc)}, nil
}

// revisionOf is the revision a representation of path is at: its strong
// entity tag, else the first version field it states, else none. A value that
// cannot be spelled in a revision (an `@` in it) binds nothing rather than
// something ambiguous.
func revisionOf(path, etag string, doc any) string {
	if exec.ValidEntityTag(etag) && !strings.Contains(etag, "@") {
		return proposal.Revision{Kind: proposal.RevisionEntity, ID: path,
			Version: strings.Trim(etag, `"`)}.String()
	}
	record, _ := doc.(map[string]any)
	for _, field := range versionFields {
		v := strings.TrimSpace(cards.Text(record[field]))
		if v != "" && !strings.ContainsAny(v, "@=") {
			return proposal.Revision{Kind: proposal.RevisionResource, ID: path,
				Version: field + "=" + v}.String()
		}
	}
	return ""
}

// apiRevision re-reads a generic revision now, for [Revision]. A create's is
// still absent while nothing holds its path, and present once something does,
// which is a create that lost.
func apiRevision(ctx context.Context, read proposal.Read, r proposal.Revision) (string, error) {
	resp, err := read(ctx, exec.Request{Method: http.MethodGet, Path: r.ID})
	switch {
	case err != nil:
		return "", err
	case r.Kind == proposal.RevisionCreate && resp.Status == http.StatusNotFound:
		return r.String(), nil
	case resp.Status == http.StatusNotFound:
		return "", proposal.ErrGone
	case !cards.Permitted(resp.Status):
		return "", refusedRead(resp)
	case r.Kind == proposal.RevisionCreate:
		return proposal.Revision{Kind: r.Kind, ID: r.ID, Version: proposal.VersionPresent}.String(), nil
	}
	doc, err := cards.Decode(resp.Body)
	if err != nil && r.Kind == proposal.RevisionResource {
		return "", fmt.Errorf("%w: %s answered no JSON: %w", ErrPreviewRead, r.ID, err)
	}
	if r.Kind == proposal.RevisionEntity {
		etag := resp.Header.Get("ETag")
		if !exec.ValidEntityTag(etag) {
			return "", fmt.Errorf("%w: %s no longer answers an entity tag", ErrPreviewRead, r.ID)
		}
		return proposal.Revision{Kind: r.Kind, ID: r.ID,
			Version: strings.Trim(etag, `"`)}.String(), nil
	}
	field, _, _ := strings.Cut(r.Version, "=")
	record, _ := doc.(map[string]any)
	return proposal.Revision{Kind: r.Kind, ID: r.ID,
		Version: field + "=" + strings.TrimSpace(cards.Text(record[field]))}.String(), nil
}

// mergePatch is RFC 7396 over decoded JSON: what the API's own PATCH does
// with a body (internal/api/configapi's merge), so the after the card shows is
// the document the route will store.
func mergePatch(target, patch any) any {
	object, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	into, ok := target.(map[string]any)
	if !ok {
		into = map[string]any{}
	}
	for key, value := range object {
		if value == nil {
			delete(into, key)
			continue
		}
		into[key] = mergePatch(into[key], value)
	}
	return into
}

// clone is a deep copy of decoded JSON, so a merge never rewrites the
// representation it was read as.
func clone(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = clone(e)
		}
		return out
	}
	return v
}

// differ draws a change's rows, each labelled and valued as a person reads it
// ([labeller]).
type differ struct {
	c changes
	l labeller
}

// diff appends the rows for what moves from before to after at field.
func (d *differ) diff(field string, before, after any) {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			break
		}
		keys := slices.Sorted(maps.Keys(b))
		for k := range a {
			if _, held := b[k]; !held {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		keys = slices.Compact(keys)
		for _, k := range keys {
			was, had := b[k]
			is, has := a[k]
			switch {
			case had && !has:
				d.removedLeaves(join(field, k), was)
			case !had && has:
				d.addedLeaves(join(field, k), is, true)
			default:
				d.diff(join(field, k), was, is)
			}
		}
		return
	case []any:
		a, ok := after.([]any)
		if !ok {
			break
		}
		d.diffList(field, b, a)
		return
	}
	if equalJSON(before, after) {
		return
	}
	if isRecord(before) || isRecord(after) {
		d.removedLeaves(field, before)
		d.addedLeaves(field, after, false)
		return
	}
	if row, ok := d.valueRow(field, before, after); ok {
		d.c = append(d.c, row)
	}
}

// diffList appends the rows for a list moving from before to after: records
// matched by the identity each carries, every other list as one value.
func (d *differ) diffList(field string, before, after []any) {
	if equalJSON(before, after) {
		return
	}
	was, wasKeyed := keyed(before)
	is, isKeyed := keyed(after)
	if !wasKeyed || !isKeyed {
		if row, ok := d.valueRow(field, before, after); ok {
			d.c = append(d.c, row)
		}
		return
	}
	for _, e := range was {
		if match := find(is, e.id); match == nil {
			d.removedLeaves(join(field, e.id), e.value)
		} else {
			d.diff(join(field, e.id), e.value, match.value)
		}
	}
	for _, e := range is {
		if find(was, e.id) == nil {
			d.addedLeaves(join(field, e.id), e.value, false)
		}
	}
}

// entry is one record of a list, by its identity.
type entry struct {
	id    string
	value any
}

// keyed is a list of records, each by the first identity key it carries, and
// whether every member is a record with one, none twice.
func keyed(list []any) ([]entry, bool) {
	out := make([]entry, 0, len(list))
	seen := map[string]bool{}
	for _, v := range list {
		record, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		id := ""
		for _, k := range identityKeys {
			if s := strings.TrimSpace(cards.Text(record[k])); s != "" {
				id = s
				break
			}
		}
		if id == "" || seen[id] {
			return nil, false
		}
		seen[id] = true
		out = append(out, entry{id: id, value: v})
	}
	return out, true
}

func find(list []entry, id string) *entry {
	for i := range list {
		if list[i].id == id {
			return &list[i]
		}
	}
	return nil
}

// addedLeaves appends a row for every value v holds, as added under field;
// onRecord marks a field added to a record that already exists.
func (d *differ) addedLeaves(field string, v any, onRecord bool) {
	d.leaves(field, v, func(f, label, value string) wire.Change {
		return proposal.Added(f, label, value, onRecord)
	})
}

// removedLeaves appends a row for every value v holds, as removed.
func (d *differ) removedLeaves(field string, v any) {
	d.leaves(field, v, proposal.Removed)
}

// sent appends the rows of a POST that does not create: every value its body
// and its query send, as sent ([proposal.Fact]), never as a value it adds,
// since what it does to what is there is its route's or its tool's. Each is
// labelled by its own name, below wrapper (a call's `args`). A request sending
// nothing names what it acts on in one row (object), or says it sends nothing,
// so the card still shows what the person approves.
func (d *differ) sent(body any, wrapper string, query url.Values, object string) {
	n := len(d.c)
	d.leaves("", body, func(field, _, value string) wire.Change {
		return proposal.Fact(field, d.l.label(strings.TrimPrefix(field, wrapper)), value)
	})
	d.cleared("", body)
	d.queried(query)
	switch {
	case len(d.c) > n:
	case object != "":
		d.c.fact("object", "Applies to", object)
	default:
		d.c.fact("sent", "Details", "None")
	}
}

// cleared appends a row for every field v sets to null beneath field, where
// the request is not a create: a null is no value to draw, and what clearing
// a field means is its route's (a room's horizon goes back to the company's),
// so the row says only that the request clears it. A create's null sets
// nothing and is no row.
func (d *differ) cleared(field string, v any) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			d.cleared(join(field, k), t[k])
		}
	case nil:
		if field != "" {
			d.c.fact(field, d.l.label(field), "Cleared")
		}
	}
}

// queried appends a row for every parameter a POST's query sends, as sent: a
// verb whose arguments travel in its query (a prune's cutoff and its
// confirmation, a backup's directory) shows them as a body's would be.
func (d *differ) queried(query url.Values) {
	for _, name := range slices.Sorted(maps.Keys(query)) {
		value := redact.Secrets(strings.Join(query[name], ", "))
		d.c.fact("query."+name, cards.FieldLabel(name), value)
	}
}

// named is what a best-effort read of the record a change is about calls it,
// for its card: the record a registered family is about ([resource.Of]), or
// the one a POST that does not create acts on ([actedOn]), read through the
// turn's grant, as the person. A record they may not read, one the table
// serves no read of, and any failure name nothing, and the card says it in
// neutral words; the read decides nothing about the change, whose route still
// decides it, and binds nothing.
func (g generic) named(ctx context.Context, read proposal.Read, route Route, method string,
	address map[string]string) string {

	res, _ := resourceOf(route)
	path := res.Of
	if path == "" && method == http.MethodPost && acts(route.Effect) {
		path = actedOn(route.Path())
	}
	if path == "" {
		return ""
	}
	values := map[string]string{}
	for _, segment := range strings.Split(path, "/") {
		if name, wild := wildcardName(segment); wild {
			values[name] = address[name]
		}
	}
	req, err := fill(http.MethodGet+" "+path, values, nil)
	if err != nil {
		return ""
	}
	if _, ok := g.table.readerOf(req.Path); !ok {
		return ""
	}
	resp, err := read(ctx, req)
	if err != nil || !cards.Permitted(resp.Status) || resp.Truncated {
		return ""
	}
	doc, err := cards.Decode(resp.Body)
	if err != nil {
		return ""
	}
	if r, ok := resourceAt(path); ok && r.Named != nil {
		return oneLineValue(r.Named(doc))
	}
	return representationName(doc)
}

// actedOn is the record a path pattern's action follows: the path up to its
// last wildcard ("/worklet/items/{key}" of "/worklet/items/{key}/purge"), or
// "" where its action names a record of its own ("remove-peer/{peer}") or
// follows none.
func actedOn(path string) string {
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if lastLiteral(segments) != len(segments)-1 {
		return ""
	}
	for i := len(segments) - 2; i >= 0; i-- {
		if _, wild := wildcardName(segments[i]); wild {
			return "/" + strings.Join(segments[:i+1], "/")
		}
	}
	return ""
}

// leaves appends row(field, label, value) for every scalar v holds beneath
// field, a list of plain values as one value.
func (d *differ) leaves(field string, v any, row func(field, label, value string) wire.Change) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			d.leaves(join(field, k), t[k], row)
		}
		return
	case []any:
		if list, ok := keyed(t); ok && len(list) > 0 {
			for _, e := range list {
				d.leaves(join(field, e.id), e.value, row)
			}
			return
		}
		if slices.ContainsFunc(t, isRecord) {
			for i, e := range t {
				d.leaves(join(field, strconv.Itoa(i+1)), e, row)
			}
			return
		}
	}
	items, isItems := plainItems(v)
	value := scalar(v)
	if isItems {
		items = d.l.items(field, items)
		value = strings.Join(items, ", ")
	} else {
		value = d.l.value(field, value)
	}
	if value == "" || field == "" {
		return
	}
	line := row(field, d.l.label(field), value)
	// A LIST OF PLAIN VALUES IS DRAWN AS A LIST: its items ride beside the
	// joined line, on the side the row holds.
	if isItems {
		if line.Kind == wire.ChangeRemoved {
			line.FromItems = items
		} else {
			line.Items = items
		}
	}
	d.c = append(d.c, line)
}

// plainItems is v as the items of a list of plain values, each as one line of
// a card, and whether v is one: a list holding a record or a list is not.
func plainItems(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok || slices.ContainsFunc(list, func(e any) bool { return isRecord(e) || isList(e) }) {
		return nil, false
	}
	items := make([]string, 0, len(list))
	for _, e := range list {
		if item := scalar(e); item != "" {
			items = append(items, item)
		}
	}
	return items, true
}

// valueRow is the row for one field moving from before to after: a list of
// plain values on either side as a list ([proposal.List]), any other value as
// one line.
func (d *differ) valueRow(field string, before, after any) (wire.Change, bool) {
	label := d.l.label(field)
	was, wasList := plainItems(before)
	is, isList := plainItems(after)
	if (wasList || before == nil) && (isList || after == nil) && (wasList || isList) {
		return proposal.List(field, label, d.l.items(field, was), d.l.items(field, is))
	}
	return proposal.Value(field, label, d.l.value(field, scalar(before)),
		d.l.value(field, scalar(after)))
}

// removalLabels are removal paths as the card names them: a member by its
// name, every other field in words ([labeller.label]).
func (d *differ) removalLabels(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, d.l.label(p))
	}
	return out
}

// bounded is c with at most [diffRows] rows, the rest counted in one row,
// which points an administrator to the technical details.
func bounded(c changes, technical bool) changes {
	if len(c) <= diffRows {
		return c
	}
	out := append(changes{}, c[:diffRows]...)
	out.fact("more", "More", moreRows(len(c)-diffRows, technical))
	return out
}

// join is a field path one key deeper.
func join(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}

// scalar is a value as one line of a card: a string as it is, a number or a
// truth as written, a list of plain values joined, and anything else as JSON.
func scalar(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return redact.Secrets(t)
	case []any:
		if !slices.ContainsFunc(t, func(e any) bool { return isRecord(e) || isList(e) }) {
			parts := make([]string, 0, len(t))
			for _, e := range t {
				parts = append(parts, scalar(e))
			}
			return strings.Join(parts, ", ")
		}
	case map[string]any:
	default:
		return cards.Text(t)
	}
	encoded, _ := json.Marshal(v)
	return redact.Secrets(string(encoded))
}

func isRecord(v any) bool { _, ok := v.(map[string]any); return ok }
func isList(v any) bool   { _, ok := v.([]any); return ok }

// equalJSON reports two decoded values that encode alike.
func equalJSON(a, b any) bool {
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// prettyBody is a request body as the card shows it: indented, every string
// through redact.Secrets.
func prettyBody(body any) string {
	encoded, err := json.MarshalIndent(scrub(clone(body)), "", "  ")
	if err != nil {
		return ""
	}
	return string(encoded)
}
