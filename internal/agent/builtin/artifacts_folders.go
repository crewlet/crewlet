package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/foldername"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// FOLDERS, AS AN AGENT FILES IN THEM (ADR-0047: an artifact lives in one
// folder, and a folder grants nothing).
//
// A folder organises; it never shares. So filing is safe to hand an agent: the
// worst a persuaded agent can do is misfile, never widen who may open
// anything. What filing still asks is two decisions, each through the
// requester ceiling: the artifact's (`move_artifact`, editor on it) and the
// destination's (`artifacts.folder.file`, contributor on it), so an agent
// outside Finance cannot plant a page in Finance's tree. Reorganising a
// team's folders (rename, move, remove, unfile) is the folder owner's and a
// person's gesture, never a tool's.
//
// A FOLDER NAME IS SOMETHING A REQUESTER MAY NOT SEE: a reference resolves only
// among the folders the caller AND the person the turn acts for can both see,
// a missing one is answered with the nearest paths they both see, and a turn
// a stranger woke sees no folder at all and files nothing in one (a stranger
// opens no artifact, and a folder's name is as much the company's as an
// artifact's title is).

// folderView is who a call's folder references resolve among: the caller and,
// where the turn acts for somebody, that requester.
type folderView struct {
	within    []artifactsdomain.Viewer
	requester *artifactsdomain.Viewer
}

// folderView is the call's view of the tree, or the refusal: a stranger's turn
// sees no folder, and a chart this node cannot read decides no visibility.
func (d ArtifactDeps) folderView(ctx context.Context, c artifactCaller, tool string) (folderView, *tools.Result) {
	if c.ceiling.Kind == authz.CeilingStranger {
		r := refused(tools.RefusalForbidden, fmt.Sprintf("%s: this turn was woken by somebody "+
			"outside the company, who sees no folder, so you neither read a folder nor file in "+
			"one for them. Leave folders out.", tool))
		return folderView{}, &r
	}
	self, err := d.viewer(ctx, c)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read the "+
			"company chart, which decides which folders you can see. Nothing was read or "+
			"changed. Try again shortly.", tool))
		return folderView{}, &r
	}
	view := folderView{within: []artifactsdomain.Viewer{self}}
	if c.ceiling.Kind == authz.CeilingRequester {
		req, err := d.viewerOf(ctx, c.ceiling.Principal, c.turn)
		if err != nil {
			r := refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read the "+
				"company chart, which decides which folders the person this turn is working "+
				"for can see. Nothing was read or changed. Try again shortly.", tool))
			return folderView{}, &r
		}
		view.within = append(view.within, req)
		view.requester = &req
	}
	return view, nil
}

// visibleFolder is one folder of the tree the call may name, by its path.
type visibleFolder struct {
	folder artifactsdomain.Folder
	path   string
}

// visibleFolders is every live folder the caller and the requester can both
// see, with its path from the top level ("Marketing/Q3"), parents first: the
// caller's own tree read, narrowed by deciding each folder's visibility for the
// requester (a tree's visibility is its top-level folder's, so every ancestor
// of a folder kept is kept).
func (d ArtifactDeps) visibleFolders(ctx context.Context, c artifactCaller, view folderView) (
	[]visibleFolder, error) {

	tree, err := d.Reader.Folders(ctx, view.within[0], seatRead)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	out := make([]visibleFolder, 0, len(tree.Folders))
	for _, item := range tree.Folders {
		p := item.Name
		if item.Parent != "" {
			parent, ok := paths[item.Parent]
			if !ok {
				continue // under a folder this caller does not see; never answered
			}
			p = parent + artifactsdomain.ListDelimiter + item.Name
		}
		if view.requester != nil {
			seen, err := authz.Visible(ctx, c.ceiling.Principal,
				artifactsdomain.FolderObject(item.Folder, d.manageOthers()), d.Chart, d.now())
			if err != nil {
				return nil, err
			}
			if !seen {
				continue
			}
		}
		paths[item.ID] = p
		out = append(out, visibleFolder{folder: item.Folder, path: p})
	}
	return out, nil
}

// pathOf is the path of folder id in visible, empty when it is not there.
func pathOf(visible []visibleFolder, id string) string {
	i := slices.IndexFunc(visible, func(v visibleFolder) bool { return v.folder.ID == id })
	if i < 0 {
		return ""
	}
	return visible[i].path
}

// resolveFolder is the folder ref names (an id, or a path of names from the top
// level) among the folders the call may name, with its path and every folder
// the call may name, or the refusal: none (answered with the nearest paths the
// call may name), several (each candidate by id, owner and audience), or a read
// this node could not serve.
func (d ArtifactDeps) resolveFolder(ctx context.Context, c artifactCaller, tool, ref string,
	view folderView) (artifactsdomain.Folder, string, []visibleFolder, *tools.Result) {

	f, err := d.Reader.ResolveFolder(ctx, ref, view.within, seatRead)
	var ambiguous *artifactsdomain.FolderAmbiguous
	switch {
	case errors.As(err, &ambiguous):
		visible, verr := d.visibleFolders(ctx, c, view)
		if verr != nil {
			r := folderReadFailure(ctx, tool, verr)
			return artifactsdomain.Folder{}, "", nil, &r
		}
		lines := make([]string, 0, len(ambiguous.Candidates))
		for _, cand := range ambiguous.Candidates {
			if pathOf(visible, cand.ID) == "" {
				continue // a candidate the requester cannot see is never named
			}
			lines = append(lines, fmt.Sprintf("- %s: owned by %s, %s", cand.ID, cand.Owner,
				audienceOf(cand.Visibility)))
		}
		if len(lines) < 2 {
			r := folderNotFound(tool, ref, nearestFolders(visible, ref))
			return artifactsdomain.Folder{}, "", nil, &r
		}
		r := refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s: %q names %d folders you "+
			"can see:\n%s\nPass the one you mean by its id.", tool, clip(ref), len(lines),
			strings.Join(lines, "\n")))
		return artifactsdomain.Folder{}, "", nil, &r
	case errors.Is(err, artifactsdomain.ErrFolderNotFound):
		visible, verr := d.visibleFolders(ctx, c, view)
		if verr != nil {
			r := folderReadFailure(ctx, tool, verr)
			return artifactsdomain.Folder{}, "", nil, &r
		}
		r := folderNotFound(tool, ref, nearestFolders(visible, ref))
		return artifactsdomain.Folder{}, "", nil, &r
	case err != nil:
		r := folderReadFailure(ctx, tool, err)
		return artifactsdomain.Folder{}, "", nil, &r
	}
	visible, err := d.visibleFolders(ctx, c, view)
	if err != nil {
		r := folderReadFailure(ctx, tool, err)
		return artifactsdomain.Folder{}, "", nil, &r
	}
	p := pathOf(visible, f.ID)
	if p == "" {
		// RESOLVED AMONG BOTH, and absent from the tree read a moment
		// later: removed, moved or narrowed in between. The one answer for
		// a folder the call may not name.
		r := folderNotFound(tool, ref, nearestFolders(visible, ref))
		return artifactsdomain.Folder{}, "", nil, &r
	}
	return f, p, visible, nil
}

// chainExists reports whether every folder of names beneath the folder at
// parentPath is one the call can already see, compared by the key the domain
// decides sibling names on ([foldername.Key]): a create the writer would answer
// with nothing written.
func chainExists(visible []visibleFolder, parentPath string, names []string) bool {
	held := make(map[string]bool, len(visible))
	for _, v := range visible {
		held[pathKey(v.path)] = true
	}
	path := parentPath
	for _, name := range names {
		path += artifactsdomain.ListDelimiter + name
		if !held[pathKey(path)] {
			return false
		}
	}
	return true
}

// pathKey is a folder path as the domain compares it, name by name.
func pathKey(path string) string {
	names := strings.Split(path, artifactsdomain.ListDelimiter)
	for i, n := range names {
		names[i] = foldername.Key(n)
	}
	return strings.Join(names, artifactsdomain.ListDelimiter)
}

// audienceOf is a folder's preset as a refusal names it.
func audienceOf(v artifactsdomain.FolderVisibility) string {
	switch v {
	case artifactsdomain.FolderCompany:
		return "everyone at the company"
	case artifactsdomain.FolderPrivate:
		return "only its owner"
	}
	return "shared with some people"
}

// folderNotFound is the ONE answer for a folder nothing holds and for one the
// call may not name, with the nearest paths it may.
func folderNotFound(tool, ref string, nearest []string) tools.Result {
	hint := "You can see no folder: file at the top level, or ask a person to make one."
	if len(nearest) > 0 {
		hint = "The nearest you can see: " + strings.Join(nearest, ", ") + "."
	}
	return refusedBy(tools.RefusalNotFound, artifactsdomain.ErrFolderNotFound, fmt.Sprintf(
		"%s: there is no folder %q you can see. %s No folder was created.", tool, clip(ref), hint))
}

// folderReadFailure explains a read of the tree that did not answer.
func folderReadFailure(ctx context.Context, tool string, err error) tools.Result {
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not read the "+
			"folders right now (%s). Nothing was changed. Try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not read the folders: %s. Nothing "+
		"was changed: say you could not check.", tool, faultSaid))
}

// nearestFolderCount is how many paths a missing folder is answered with: few
// enough to read at a glance, enough to cover a typo and a sibling.
const nearestFolderCount = 3

// nearestFolders is the paths of visible nearest to ref, by edit distance on
// the case-folded path, the nearest first and ties A to Z.
func nearestFolders(visible []visibleFolder, ref string) []string {
	want := strings.ToLower(strings.Trim(strings.TrimSpace(ref), artifactsdomain.ListDelimiter))
	type scored struct {
		path string
		d    int
	}
	all := make([]scored, 0, len(visible))
	for _, v := range visible {
		all = append(all, scored{v.path, editDistance(want, strings.ToLower(v.path))})
	}
	slices.SortFunc(all, func(a, b scored) int {
		if a.d != b.d {
			return a.d - b.d
		}
		return strings.Compare(a.path, b.path)
	})
	out := make([]string, 0, nearestFolderCount)
	for i := 0; i < len(all) && i < nearestFolderCount; i++ {
		out = append(out, all[i].path)
	}
	return out
}

// editDistance is the Levenshtein distance between a and b, by rune.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// decideFiling asks whether the call may file into folder f (contributor on it,
// through the requester ceiling), answering the role the decision found or
// the refusal.
func (d ArtifactDeps) decideFiling(ctx context.Context, c artifactCaller, tool string,
	f artifactsdomain.Folder, p string) (artifacts.Role, *tools.Result) {

	dec := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactFolderFile,
		artifactsdomain.FolderObject(f, d.manageOthers()), d.Chart, d.now())
	switch {
	case dec.Unknown():
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, dec.Err),
			fmt.Sprintf("%s could not check whether you may file in %q right now (the company "+
				"chart or the requester could not be read). Nothing was changed. Try again "+
				"shortly.", tool, p))
		return "", &r
	case dec.Allowed && !dec.Elevated:
		return dec.Standing.Role, nil
	case dec.NotFound():
		r := folderNotFound(tool, p, nil)
		return "", &r
	}
	r := authorityRefusal(tool, DecisionError(authz.ActionArtifactFolderFile, dec))
	switch dec.Reason {
	case authz.ReasonCeiling:
		r.Output = fmt.Sprintf("%s didn't run: the person this turn is working for may not "+
			"file in %q themselves, so you may not file there for them. Leave it at the top "+
			"level, or tell them who owns the folder (%s).", tool, p, f.Owner)
	default:
		r.Output = fmt.Sprintf("%s didn't run: filing in %q needs the contributor role on it, "+
			"and you hold %s. Ask its owner (%s) to let you file there, or file elsewhere.",
			tool, p, roleOrNone(dec.Standing.Role), f.Owner)
	}
	return "", &r
}

// folderWriteFailure explains a folder write the domain refused.
func folderWriteFailure(ctx context.Context, tool string, c artifactCaller, err error) tools.Result {
	switch {
	case errors.Is(err, artifactsdomain.ErrFolderNotFound):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: the folder was removed "+
			"or moved out of your sight while this ran. Nothing was changed; list the folders "+
			"again with list_artifacts and `delimiter`.", tool))
	case errors.Is(err, artifactsdomain.ErrFolderDepth):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: folders go "+
			"at most %d deep, counting the top level. File in a shallower folder.", tool,
			artifacts.MaxFolderDepth))
	case errors.Is(err, artifactsdomain.ErrFolderLimit):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: the "+
			"company, or that folder, already holds as many folders as it may (%d in all, %d in "+
			"one folder). File into a folder that exists.", tool, artifacts.MaxFolders,
			artifacts.MaxFolderChildren))
	case errors.Is(err, artifactsdomain.ErrFolderNameTaken):
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s: %s", tool, domainSentence(err)))
	}
	return publishFailure(ctx, tool, c, err)
}

// ---- create_artifact_folder ---------------------------------------------- //

type createArtifactFolder struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*createArtifactFolder)(nil)

func (t *createArtifactFolder) Name() string { return CreateArtifactFolderTool }

func (t *createArtifactFolder) Description() string {
	return "Create a folder for artifacts in this Crewlet company, inside a folder that " +
		"already exists. Folders organise and never share: filing an artifact changes where " +
		"people find it, never who can open it. `parent` is the existing folder (its id, or " +
		"its path from the top level such as `Marketing/Campaigns`); `name` is the new folder, " +
		"or a path of names (`Q3/Launch`) whose missing folders are all made. A folder that " +
		"already exists is answered as it is. Only a person creates a top-level folder. " +
		"Prefer an existing folder: make one when a person asked for it, or for three or " +
		"more related artifacts no folder fits."
}

func (t *createArtifactFolder) Parameters() map[string]any {
	return t.deps.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"parent": map[string]any{
				"type": "string",
				"description": "The existing folder to create in: its `folder` id, or its path " +
					"of names from the top level (`Marketing/Campaigns`).",
			},
			"name": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("The new folder's name, one to four words and at most "+
					"%d bytes, or a path of names (`Q3/Launch`); folders go at most %d deep.",
					artifacts.MaxFolderNameBytes, artifacts.MaxFolderDepth),
			},
		},
		"required": []any{"parent", "name"},
	})
}

func (t *createArtifactFolder) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *createArtifactFolder) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	ref := strings.TrimSpace(argString(args, "parent"))
	if ref == "" {
		return failed("create_artifact_folder needs a `parent`: the folder to create in, by id " +
			"or path. Only a person creates a top-level folder."), nil
	}
	names, why := folderNames(argString(args, "name"))
	if why != "" {
		return failed(why), nil
	}
	view, refusal := t.deps.folderView(ctx, c, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	parent, parentPath, visible, refusal := t.deps.resolveFolder(ctx, c, t.Name(), ref, view)
	if refusal != nil {
		return *refusal, nil
	}
	owner := iam.RecordOwner(c.principal)
	// THE CREATOR OWNS WHAT IT MAKES, so the table asks the app (and the
	// ceiling spares the agent's own new work, [authz.DecideWithin]); the
	// parent's contributor role is the second decision.
	dec := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactFolderCreate,
		authz.Object{Kind: authz.KindArtifactFolder, Owner: owner}, t.deps.Chart, t.deps.now())
	if dec.Unknown() || !dec.Allowed {
		return authorityRefusal(t.Name(), DecisionError(authz.ActionArtifactFolderCreate, dec)), nil
	}
	role, refusal := t.deps.decideFiling(ctx, c, t.Name(), parent, parentPath)
	if refusal != nil {
		return *refusal, nil
	}
	// THE TURN'S GUARDRAIL, an agent's alone, charged before the write and
	// given back when the write made nothing. A chain the call can already
	// see is answered as it is, and charges nothing even past the bound.
	refund := func() {}
	if c.agent() && c.turn != nil && !chainExists(visible, parentPath, names) {
		give, used, ok := t.deps.FolderTurns.spend(c.turn.RunID, c.key)
		if !ok {
			return refused(tools.RefusalForbidden, fmt.Sprintf("create_artifact_folder refused "+
				"that (turn_quota): this turn has made %d of its %d folders. File into the folders "+
				"there are, report what is left, and let the next turn continue.", used,
				artifacts.TurnFolderCreates)), nil
		}
		refund = give
	}
	created, err := t.deps.Writer.CreateFolders(ctx, t.deps.call(c, role, false),
		artifactsdomain.FolderCreateInput{Parent: parent.ID, Names: names, Parents: true,
			Owner: owner, OwnerKind: c.actor.Kind})
	if err != nil {
		refund()
		return folderWriteFailure(ctx, t.Name(), c, err), nil
	}
	if created.Result.Outcome == statelog.OutcomeUnknown {
		return artifactUnknown(t.Name(), c, created.Result, "List the folder with list_artifacts "+
			"and `delimiter` before creating it another way: if it landed, it is there."), nil
	}
	chain := make([]map[string]any, 0, len(created.Folders))
	path := parentPath
	for _, f := range created.Folders {
		path += artifactsdomain.ListDelimiter + f.Name
		chain = append(chain, map[string]any{"folder": f.ID, "path": path})
	}
	if len(created.Folders) == 0 {
		refund()
		return faulted(ctx, t.Name(), errors.New("builtin: a folder create answered no folder"),
			"create_artifact_folder could not tell which folder it made: "+faultSaid+"."), nil
	}
	out := map[string]any{"folder": created.Folders[len(created.Folders)-1].ID, "path": path,
		"folders": chain}
	if created.Created == 0 {
		refund()
		out["outcome"] = "exists"
	} else {
		t.deps.settle(ctx, created.Result.Position)
		out["outcome"], out["created"] = "created", created.Created
		out["write"], out["position"] = outcomeOf(created.Result), positionOf(created.Result.Position)
	}
	return jsonResult(withArtifactOperation(out, c))
}

// folderNames reads a folder name, or a path of names separated by "/", into
// the chain a create makes, or the refusal. Each name's own grammar is the
// writer's (internal/artifacts/foldername), refused there by name.
func folderNames(raw string) ([]string, string) {
	text := strings.Trim(strings.TrimSpace(raw), artifactsdomain.ListDelimiter)
	if text == "" {
		return nil, "create_artifact_folder needs a `name`: one to four words for what the " +
			"folder holds."
	}
	parts := strings.Split(text, artifactsdomain.ListDelimiter)
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		name := strings.TrimSpace(p)
		if name == "" {
			return nil, fmt.Sprintf("create_artifact_folder: %q has an empty name between two "+
				"slashes; name each folder of the path.", clip(raw))
		}
		names = append(names, name)
	}
	if len(names) >= artifacts.MaxFolderDepth {
		return nil, fmt.Sprintf("create_artifact_folder: %q names %d folders, and beneath an "+
			"existing folder a path holds at most %d (folders go at most %d deep).", clip(raw),
			len(names), artifacts.MaxFolderDepth-1, artifacts.MaxFolderDepth)
	}
	return names, ""
}

// ---- move_artifact ------------------------------------------------------ //

type moveArtifact struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*moveArtifact)(nil)

func (t *moveArtifact) Name() string { return MoveArtifactTool }

func (t *moveArtifact) Description() string {
	return fmt.Sprintf("File artifacts in a folder in this Crewlet company, or take them out "+
		"to the top level: up to %d at once, each decided and filed on its own, and the answer "+
		"says what happened to each. A folder grants nothing: moving an artifact changes where "+
		"people find it, never who can open it. You move an artifact you may edit into a folder "+
		"you may file in. Never move an artifact you do not own unless a person asked.",
		artifacts.MaxMovesPerCall)
}

func (t *moveArtifact) Parameters() map[string]any {
	return t.deps.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifacts": map[string]any{
				"type": "array", "items": map[string]any{"type": "string"},
				"minItems": 1, "maxItems": artifacts.MaxMovesPerCall,
				"description": "The artifacts to move: each its `artifact` id or dashboard address.",
			},
			"folder": map[string]any{
				"type": "string",
				"description": "Where they go: a folder's id, or its path of names from the top " +
					"level (`Marketing/Q3`). An empty string is the top level.",
			},
		},
		"required": []any{"artifacts", "folder"},
	})
}

func (t *moveArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *moveArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	refs, why := moveRefs(args)
	if why != "" {
		return failed(why), nil
	}
	if _, given := args["folder"]; !given {
		return failed("move_artifact needs a `folder`: a folder's id or path, or an empty " +
			"string for the top level."), nil
	}
	ref := strings.TrimSpace(argString(args, "folder"))
	var dest, destPath string
	if ref != "" {
		view, refusal := t.deps.folderView(ctx, c, t.Name())
		if refusal != nil {
			return *refusal, nil
		}
		f, p, _, refusal := t.deps.resolveFolder(ctx, c, t.Name(), ref, view)
		if refusal != nil {
			return *refusal, nil
		}
		// THE DESTINATION IS DECIDED ONCE, before any artifact: a refusal
		// there refuses the whole call with nothing moved.
		if _, refusal := t.deps.decideFiling(ctx, c, t.Name(), f, p); refusal != nil {
			return *refusal, nil
		}
		dest, destPath = f.ID, p
	}
	results := make([]map[string]any, 0, len(refs))
	var firstCause error
	landed := 0
	for _, r := range refs {
		res, cause := t.moveOne(ctx, c, r, dest)
		if cause != nil && firstCause == nil {
			firstCause = cause
		}
		if o := res["outcome"]; o == "moved" || o == "unchanged" {
			landed++
		}
		results = append(results, res)
	}
	out := map[string]any{"folder": dest, "path": destPath, "moved": landed, "results": results}
	if landed == 0 {
		rendered, err := jsonResult(withArtifactOperation(out, c))
		if err != nil {
			return rendered, err
		}
		// NOTHING LANDED, so the call failed, classed as its first item's
		// refusal; each item's own reason is in its result.
		rendered.Failed, rendered.Cause = true, firstCause
		return rendered, nil
	}
	return jsonResult(withArtifactOperation(out, c))
}

// moveOne files one artifact in dest ("" for the top level): its own decision
// and its own record. The result says what happened, and the cause is the
// refusal's where it did not land.
func (t *moveArtifact) moveOne(ctx context.Context, c artifactCaller, ref, dest string) (
	map[string]any, error) {

	res := map[string]any{"artifact": ref}
	refuse := func(r tools.Result) (map[string]any, error) {
		res["outcome"], res["message"] = "refused", r.Output
		return res, r.Cause
	}
	h, _, refusal := t.deps.head(ctx, t.Name(), map[string]any{"artifact": ref})
	if refusal != nil {
		return refuse(*refusal)
	}
	res["artifact"] = h.ID.String()
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactMove, h, ref)
	if refusal != nil {
		return refuse(*refusal)
	}
	if h.Status == artifactsdomain.StatusTrashed {
		return refuse(refusedBy(tools.RefusalForbidden, artifactsdomain.ErrTrashed, fmt.Sprintf(
			"move_artifact: artifact %s is in the trash, and only a person restores it. Nothing "+
				"was moved.", h.ID)))
	}
	if h.Folder == dest {
		res["outcome"] = "unchanged"
		return res, nil
	}
	t.deps.present(c, h.ID, presence.Editing, presence.Focus{})
	written, err := t.deps.Writer.Move(ctx, t.deps.call(c, dec.Standing.Role, dec.Elevated), h.ID, dest)
	switch {
	case err != nil:
		return refuse(folderWriteFailure(ctx, t.Name(), c, err))
	case written.Result.Outcome == statelog.OutcomeUnknown:
		r := artifactUnknown(t.Name(), c, written.Result, "Read the artifact with get_artifact "+
			"or list the folder: if it is there, the move landed.")
		res["outcome"], res["message"] = "unknown", r.Output
		return res, r.Cause
	}
	t.deps.settle(ctx, written.Result.Position)
	res["outcome"] = "moved"
	if url := t.deps.link(h.ID); url != "" {
		res["url"] = url
	}
	return res, nil
}

// moveRefs reads `artifacts`: one to [artifacts.MaxMovesPerCall] references,
// each once.
func moveRefs(args map[string]any) ([]string, string) {
	raw, ok := argval.StringItems(args["artifacts"])
	if !ok || len(raw) == 0 {
		return nil, "move_artifact needs `artifacts`: a list of the artifact ids to move."
	}
	if len(raw) > artifacts.MaxMovesPerCall {
		return nil, fmt.Sprintf("move_artifact moves at most %d artifacts in one call; move "+
			"the rest in another.", artifacts.MaxMovesPerCall)
	}
	out := make([]string, 0, len(raw))
	for i, v := range raw {
		ref, ok := argval.String(v)
		ref = strings.TrimSpace(ref)
		if !ok || ref == "" {
			return nil, fmt.Sprintf("move_artifact: artifact %d of the list is not an id.", i+1)
		}
		if !slices.Contains(out, ref) {
			out = append(out, ref)
		}
	}
	return out, ""
}
