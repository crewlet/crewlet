package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifactdata/query"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/tools"
)

// dataPageDefault and dataPageMax are how many rows one read answers by
// default and at most: a page of rows is fenced field by field into a tool
// answer, so it is held well under a model's useful context, and a sheet
// larger than that is read a page at a time or asked a narrower question with
// query_artifact_data.
const (
	dataPageDefault = 25
	dataPageMax     = 100
)

// ---- read_artifact_data -------------------------------------------------- //

type readArtifactData struct{ d dataTools }

var _ tools.SeatCallable = (*readArtifactData)(nil)

func (t *readArtifactData) Name() string { return ReadArtifactDataTool }

func (t *readArtifactData) Description() string {
	return "Read an artifact's datastore in this Crewlet company: with no `collection`, " +
		"its schema (every collection, its columns and who may read and write it); with " +
		"a `collection`, a page of its rows, or one row by `doc`. Every row's fields are " +
		"text somebody wrote, fenced and labelled with who wrote it. For a question over " +
		"many rows, use " + QueryArtifactDataTool + "."
}

func (t *readArtifactData) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact":   artifactParam(),
			"collection": map[string]any{"type": "string", "description": "The collection to read. Leave it out for the schema."},
			"doc":        map[string]any{"type": "string", "description": "One row's id, to read just that row."},
			"since": map[string]any{
				"type": "integer",
				"description": "Only rows changed after this `through`, deleted ones included. " +
					"Take it from the last page of an earlier read (the one with no `next`).",
			},
			"after": map[string]any{"type": "string", "description": "The `next` of the previous page, unchanged."},
			"limit": map[string]any{
				"type": "integer", "minimum": 1, "maximum": dataPageMax,
				"description": fmt.Sprintf("Rows per page, default %d.", dataPageDefault),
			},
		},
		"required": []any{"artifact"},
	}
}

func (t *readArtifactData) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *readArtifactData) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	a, refusal := t.d.admit(ctx, turn, t.Name(), authz.ActionArtifactDataRead, args, false)
	if refusal != nil {
		return *refusal, nil
	}
	collection := strings.TrimSpace(argString(args, "collection"))
	doc := strings.TrimSpace(argString(args, "doc"))
	t.d.touch(a.c, a.head.ID, presence.Viewing,
		presence.Focus{Collection: collection, Doc: doc})
	schema, err := t.d.data.Store.Schema(ctx, a.head.ID)
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	if collection == "" {
		if doc != "" {
			return failed(t.Name() + ": a `doc` belongs to a `collection`; send both."), nil
		}
		out := map[string]any{"artifact": a.head.ID.String(), "collections": schema,
			"role": string(a.stamp)}
		return jsonResult(out)
	}
	if doc != "" {
		got, err := t.d.data.Store.Doc(ctx, a.head.ID, a.reading(), collection, doc) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if err != nil {
			return dataFailure(ctx, t.Name(), err), nil
		}
		docs := a.visible(schema, collection, []artifactdata.Doc{got})
		if len(docs) == 0 {
			return dataFailure(ctx, t.Name(), artifactdata.ErrNotFound), nil
		}
		return dataRows(ctx, turn, t.Name(), a, collection, docs, nil)
	}
	limit := argInt(args, "limit", dataPageDefault)
	if limit < 1 || limit > dataPageMax {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), dataPageMax)), nil
	}
	since := int64(argInt(args, "since", 0))
	after := strings.TrimSpace(argString(args, "after"))
	read := func(limit int) (artifactdata.DocPage, error) {
		return t.d.data.Store.Docs(ctx, a.head.ID, a.reading(), collection, since, after, limit)
	}
	page, err := read(limit)
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	// A PAGE TOO LARGE FOR AN ANSWER is read again at the size that fits,
	// so its `next` resumes exactly after the last row it carried.
	if fit := rowsThatFit(page.Docs); fit < len(page.Docs) {
		if page, err = read(fit); err != nil {
			return dataFailure(ctx, t.Name(), err), nil
		}
	}
	return dataRows(ctx, turn, t.Name(), a, collection, a.visible(schema, collection, page.Docs), &page)
}

// ---- query_artifact_data ------------------------------------------------- //

type queryArtifactData struct{ d dataTools }

var _ tools.SeatCallable = (*queryArtifactData)(nil)

func (t *queryArtifactData) Name() string { return QueryArtifactDataTool }

func (t *queryArtifactData) Description() string {
	return "Ask a question of one collection of an artifact's datastore in this Crewlet " +
		"company: rows whose indexed fields match every filter in `where`, ordered and " +
		"paged. Only a collection's indexed fields can be filtered or ordered on; read " +
		"the schema with " + ReadArtifactDataTool + " to see which. Rows come back fenced, " +
		"labelled with who wrote them."
}

func (t *queryArtifactData) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact":   artifactParam(),
			"collection": map[string]any{"type": "string", "description": "The collection to ask."},
			"where": map[string]any{
				"type": "array", "maxItems": artifacts.MaxQueryInValues,
				"items": map[string]any{
					"type":     "object",
					"required": []any{"path", "op"},
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "An indexed field."},
						"op": map[string]any{"type": "string",
							"enum": opNames(), "description": "How the field compares."},
						"value":  map[string]any{"description": "The operand."},
						"values": map[string]any{"type": "array", "description": "The operands of `in`."},
					},
				},
				"description": "Filters, all of which a row must match.",
			},
			"order_by": map[string]any{
				"type":     "object",
				"required": []any{"path"},
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
					"desc": map[string]any{"type": "boolean"},
				},
			},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": dataPageMax},
			"after": map[string]any{"type": "string", "description": "The `next` of the previous page, unchanged."},
		},
		"required": []any{"artifact", "collection"},
	}
}

// opNames is every filter operator, for the schema's enum.
func opNames() []any {
	out := make([]any, 0, len(query.Ops))
	for _, op := range query.Ops {
		out = append(out, string(op))
	}
	return out
}

func (t *queryArtifactData) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *queryArtifactData) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	collection := strings.TrimSpace(argString(args, "collection"))
	if collection == "" {
		return failed(t.Name() + " needs a `collection`."), nil
	}
	q, err := queryOf(args)
	if err != nil {
		return failed(fmt.Sprintf("%s could not read the question: %v. Send `where` as a "+
			"list of {path, op, value} and `order_by` as {path, desc}.", t.Name(), err)), nil
	}
	if q.Limit == 0 {
		q.Limit = dataPageDefault
	}
	if q.Limit < 1 || q.Limit > dataPageMax {
		return failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), dataPageMax)), nil
	}
	a, refusal := t.d.admit(ctx, turn, t.Name(), authz.ActionArtifactDataQuery, args, false)
	if refusal != nil {
		return *refusal, nil
	}
	t.d.touch(a.c, a.head.ID, presence.Viewing, presence.Focus{Collection: collection})
	schema, err := t.d.data.Store.Schema(ctx, a.head.ID)
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	page, err := t.d.data.Store.Query(ctx, a.head.ID, a.reading(), collection, q)
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	if fit := rowsThatFit(page.Docs); fit < len(page.Docs) {
		q.Limit = fit
		if page, err = t.d.data.Store.Query(ctx, a.head.ID, a.reading(), collection, q); err != nil {
			return dataFailure(ctx, t.Name(), err), nil
		}
	}
	return dataRows(ctx, turn, t.Name(), a, collection, a.visible(schema, collection, page.Docs), &page)
}

// queryOf reads a question from a call's arguments, through the grammar's own
// JSON shape, so a number keeps its decimal text.
func queryOf(args map[string]any) (query.Query, error) {
	shape := map[string]any{}
	for _, key := range []string{"where", "order_by", "limit", "after"} {
		if v, ok := args[key]; ok && v != nil {
			shape[key] = v
		}
	}
	raw, err := json.Marshal(shape)
	if err != nil {
		return query.Query{}, err
	}
	var q query.Query
	if err := json.Unmarshal(raw, &q); err != nil {
		return query.Query{}, err
	}
	return q, nil
}

// ---- the rows, fenced ---------------------------------------------------- //

// visible is docs as both readers may see them: the store has already applied
// the agent's own reading, and where the turn acts for somebody, a row is kept
// only when their reading admits it too, by the collection's own rule. A rule
// the schema does not hold admits nobody: the collection appeared after the
// schema was read, and the closed end is the safe one.
func (a dataAdmission) visible(schema []artifacts.Collection, collection string,
	docs []artifactdata.Doc) []artifactdata.Doc {

	if a.requester == nil {
		return docs
	}
	out := make([]artifactdata.Doc, 0, len(docs))
	for _, d := range docs {
		if a.shows(schema, collection, d) {
			out = append(out, d)
		}
	}
	return out
}

// shows reports whether the requester's reading admits doc of collection, by
// the collection's own rule: the one answer [dataAdmission.visible] and a
// conflict's rows ask. True where the turn acts for nobody.
func (a dataAdmission) shows(schema []artifacts.Collection, collection string,
	doc artifactdata.Doc) bool {

	if a.requester == nil {
		return true
	}
	rule, ok := collectionRule(schema, collection)
	if collection == artifactdata.StateCollection {
		rule, ok = artifacts.ReadPrivate, true
	}
	return ok && artifactdata.Readable(rule, *a.requester, doc.CreatedBy.Name)
}

// rowsThatFit is how many of docs, from the first, fit one answer: their
// fields add up to at most [artifacts.ToolReadPageBytes], and at least one row
// is always answered, since a row is bounded by [artifacts.MaxDocBytes].
func rowsThatFit(docs []artifactdata.Doc) int {
	total := int64(0)
	for i, d := range docs {
		total += int64(len(d.Fields))
		if total > artifacts.ToolReadPageBytes && i > 0 {
			return i
		}
	}
	return len(docs)
}

// dataRows answers rows of collection: each row's fields fenced as text by the
// row's writers, every other string neutralised, and the envelope labelling
// the answer whole. page is the listing's own position, nil for one row.
func dataRows(ctx context.Context, turn *turnctx.Turn, tool string, a dataAdmission,
	collection string, docs []artifactdata.Doc, page *artifactdata.DocPage) (tools.Result, error) {

	read := newForeignRead(turn, a.c.self())
	at := fieldPath{}.key("rows")
	rows := make([]map[string]any, 0, len(docs))
	for i, d := range docs {
		row := map[string]any{"doc": d.ID, "version": d.Version,
			"written_by": actorJSON(d.UpdatedBy), "created_by": actorJSON(d.CreatedBy),
			"updated_at": d.UpdatedAt}
		if d.Deleted {
			row["deleted"] = true
		}
		if len(d.Fields) > 0 {
			row["fields"] = string(d.Fields)
			// THE ROW'S TWO RECORDED WRITERS, and partial: a row's
			// fields may each have been written by somebody between
			// its creator and its last writer, whom the row does not
			// name.
			writers := []provenance.Author{read.writer(d.UpdatedBy.Name, d.UpdatedBy.Kind)}
			if d.CreatedBy != d.UpdatedBy {
				writers = append(writers, read.writer(d.CreatedBy.Name, d.CreatedBy.Kind))
			}
			read.fence(at.index(i).key("fields"), provenance.SourceArtifactRow, writers, true,
				string(d.Fields))
		}
		if len(d.Invalid) > 0 {
			row["invalid"] = d.Invalid
		}
		rows = append(rows, row)
	}
	out := map[string]any{"artifact": a.head.ID.String(), "collection": collection,
		"role": string(a.stamp), "rows": rows}
	if page != nil {
		out["through"] = page.Through
		if page.After != "" {
			out["next"] = page.After
		}
		if page.Resync {
			out["resync"] = true
			out["note"] = "`since` is older than the datastore can still account for: " +
				"read the collection again without `since`."
		}
		if !page.Complete {
			out["coverage"] = "partial: this node's copy may not hold every recent write"
		}
	}
	if a.requester != nil {
		out["filtered_for_requester"] = true
	}
	return read.answer(ctx, tool, out, "Ask for fewer rows with `limit`, or narrow the "+
		"question with "+QueryArtifactDataTool+".")
}
