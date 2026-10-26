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
// said about it (ADR-0050).
//
//  1. The request is built from the call ([generic.request]), so the card and
//     the approval's request are one value.
//  2. A PUT, a PATCH or a DELETE on a path the table also reads (a GET on the
//     same path) reads that representation through the turn's grant.
//  3. What the change makes of it: a PUT's body replaces it, a PATCH's body is
//     merged onto it (RFC 7396, the API's own patch format), a DELETE takes it
//     away, and a POST creates what its body states.
//  4. The rows are the fields that move and nothing else: a structural diff,
//     a list of records matched by the identity each carries (a seat's
//     handle, a unit's id) so one new seat is one added member with its
//     fields, long text as a word diff, a removal as the values it removes.
//     The request itself leads the card, and its body, redacted, follows.
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
//     card is drawn (ADR-0054): a body the route would refuse is the step's
//     refusal in the route's own words, so the model corrects it within the
//     turn, and the check's warnings are rows on the card. The route's check
//     is the one source of what it accepts; no rule of it is copied here.
//  8. A route that records a revision summary ([Route.Summary]) carries it as
//     `_summary` in the body ([summarised]), shown on the card as the history
//     line and kept out of the field rows.

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
			rep, err := readRepresentation(ctx, read, reader, req.Path)
			switch {
			case err != nil:
				return proposal.Preview{}, err
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

	var warnings []string
	if route.DryRun {
		found, checkErr := checked(ctx, read, req, revision)
		if checkErr != nil {
			return proposal.Preview{}, checkErr
		}
		warnings = found
	}

	c := changes{}
	c.fact("request", "Request", req.Method+" "+shownPath(req))
	switch req.Method {
	case http.MethodDelete:
		if before == nil {
			c.fact("removes", "Removes", req.Path)
		} else {
			leaves(&c, "", before, proposal.Removed)
		}
	case http.MethodPost:
		leaves(&c, "", changed, func(field, label, v string) wire.Change {
			return proposal.Added(field, label, v, false)
		})
	default:
		after := changed
		if req.Method == http.MethodPatch {
			after = mergePatch(clone(before), changed)
		}
		if before == nil {
			leaves(&c, "", after, func(field, label, v string) wire.Change {
				return proposal.Added(field, label, v, false)
			})
		} else {
			diff(&c, "", before, after)
		}
	}
	if len(c) == 1 {
		return proposal.Preview{}, fmt.Errorf("%w: the change leaves %s exactly as it is",
			ErrInvalidArguments, req.Path)
	}
	c = bounded(c)
	for i, warning := range warnings {
		c.fact("check.warning."+strconv.Itoa(i+1), "Warning", warning)
	}
	c.fact("request.summary", "History note", redact.Secrets(summary))
	if body != nil {
		c.fact("request.body", "Request body", prettyBody(body))
	}
	recency, _ := authz.RecencyOf(authz.Action(act))
	return proposal.Preview{
		Title:        titleOf(req.Method, creates),
		Target:       req.Path,
		Note:         "This is sent to the engine's API exactly as shown, as you.",
		ApproveLabel: "Approve and send",
		Changes:      c.rows(),
		StepUp:       recency == iam.RecencyStepUp,
		// ON EVERY METHOD, for a representation that was read: a DELETE or
		// a PATCH prepared against a version the resource has left is as
		// stale as a PUT.
		SourceRevision: revision,
		Request:        req,
		Act:            act,
	}, nil
}

// checked asks a [Route.DryRun] route to check req: the same request, under
// the precondition the proposal will be sent with, with `dry_run=true`, through
// the turn's read grant (which carries a write's method only as such a check,
// ADR-0054). A refusal is the step's, in the route's words ([PreviewRefusal]);
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

// checkWarnings are a check's `warnings`, each its `message` (or the value
// itself), as single redacted lines.
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

// titleOf is a change's title, by its method and whether it creates what its
// path names.
func titleOf(method string, creates bool) string {
	switch {
	case method == http.MethodPost || creates:
		return "Create through the API"
	case method == http.MethodDelete:
		return "Delete through the API"
	}
	return "Change through the API"
}

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
		return representation{}, fmt.Errorf("%w: %s is larger than one step reads, so the "+
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

// diff appends the rows for what moves from before to after at field.
func diff(c *changes, field string, before, after any) {
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
				removedLeaves(c, join(field, k), was)
			case !had && has:
				addedLeaves(c, join(field, k), is, true)
			default:
				diff(c, join(field, k), was, is)
			}
		}
		return
	case []any:
		a, ok := after.([]any)
		if !ok {
			break
		}
		diffList(c, field, b, a)
		return
	}
	if equalJSON(before, after) {
		return
	}
	if isRecord(before) || isRecord(after) {
		removedLeaves(c, field, before)
		addedLeaves(c, field, after, false)
		return
	}
	if row, ok := valueRow(field, before, after); ok {
		*c = append(*c, row)
	}
}

// diffList appends the rows for a list moving from before to after: records
// matched by the identity each carries, every other list as one value.
func diffList(c *changes, field string, before, after []any) {
	if equalJSON(before, after) {
		return
	}
	was, wasKeyed := keyed(before)
	is, isKeyed := keyed(after)
	if !wasKeyed || !isKeyed {
		if row, ok := valueRow(field, before, after); ok {
			*c = append(*c, row)
		}
		return
	}
	for _, e := range was {
		if match := find(is, e.id); match == nil {
			removedLeaves(c, join(field, e.id), e.value)
		} else {
			diff(c, join(field, e.id), e.value, match.value)
		}
	}
	for _, e := range is {
		if find(was, e.id) == nil {
			addedLeaves(c, join(field, e.id), e.value, false)
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
func addedLeaves(c *changes, field string, v any, onRecord bool) {
	leaves(c, field, v, func(f, label, value string) wire.Change {
		return proposal.Added(f, label, value, onRecord)
	})
}

// removedLeaves appends a row for every value v holds, as removed.
func removedLeaves(c *changes, field string, v any) {
	leaves(c, field, v, proposal.Removed)
}

// leaves appends row(field, label, value) for every scalar v holds beneath
// field, a list of plain values as one value.
func leaves(c *changes, field string, v any, row func(field, label, value string) wire.Change) {
	switch t := v.(type) {
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(t)) {
			leaves(c, join(field, k), t[k], row)
		}
		return
	case []any:
		if list, ok := keyed(t); ok && len(list) > 0 {
			for _, e := range list {
				leaves(c, join(field, e.id), e.value, row)
			}
			return
		}
		if slices.ContainsFunc(t, isRecord) {
			for i, e := range t {
				leaves(c, join(field, strconv.Itoa(i+1)), e, row)
			}
			return
		}
	}
	value := scalar(v)
	if value == "" || field == "" {
		return
	}
	line := row(field, cards.FieldLabel(field), value)
	// A LIST OF PLAIN VALUES IS DRAWN AS A LIST: its items ride beside the
	// joined line, on the side the row holds.
	if items, ok := plainItems(v); ok {
		if line.Kind == wire.ChangeRemoved {
			line.FromItems = items
		} else {
			line.Items = items
		}
	}
	*c = append(*c, line)
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
func valueRow(field string, before, after any) (wire.Change, bool) {
	was, wasList := plainItems(before)
	is, isList := plainItems(after)
	if (wasList || before == nil) && (isList || after == nil) && (wasList || isList) {
		return proposal.List(field, cards.FieldLabel(field), was, is)
	}
	return proposal.Value(field, cards.FieldLabel(field), scalar(before), scalar(after))
}

// bounded is c with at most [diffRows] field rows past the request's, the rest
// counted in one row.
func bounded(c changes) changes {
	if len(c) <= diffRows+1 {
		return c
	}
	more := len(c) - diffRows - 1
	out := append(changes{}, c[:diffRows+1]...)
	out.fact("more", "More", strconv.Itoa(more)+" more fields change; the request "+
		"body below is the whole change")
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
