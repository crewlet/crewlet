package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// A PERSON'S VIEWED FILES: the operator tool `code_mark_viewed`, a person's own marks over a change's
// files, decided by Gitlet on their record (gitlet.MarkWrite.Files).

// CodeMarkViewedTool is the tool's name, its authority row's action.
const CodeMarkViewedTool = string(authz.ActionCodeMarkViewed)

// codeViewedVerb is the verb a viewed mark's operation is named by.
const codeViewedVerb = "code-viewed"

type codeMarkViewed struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeMarkViewed)(nil)

func (t *codeMarkViewed) Name() string { return CodeMarkViewedTool }

func (t *codeMarkViewed) Description() string {
	return fmt.Sprintf("Mark files of a change as viewed, or take marks back: your own progress through a "+
		"change's files. Name the change by `repo` and `change`. `viewed` lists files to mark, each `{path, "+
		"blob_oid, mode}`: the path as `%s` part=files names it, and that file's `new_oid` and "+
		"`new_mode` from the same answer; omit both for a file the change deletes, which has no `new_oid`. "+
		"`unviewed` lists paths whose mark to take back. At most %d files a call. A mark stays while later "+
		"revisions give the file the same object id and mode, and reads unviewed once one changes either; mark "+
		"it again then. Only a draft or open change keeps marks, and you hold at most %d, your oldest going "+
		"first.", CodeGetChangeTool, gitlet.MaxViewedPerMark, gitlet.MaxViewedFiles)
}

func (t *codeMarkViewed) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{"type": "string", "description": "The repository's name."},
			"change": map[string]any{"type": "string",
				"description": "The change's number on `repo`, or its reference (web!42)."},
			"viewed": map[string]any{
				"type":        "array",
				"description": "Files to mark viewed, each as the revision you read gives it.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string"},
						"blob_oid": map[string]any{"type": "string",
							"description": "The file's `new_oid`; omit it for a file the change deletes."},
						"mode": map[string]any{"type": "string",
							"description": "The file's `new_mode`; omit it for a file the change deletes."},
					},
					"required": []string{"path"},
				},
			},
			"unviewed": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Paths whose viewed mark to take back."},
			"seen": map[string]any{
				"type": "string",
				"description": "How far you have read the code log, `<stream>@<generation>:<sequence>`: the " +
					"`position` " + CodeGetChangeTool + " answered with the files you mark. The change is " +
					"read at least that far before the mark is made. Omit it to read wherever the call is answered.",
			},
		},
		"required": []string{"repo", "change"},
	})
}

func (t *codeMarkViewed) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeMarkViewed) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.writeCaller(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	if c.asker.Kind != gitlet.PrincipalOperator || c.asker.Seat == "" {
		return refusedBy(tools.RefusalForbidden, gitlet.ErrNotPermitted, fmt.Sprintf("%s marked nothing: a "+
			"change's viewed files are a person's, marked by the person who reads them.", t.Name())), nil
	}
	actor, bad := t.deps.bindOperation(c.actor, t.Name(), args)
	if bad != "" {
		return failed(bad), nil
	}
	c.actor = actor
	name, number, refused := changeArg(t.Name(), args)
	if refused != nil {
		return *refused, nil
	}
	viewed, unviewed, bad := viewedGestureOf(t.Name(), args)
	if bad != "" {
		return failed(bad), nil
	}
	seen, bad := markFloor(args, topics.CodeLogStream, "code log", "pass the `position` "+CodeGetChangeTool+" answered")
	if bad != "" {
		return failed(bad), nil
	}
	g := &changeRead{deps: t.deps, turn: turn, tool: t.Name(), c: c, name: name, number: number}
	change, at, refusal := g.row(ctx, seen)
	if refusal != nil {
		return *refusal, nil
	}
	if !change.State.Live() {
		detail := fmt.Sprintf("%s is %s, and a change keeps viewed files only while it is a draft or open",
			change.Ref, change.State)
		return refusedBy(tools.RefusalConflict, writeRefusalError(string(gitlet.MarkEnded), detail),
			fmt.Sprintf("%s marked nothing: %s (%s). That change takes no viewed marks now.", t.Name(), detail,
				gitlet.MarkEnded)), nil
	}
	handle := c.asker.Seat
	opID := opIDFor(actor, t.Name(), codeViewedVerb, handle, args)
	res, err := t.deps.Writer.Mark(ctx, gitlet.MarkWrite{OpID: opID, Asker: c.asker, Handle: handle, Seen: at,
		Files: &gitlet.ViewedGesture{RepoID: change.RepoID, ChangeID: change.ID, Viewed: viewed,
			Unviewed: unviewed}})
	if err != nil {
		var unserved *estate.ErrUnserved
		if errors.Is(err, estate.ErrOutcomeUnknown) || errors.As(err, &unserved) && unserved.Silent() {
			return t.unknown(actor, change.Ref, opID), nil
		}
		return codeWriteFailure(ctx, t.Name(), actor, "Gitlet", err), nil
	}
	if res.Refusal != "" {
		return markRefused(t.Name(), res), nil
	}
	if res.Outcome == statelog.OutcomeUnknown {
		return t.unknown(actor, change.Ref, opID), nil
	}
	answer, _ := jsonResult(withOperation(map[string]any{
		"handle": handle, "repo": change.Repo, "change": change.Ref, "outcome": string(res.Outcome),
		"unchanged": res.Unchanged, "position": positionOf(res.Position),
	}, actor))
	return answer, nil
}

// unknown is a viewed mark whose outcome nobody could say.
func (t *codeMarkViewed) unknown(actor Actor, ref, opID string) tools.Result {
	return unknownWrite(actor, t.Name(), fmt.Sprintf("the files of %s were marked", ref), opID, false,
		unknownNext(false, sameCall(actor, t.Name()), "Read the change's files again",
			"it marks the same files again, which changes nothing"))
}

// viewedGestureOf is a call's `viewed` and `unviewed`, or why they are no gesture, in the model's
// words. Paths are kept exactly as sent, in both lists, so a mark and its take-back name the same
// file; Gitlet checks the rest.
func viewedGestureOf(tool string, args map[string]any) ([]gitlet.ViewedFile, []string, string) {
	var viewed []gitlet.ViewedFile
	if raw, present := args["viewed"]; present && raw != nil {
		list, ok := argval.List(raw)
		if !ok {
			return nil, nil, fmt.Sprintf("%s: `viewed` is a list of files, each `{path, blob_oid, mode}`.", tool)
		}
		for i, item := range list {
			f, isObject := argval.Object(item)
			if !isObject {
				return nil, nil, fmt.Sprintf("%s: `viewed` item %d is no object; name each file as `{path, "+
					"blob_oid, mode}`.", tool, i)
			}
			file := gitlet.ViewedFile{Path: argString(f, "path"), BlobOID: strings.TrimSpace(argString(f, "blob_oid")),
				Mode: strings.TrimSpace(argString(f, "mode"))}
			switch {
			case file.Path == "":
				return nil, nil, fmt.Sprintf("%s: `viewed` item %d names no `path`.", tool, i)
			case (file.BlobOID == "") != (file.Mode == ""):
				return nil, nil, fmt.Sprintf("%s: `viewed` item %d (%s) names one of `blob_oid` and `mode`: pass "+
					"both, the file's `new_oid` and `new_mode`, or neither for a file the change deletes.", tool, i,
					clip(file.Path))
			}
			viewed = append(viewed, file)
		}
	}
	unviewed, ok := argPaths(args, "unviewed")
	if !ok {
		return nil, nil, fmt.Sprintf("%s: `unviewed` is a list of paths.", tool)
	}
	if i := slices.Index(unviewed, ""); i >= 0 {
		return nil, nil, fmt.Sprintf("%s: `unviewed` item %d is no path.", tool, i)
	}
	switch n := len(viewed) + len(unviewed); {
	case n == 0:
		return nil, nil, fmt.Sprintf("%s marked nothing: name the files in `viewed` or `unviewed`.", tool)
	case n > gitlet.MaxViewedPerMark:
		return nil, nil, fmt.Sprintf("%s: %d files named, past the %d one call marks; split them across calls.",
			tool, n, gitlet.MaxViewedPerMark)
	}
	return viewed, unviewed, ""
}
