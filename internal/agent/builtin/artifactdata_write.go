package builtin

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// ---- write_artifact_data ------------------------------------------------- //

type writeArtifactData struct {
	artifactTarget
	d dataTools
}

var _ tools.SeatCallable = (*writeArtifactData)(nil)

func (t *writeArtifactData) Name() string { return WriteArtifactDataTool }

func (t *writeArtifactData) Description() string {
	return "Write rows of an artifact's datastore in this Crewlet company: a batch of " +
		"1 to 100 ops, applied all or none, each decided by its collection's rules. " +
		"`set` replaces a row's fields (creating it), `merge` sets the named fields, " +
		"`delete` removes a row, `increment` adds to a number, `append` grows a long " +
		"text or a list, `add` and `remove` move one option of a multiselect. Pass " +
		"`if_version` (the row's `version` you read) to write only if nobody changed it " +
		"since; a conflict hands back the rows as they are now."
}

func (t *writeArtifactData) Parameters() map[string]any {
	return t.d.art.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"ops": map[string]any{
				"type": "array", "minItems": 1, "maxItems": artifacts.MaxBatchOps,
				"items": map[string]any{
					"type":     "object",
					"required": []any{"op", "collection", "doc"},
					"properties": map[string]any{
						"op":         map[string]any{"type": "string", "enum": dataOpNames()},
						"collection": map[string]any{"type": "string"},
						"doc":        map[string]any{"type": "string", "description": "The row's id."},
						"if_version": map[string]any{"type": "integer",
							"description": "Write only if the row is still at this version."},
						"fields": map[string]any{"type": "object",
							"description": "A set's or a merge's field values."},
						"field": map[string]any{"type": "string",
							"description": "The one field an increment, append, add or remove changes."},
						"value": map[string]any{"description": "Its operand."},
					},
				},
			},
		},
		"required": []any{"artifact", "ops"},
	})
}

// dataOpNames is every op, for the schema's enum.
func dataOpNames() []any {
	out := make([]any, 0, len(artifacts.DataOpKinds))
	for _, op := range artifacts.DataOpKinds {
		out = append(out, string(op))
	}
	return out
}

func (t *writeArtifactData) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *writeArtifactData) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	var ops []artifacts.DataOp
	if err := argJSON(args, "ops", &ops); err != nil || len(ops) == 0 {
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return failed(fmt.Sprintf("%s needs `ops`: a list of 1 to %d {op, collection, doc, "+
			"...} objects.", t.Name(), artifacts.MaxBatchOps)), nil
	}
	if len(ops) > artifacts.MaxBatchOps {
		return failed(fmt.Sprintf("%s takes at most %d ops in one call; split the batch.",
			t.Name(), artifacts.MaxBatchOps)), nil
	}
	for i := range ops {
		for key, raw := range ops[i].Fields {
			ops[i].Fields[key] = unfenceValue(turn, raw)
		}
		ops[i].Value = unfenceValue(turn, ops[i].Value)
	}
	a, refusal := t.d.admit(ctx, turn, t.Name(), authz.ActionArtifactDataWrite, args, true)
	if refusal != nil {
		return *refusal, nil
	}
	if a.head.Status == artifactsdomain.StatusTrashed {
		// THE ARTIFACT'S STATE, under its own sentinel: never the
		// datastore's ErrGone, which is a purge's and says the data will
		// not come back.
		return refusedBy(tools.RefusalConflict, artifactsdomain.ErrTrashed, fmt.Sprintf("%s: the "+
			"artifact is in the trash, and its data is not written until a person restores "+
			"it. Nothing was written.", t.Name())), nil
	}
	if a.c.agent() && a.c.turn != nil {
		if err := t.d.data.Turns.spend(a.c.turn.RunID, a.c.key, len(ops)); err != nil {
			return dataFailure(ctx, t.Name(), err), nil
		}
	}
	t.d.touch(a.c, a.head.ID, presence.Editing,
		presence.Focus{Collection: ops[0].Collection, Doc: ops[0].Doc})
	res, err := t.d.data.Store.Write(ctx, t.d.art.call(a.c, a.stamp, a.dec.Elevated),
		a.head.ID, artifacts.DataWrite{Ops: ops})
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	return t.d.verdict(ctx, turn, t.Name(), a, res, "Read the rows with "+ReadArtifactDataTool+": "+
		"they say whether the write landed.")
}

// conflictRows is the part of a conflict's current rows this call may hand
// back, and whether any was withheld: every row where the turn acts for
// nobody, and otherwise only the rows the requester's reading admits by their
// collection's own rule, exactly as a read is filtered. The store read them at
// the agent's record, so without this an agent's own rows in an `own_rows` or
// `private` collection would reach a requester the read path keeps them from.
// A schema this node could not read withholds every row: the closed end.
func (d dataTools) conflictRows(ctx context.Context, a dataAdmission,
	docs []artifactdata.Doc) ([]artifactdata.Doc, bool) {

	if a.requester == nil {
		return docs, false
	}
	schema, err := d.data.Store.Schema(ctx, a.head.ID)
	if err != nil {
		log.WarnContext(ctx, "artifact_data_conflict_rows_withheld", "artifact",
			a.head.ID.String(), "error", err.Error(),
			"detail", "the schema could not be read, so no row could be checked against "+
				"what the requester may read")
		return nil, len(docs) > 0
	}
	out := make([]artifactdata.Doc, 0, len(docs))
	for _, doc := range docs {
		if a.shows(schema, doc.Collection, doc) {
			out = append(out, doc)
		}
	}
	return out, len(out) < len(docs)
}

// verdict answers a datastore write by what the applier decided: applied, or
// the refusal it wrote (a conflict handing back the rows as they are now,
// fenced, since others wrote them, and only those the requester could read),
// or the outcome nobody can vouch for.
func (d dataTools) verdict(ctx context.Context, turn *turnctx.Turn, tool string, a dataAdmission,
	res artifactdata.WriteResult, look string) (tools.Result, error) {

	if res.Result.Outcome == statelog.OutcomeUnknown {
		return artifactUnknown(tool, a.c, res.Result, look), nil
	}
	out := map[string]any{"artifact": a.head.ID.String(), "verdict": string(res.Verdict),
		"seq": res.Seq, "position": positionOf(res.Result.Position)}
	switch res.Verdict {
	case artifacts.DataApplied:
		return jsonResult(withArtifactOperation(out, a.c))
	case "":
		// ON THE LOG AND NOT YET APPLIED HERE: the verdict is decided, and
		// this node has not caught up to read it.
		out["verdict"] = "pending"
		said := partial(out, NoteKey, caveat.DatastorePending("The write is on the "+
			"datastore's log and this node has not applied it yet, so its verdict is "+
			"not known here. "+look))
		return caveated(withArtifactOperation(out, a.c), nil, said)
	case artifacts.DataConflict:
		read := newForeignRead(turn, a.c.self())
		shown, withheld := d.conflictRows(ctx, a, res.Conflict)
		rows := make([]map[string]any, 0, len(shown))
		for i, doc := range shown {
			row := map[string]any{"collection": doc.Collection, "doc": doc.ID, "version": doc.Version,
				"written_by": actorJSON(doc.UpdatedBy)}
			if doc.Deleted {
				row["deleted"] = true
			}
			if len(doc.Fields) > 0 {
				row["fields"] = string(doc.Fields)
				read.fence(fieldPath{}.key("current").index(i).key("fields"),
					provenance.SourceArtifactRow,
					[]provenance.Author{read.writer(doc.UpdatedBy.Name, doc.UpdatedBy.Kind)}, true,
					string(doc.Fields))
			}
			rows = append(rows, row)
		}
		out["current"] = rows
		out["refusal"] = "conflict"
		note := "Nothing was written: a row moved past the `if_version` you sent. " +
			"`current` is each such row as it is now; merge onto it and write again with " +
			"its `version`."
		if withheld {
			note = "Nothing was written: a row moved past the `if_version` you sent. " +
				"`current` is each such row the person this turn is working for may read, " +
				"as it is now; the others are not shown. Merge onto what is shown and write " +
				"again with its `version`."
		}
		// A REFUSAL, answered as one below: its note is the model's alone,
		// and a person's surface answers the conflict's own sentence — and
		// no receipt, so the rows rendered for one are not kept either.
		out[NoteKey] = note
		r, err := read.answer(ctx, tool, withArtifactOperation(out, a.c),
			"Write fewer rows at once.")
		if err == nil {
			r.Failed = true
			r.Cause = tools.Classify(tools.RefusalConflict, artifactdata.ErrConflict)
			r.PersonReceipt = nil
		}
		return r, err
	}
	class := DataVerdictClass(res.Verdict)
	refusal := refusedBy(class, fmt.Errorf("artifactdata: %s: %s", res.Verdict, res.Reason),
		fmt.Sprintf("%s did not write anything: the datastore refused the batch (%s: %s). "+
			"No op of it landed.", tool, res.Verdict, res.Reason))
	if res.Verdict == artifacts.DataQuota {
		// A LIMIT ON WHAT THE DATA MAY HOLD, its own class, in a person's
		// words beside the model's sentence (ADR-0060): the sentence names
		// the applier's reason, a collection, a count or a Tier A key, and a
		// person's surface answers the catalog's phrasing in its place, as a
		// publish past the company's storage is answered.
		return toPerson(refusal, caveat.DatastoreQuotaReached()), nil
	}
	return refusal, nil
}

// ---- set_artifact_schema ------------------------------------------------- //

type setArtifactSchema struct {
	artifactTarget
	d dataTools
}

var _ tools.SeatCallable = (*setArtifactSchema)(nil)

func (t *setArtifactSchema) Name() string { return SetArtifactSchemaTool }

func (t *setArtifactSchema) Description() string {
	return "Set the schema of an artifact's datastore in this Crewlet company: EVERY " +
		"collection it keeps after the change (read the current one with " +
		ReadArtifactDataTool + " and no `collection`, and send it back with your change), " +
		"each with its columns, who reads it, who creates and deletes rows, and each " +
		"column's write rule. An agent may tighten a collection's rules (add " +
		"`humans_only`, raise a role) and never loosen them; that is a person's call."
}

func (t *setArtifactSchema) Parameters() map[string]any {
	return t.d.art.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"collections": map[string]any{
				"type": "array", "maxItems": artifacts.MaxCollections,
				"items": map[string]any{
					"type":     "object",
					"required": []any{"name"},
					"properties": map[string]any{
						"name":   map[string]any{"type": "string"},
						"title":  map[string]any{"type": "string"},
						"preset": map[string]any{"type": "string", "enum": presetNames()},
						"read":   map[string]any{"type": "string", "enum": readRuleNames()},
						"create": map[string]any{"type": "string", "enum": roleNames()},
						"delete": map[string]any{"type": "string", "enum": roleNames()},
						"columns": map[string]any{"type": "array",
							"maxItems": artifacts.MaxColumns,
							"items":    map[string]any{"type": "object"},
							"description": "Each {key, type, label?, required?, options?, " +
								"write?, humans_only?, ...}."},
						"indexed": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
				"description": "Every collection the datastore keeps after the change.",
			},
		},
		"required": []any{"artifact", "collections"},
	})
}

func presetNames() []any {
	out := make([]any, 0, len(artifacts.Presets))
	for _, p := range artifacts.Presets {
		out = append(out, string(p))
	}
	return out
}

func readRuleNames() []any {
	out := make([]any, 0, len(artifacts.ReadRules))
	for _, r := range artifacts.ReadRules {
		out = append(out, string(r))
	}
	return out
}

func roleNames() []any {
	out := make([]any, 0, len(artifacts.Roles))
	for _, r := range artifacts.Roles {
		out = append(out, string(r))
	}
	return out
}

func (t *setArtifactSchema) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *setArtifactSchema) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	var collections []artifacts.Collection
	if err := argJSON(args, "collections", &collections); err != nil {
		return failed(fmt.Sprintf("%s could not read `collections`: %v. Send every "+
			"collection as {name, read, create, delete, columns, ...}.", t.Name(), err)), nil
	}
	if _, given := args["collections"]; !given {
		return failed(t.Name() + " needs `collections`: every collection the datastore " +
			"keeps after the change, read first with " + ReadArtifactDataTool + "."), nil
	}
	a, refusal := t.d.admit(ctx, turn, t.Name(), authz.ActionArtifactSchema, args, true)
	if refusal != nil {
		return *refusal, nil
	}
	if a.head.Status == artifactsdomain.StatusTrashed {
		return refusedBy(tools.RefusalConflict, artifactsdomain.ErrTrashed, fmt.Sprintf("%s: the "+
			"artifact is in the trash. Nothing was changed.", t.Name())), nil
	}
	t.d.touch(a.c, a.head.ID, presence.Editing, presence.Focus{})
	res, err := t.d.data.Store.Configure(ctx, t.d.art.call(a.c, a.stamp, a.dec.Elevated),
		a.head.ID, collections)
	if err != nil {
		return dataFailure(ctx, t.Name(), err), nil
	}
	return t.d.verdict(ctx, turn, t.Name(), a, res, "Read the schema with "+ReadArtifactDataTool+": "+
		"it says whether the change landed.")
}

// argJSON decodes one argument into v through its JSON form, so a number keeps
// the decimal text the caller sent. An absent argument decodes to nothing.
func argJSON(args map[string]any, key string, v any) error {
	raw, ok := args[key]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// unfenceValue is a value as a write stores it: a string a model copied back
// out of a fence this turn wrote is stored without the fence ([unfence]).
func unfenceValue(turn *turnctx.Turn, raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || raw[0] != '"' {
		return raw
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	clean := unfence(turn, s)
	if clean == s {
		return raw
	}
	out, err := json.Marshal(clean)
	if err != nil {
		return raw
	}
	return out
}
