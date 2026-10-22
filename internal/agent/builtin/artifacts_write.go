package builtin

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/edits"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/words"
)

// The stale answer's budget for the live version's text: the first
// [artifacts.ToolReadPageBytes] of each changed text file, until this much
// text is inlined across them, so the answer stays inside one tool answer
// ([ToolAnswerBytes]) beside the changed paths and the retry sentence. A
// changed file past it is named, and read with get_artifact.
const staleTextBudget = 40 << 10

// ---- the shared parameters ---------------------------------------------- //

func filesParam(patch bool) map[string]any {
	desc := "Files by published path, each `{content}` (with `encoding`: `text`, the " +
		"default, or `base64`), `{from_sandbox: \"dist/app.js\"}` (a file your coding run " +
		"wrote in this turn, relative to its workspace), `{from_project_file: {project, " +
		"path}}` or `{from_brand: \"logo.primary\"}` (a logo or typeface of your company's " +
		"adopted brand, copied in; " + CompanyBrandTool + " gives each ready to paste). Inline " +
		"content is at most 1 MiB in all."
	if patch {
		desc += " Null removes a file; files you leave out are kept."
	}
	return map[string]any{"type": "object", "description": desc}
}

func sourceParam() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sandbox": map[string]any{
				"type": "string",
				"description": "A directory your coding run built into in this turn, " +
					"relative to its workspace (e.g. `dist`): its files become the whole " +
					"file set, each at its path below the directory.",
			},
		},
		"description": "Publish a build's whole output. Not combined with `content`, " +
			"`edits` or `files`.",
	}
}

func capabilitiesParam() map[string]any {
	caps := make([]any, 0, len(artifacts.Capabilities))
	for _, c := range artifacts.Capabilities {
		caps = append(caps, string(c))
	}
	return map[string]any{
		"type": "array", "items": map[string]any{"type": "string", "enum": caps},
		"description": "The runtime capabilities the page uses: `db` (the shared " +
			"datastore), `state` (per-viewer state), `assets`, `identity`, `presence`. On " +
			"a publish, omit to keep the current set; an empty list declares none.",
	}
}

// capabilitiesArg reads `capabilities`: nil when absent (keep the head's), an
// empty set when the caller sent an empty list.
func capabilitiesArg(tool string, args map[string]any) ([]artifacts.Capability, string) {
	raw, present := args["capabilities"]
	if !present || raw == nil {
		return nil, ""
	}
	list, ok := argval.StringItems(raw)
	if !ok {
		return nil, tool + " takes `capabilities` as a list of names."
	}
	out := make([]artifacts.Capability, 0, len(list))
	for _, v := range list {
		name, _ := argval.String(v)
		c := artifacts.Capability(strings.TrimSpace(name))
		if !c.Valid() {
			return nil, fmt.Sprintf("%s: %q is not a capability; the page may use %s.",
				tool, clip(fmt.Sprint(v)), words.Or(artifacts.Capabilities))
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, ""
}

// sourceArg reads `source`: absent, or an object naming the sandbox directory
// and nothing else.
func sourceArg(tool string, args map[string]any) (map[string]any, bool, string) {
	raw, present := args["source"]
	if !present || raw == nil {
		return nil, false, ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, false, tool + " takes `source` as `{sandbox: \"<directory>\"}`."
	}
	for field := range m {
		if field != "sandbox" {
			return nil, false, fmt.Sprintf("%s: `source` takes `sandbox` alone, and "+
				"carries %q.", tool, field)
		}
	}
	return m, true, ""
}

// defaultEntry is the entry a new artifact's `content` lands at: its kind's
// first default entry, and nothing for a kind that publishes no files at
// create.
func defaultEntry(spec artifacts.KindSpec) string {
	if len(spec.Entry.Defaults) == 0 {
		return ""
	}
	return spec.Entry.Defaults[0]
}

// carriedEntry is the entry a new artifact whose `files` or `source` name none
// opens on: the first of its kind's default entries the files hold, and none
// where they hold no default, which the publish then refuses by name.
func carriedEntry(spec artifacts.KindSpec, files *fileSources) string {
	for _, d := range spec.Entry.Defaults {
		if _, ok := files.find(d); ok {
			return d
		}
	}
	return ""
}

// entryDefaults is the agent tools' account of where `content` lands, kind by
// kind, from the kind table.
func entryDefaults() string {
	var parts []string
	for _, s := range artifacts.KindSpecs() {
		if d := defaultEntry(s); d != "" {
			parts = append(parts, fmt.Sprintf("`%s` for %s", d, s.Kind))
		}
	}
	return strings.Join(parts, ", ")
}

// ---- create_artifact ---------------------------------------------------- //

type createArtifact struct{ deps ArtifactDeps }

var _ tools.SeatCallable = (*createArtifact)(nil)

func (t *createArtifact) Name() string { return CreateArtifactTool }

func (t *createArtifact) Description() string {
	return "Create a NEW artifact in this Crewlet company (not claude.ai): an " +
		"interactive HTML page, a design canvas of HTML artboards, a document, a " +
		"slide deck, or a sheet, shared with the " +
		"colleagues who must use it. Load the `" + ArtifactsToolSkillKey + "` tool skill " +
		"first: it is the design contract every page is held to. Before writing a " +
		"brand kit, a deck's index, a design's canvas, a dashboard or a doc's index, read " +
		"its exact format with " + ArtifactFormatTool + ". When a person asked for it, " +
		"they are made an editor. Begin a deck with `starter` rather than writing every slide. " +
		"When your company has a brand, read " + CompanyBrandTool + " before writing slides, " +
		"logos or typefaces. To change an existing artifact use " + PublishArtifactTool + "; the " +
		"answer carries the `url` to send people."
}

func (t *createArtifact) Parameters() map[string]any {
	return t.deps.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind": map[string]any{
				"type": "string", "enum": kindEnum(),
				"description": artifacts.KindSummaries(),
			},
			"title": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Two to four words, at most %d bytes.",
					artifacts.MaxTitleBytes),
			},
			"description": map[string]any{
				"type":        "string",
				"description": "One sentence for the gallery card.",
			},
			"icon": map[string]any{
				"type":        "string",
				"description": "One generic word, e.g. chart, table, calendar.",
			},
			"entry": map[string]any{
				"type":        "string",
				"description": "The file the artifact opens on. Default " + entryDefaults() + ".",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The entry page's whole content.",
			},
			"files":  filesParam(false),
			"source": sourceParam(),
			"label": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Names this first version, at most %d bytes. "+
					"Labelled versions are kept.", artifacts.MaxLabelBytes),
			},
			"contract": map[string]any{
				"type": "string",
				"description": "The page runtime contract, an exact version or `latest` " +
					"(the default).",
			},
			"capabilities": capabilitiesParam(),
			"source_key": map[string]any{
				"type": "string",
				"description": "A stable name for what this artifact publishes, e.g. " +
					"`weekly-review`. Creating again with the same key answers the " +
					"artifact you already made rather than a second one.",
			},
			"starter": starterParam(),
			"folder": map[string]any{
				"type": "string",
				"description": "The folder to file it in: a folder's id, or its path of names " +
					"from the top level (`Marketing/Q3`). It must exist and be one you may file " +
					"in; a missing one is refused, never created. Omit for the top level.",
			},
		},
		"required": []any{"kind", "title"},
	})
}

// starterParam is create_artifact's `starter`: a deck begun from a plan the
// engine composes, in a theme and with the slides named.
func starterParam() map[string]any {
	themes := make([]any, 0, len(slideformat.Themes())+1)
	themes = append(themes, slideformat.BrandTheme)
	for _, th := range slideformat.Themes() {
		themes = append(themes, th.ID)
	}
	layouts := make([]any, 0, len(slideformat.Layouts()))
	for _, l := range slideformat.Layouts() {
		layouts = append(layouts, l.ID)
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"theme": map[string]any{
				"type": "string", "enum": themes,
				"description": "`brand` draws your company's adopted brand (its colours, faces " +
					"and logo); otherwise one of the engine's themes. Default `" +
					slideformat.DefaultTheme + "`.",
			},
			"slides": map[string]any{
				"type": "array", "maxItems": slideformat.MaxPlanSlides,
				"items": map[string]any{
					"type":     "object",
					"required": []any{"layout"},
					"properties": map[string]any{
						"layout": map[string]any{"type": "string", "enum": layouts},
						"title":  map[string]any{"type": "string"},
					},
				},
				"description": "The slides it opens with, in order, each a layout and its " +
					"title (a cover's is the deck's title).",
			},
		},
		"description": "For a `slides` deck only: begin from the engine's composer, with no " +
			"`content`, `files`, `source` or `entry`. A plan costs a few lines where writing " +
			"every slide costs thousands; fill the text in afterwards with `edits`.",
	}
}

// starterArg reads `starter`: absent, or a deck plan.
func starterArg(args map[string]any) (*slideformat.Plan, string) {
	raw, present := args["starter"]
	if !present || raw == nil {
		return nil, ""
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, CreateArtifactTool + " takes `starter` as `{theme, slides: [{layout, title}]}`."
	}
	plan := &slideformat.Plan{}
	for field, v := range m {
		switch field {
		case "theme":
			theme, ok := argval.String(v)
			if !ok || v == nil {
				return nil, CreateArtifactTool + ": `starter.theme` is a theme's id."
			}
			plan.Theme = strings.TrimSpace(theme)
		case "slides":
			list, ok := argval.List(v)
			if !ok {
				return nil, CreateArtifactTool + ": `starter.slides` is a list of `{layout, title}`."
			}
			for i, item := range list {
				slide, ok := item.(map[string]any)
				if !ok {
					return nil, fmt.Sprintf(CreateArtifactTool+": starter slide %d is not an object.", i+1)
				}
				for key := range slide {
					if key != "layout" && key != "title" {
						return nil, fmt.Sprintf(CreateArtifactTool+": starter slide %d carries %q, and "+
							"a slide takes `layout` and `title`.", i+1, key)
					}
				}
				plan.Slides = append(plan.Slides, slideformat.PlanSlide{
					Layout: strings.TrimSpace(argString(slide, "layout")),
					Title:  strings.TrimSpace(argString(slide, "title"))})
			}
		default:
			return nil, fmt.Sprintf(CreateArtifactTool+": `starter` carries %q, and takes `theme` "+
				"and `slides`.", field)
		}
	}
	return plan, ""
}

func (t *createArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *createArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	kind := artifacts.Kind(strings.TrimSpace(argString(args, "kind")))
	spec, ok := kind.Spec()
	if !ok {
		return failed(fmt.Sprintf(CreateArtifactTool+" takes `kind` as one of %s.", words.Or(artifacts.Kinds))), nil
	}
	title := strings.TrimSpace(argString(args, "title"))
	if title == "" {
		return failed(CreateArtifactTool + " needs a `title`: two to four words."), nil
	}
	caps, why := capabilitiesArg(t.Name(), args)
	if why != "" {
		return failed(why), nil
	}
	owner := iam.RecordOwner(c.principal)
	// THE CREATOR OWNS IT, so the table asks the app and the ceiling spares
	// the agent's own new work ([authz.DecideWithin]).
	dec := authz.DecideWithin(ctx, c.principal, c.ceiling, authz.ActionArtifactCreate,
		authz.Object{Kind: authz.KindArtifact, Owner: owner}, t.deps.Chart, t.deps.now())
	if dec.Unknown() || !dec.Allowed {
		return authorityRefusal(t.Name(), DecisionError(authz.ActionArtifactCreate, dec)), nil
	}
	// THE FOLDER IS DECIDED BEFORE A BYTE MOVES: a folder the call may not
	// name, or may not file in, refuses the create with nothing uploaded.
	folder, folderPath, refusal := t.folder(ctx, c, args)
	if refusal != nil {
		return *refusal, nil
	}

	files := newFileSources(t.deps, c, t.Name())
	_, hasContent := args["content"]
	source, hasSource, why := sourceArg(t.Name(), args)
	if why != "" {
		return failed(why), nil
	}
	_, hasFiles := args["files"]
	entry := strings.TrimSpace(argString(args, "entry"))
	plan, why := starterArg(args)
	if why != "" {
		return failed(why), nil
	}
	switch {
	case plan != nil && spec.Format != artifacts.FormatSlides:
		return failed(fmt.Sprintf(CreateArtifactTool+": `starter` begins a slide deck, and a %s "+
			"begins from its `content` or `files`.", kind)), nil
	case plan != nil && (hasContent || hasFiles || hasSource || entry != ""):
		return failed(CreateArtifactTool + " takes `starter` alone: the engine composes the deck's " +
			"files and its entry, so `content`, `files`, `source` and `entry` are not " +
			"combined with it. Change the slides with " + PublishArtifactTool + " `edits` afterwards."), nil
	case hasSource && (hasContent || hasFiles):
		return failed(CreateArtifactTool + " takes `source` alone: a build's output is the " +
			"whole file set, so `content` and `files` are not combined with it."), nil
	case spec.Store == artifacts.StoreContent && (hasContent || hasFiles || hasSource):
		return failed(fmt.Sprintf(CreateArtifactTool+": a %s's content is its datastore, so it "+
			"publishes no files. Create it with a title and declare its collections "+
			"with "+SetArtifactSchemaTool+".", kind)), nil
	}
	if hasContent {
		p := entry
		if p == "" {
			p = defaultEntry(spec)
		}
		clean, refusal := files.cleanPath(p) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if refusal != nil {
			return *refusal, nil
		}
		entry = clean
		if refusal := files.addInline(clean, argString(args, "content"), ""); refusal != nil {
			return *refusal, nil
		}
	}
	if refusal := files.parseFiles(ctx, args, false); refusal != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return *refusal, nil
	}
	if hasSource {
		if refusal := files.addBoxDir(ctx, argString(source, "sandbox")); refusal != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return *refusal, nil
		}
	}
	if spec.Store != artifacts.StoreContent && len(files.files) == 0 && plan == nil {
		return failed(CreateArtifactTool + " needs the page: `content` for the entry page, " +
			"`files`, a build's `source`, or a deck's `starter`."), nil
	}
	if entry == "" {
		entry = carriedEntry(spec, files)
	}

	call := t.deps.call(c, artifacts.RoleOwner, false)
	upload, specs, refusal := files.stage(ctx, call, artifacts.ID{}, false)
	if refusal != nil {
		return *refusal, nil
	}
	shares := t.requesterShare(c)
	published, report, err := t.deps.Publisher.Create(ctx, call, artifactupload.CreateRequest{
		Kind: kind, Title: title, Description: strings.TrimSpace(argString(args, "description")),
		Icon: strings.TrimSpace(argString(args, "icon")), Owner: owner, OwnerKind: c.actor.Kind,
		SourceKey: strings.TrimSpace(argString(args, "source_key")), Shares: shares,
		Upload: upload, Files: specs, Entry: entry,
		Label:    strings.TrimSpace(argString(args, "label")),
		Contract: strings.TrimSpace(argString(args, "contract")), Capabilities: caps,
		Starter: plan != nil, Deck: plan, Folder: folder,
	})
	if err != nil {
		return publishFailure(ctx, t.Name(), c, err), nil
	}
	if published.Result.Outcome == statelog.OutcomeUnknown {
		return artifactUnknown(t.Name(), c, published.Result, "Read your artifacts with "+
			ListArtifactsTool+" before creating it a different way: if it landed, it is there."), nil
	}
	t.deps.settle(ctx, published.Result.Position)
	out := t.deps.publishedAnswer(c, published, "created")
	if folderPath != "" {
		out["folder"] = folderPath
	}
	if len(shares) > 0 {
		out["shared_with"] = []map[string]any{{"to": shareTarget(shares[0].Audience),
			"role": string(shares[0].Role)}}
	}
	return caveated(out, findingCaveats(report))
}

// folder is the folder a create files into, by id, with its path ("" for the
// top level), under both of filing's decisions, or the refusal.
func (t *createArtifact) folder(ctx context.Context, c artifactCaller, args map[string]any) (
	string, string, *tools.Result) {

	ref := strings.TrimSpace(argString(args, "folder"))
	if ref == "" {
		return "", "", nil
	}
	view, refusal := t.deps.folderView(ctx, c, t.Name())
	if refusal != nil {
		return "", "", refusal
	}
	f, p, _, refusal := t.deps.resolveFolder(ctx, c, t.Name(), ref, view)
	if refusal != nil {
		return "", "", refusal
	}
	if _, refusal := t.deps.decideFiling(ctx, c, t.Name(), f, p); refusal != nil {
		return "", "", refusal
	}
	return f.ID, p, nil
}

// requesterShare is the share an agent's new artifact is created with: the
// person whose request the turn is serving, as an editor, so the story "an
// agent hands a teammate a review table" needs no second gesture. Only a
// person, and only on an agent's own create.
func (t *createArtifact) requesterShare(c artifactCaller) []artifacts.Share {
	if !c.agent() || c.ceiling.Kind != authz.CeilingRequester || c.turn == nil {
		return nil
	}
	handle := strings.TrimSpace(c.turn.Requester)
	o := t.deps.orgFor(c.turn)
	if handle == "" || o == nil {
		return nil
	}
	seat := o.SeatByHandle(handle)
	if seat == nil || !seat.IsHuman() {
		return nil
	}
	return []artifacts.Share{{
		Audience: artifacts.Audience{Kind: artifacts.AudienceSeat, ID: handle},
		Role:     artifacts.RoleEditor,
	}}
}

// publishedAnswer is a version that landed, as create and publish answer it.
func (d ArtifactDeps) publishedAnswer(c artifactCaller, p artifactsdomain.Published,
	outcome string) map[string]any {

	out := map[string]any{
		"outcome": outcome, "artifact": p.Artifact.String(), "version": p.Number,
		"write": outcomeOf(p.Result), "position": positionOf(p.Result.Position),
	}
	if url := d.link(p.Artifact); url != "" {
		out["url"] = url
	}
	return withArtifactOperation(out, c)
}

// findingCaveats is the linter's warnings about a published version, as the
// caveats its answer carries ([caveated]): each one about a file the caller
// wrote, in the same words for a model and a person who wrote it.
func findingCaveats(report artifactupload.Report) []caveat.Caveat {
	if len(report.Warnings) == 0 {
		return nil
	}
	out := make([]caveat.Caveat, len(report.Warnings))
	for i, f := range report.Warnings {
		where := f.Path
		if f.Line > 0 {
			where = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		out[i] = caveat.FileFinding(where, f.Message)
	}
	return out
}

// withArtifactOperation puts the call's `op_id` on its answer on the operator
// surface, where a caller brings it back to repeat the call as the same
// operation.
func withArtifactOperation(out map[string]any, c artifactCaller) map[string]any {
	if c.opID != "" {
		out[opIDArg] = c.opID
	}
	return out
}

// artifactUnknown explains a write whose outcome is unknown: it may have
// landed and it may not, so it is reported as neither, with how this caller
// makes the same write again.
func artifactUnknown(tool string, c artifactCaller, r statelog.Result, look string) tools.Result {
	why := "the write's acknowledgement was lost"
	if r.Unvouched {
		why = "this node's operation ledger may have lost the record of it, so this " +
			"node cannot tell"
	}
	again := "Make the same call again with exactly the same arguments: it is the same " +
		"operation and lands once."
	switch {
	case c.opID != "":
		again = fmt.Sprintf("Send the same call again with `op_id` %q and exactly the "+
			"same arguments: it is the same operation and lands once.", c.opID)
	case c.turn == nil && c.key != "":
		again = "Send the same request again, unchanged."
	case c.key == "":
		again = look
	}
	return failedUnknown(r.OpID, r.Unvouched, fmt.Sprintf("%s: whether this landed is "+
		"unknown (%s; operation %s). Do not report it as done, and do not report it as "+
		"failed. %s", tool, why, r.OpID, again))
}

// publishFailure explains a create, a publish or an asset the pipeline or the
// domain refused, classed for a reader that is not a model.
func publishFailure(ctx context.Context, tool string, c artifactCaller, err error) tools.Result {
	var (
		turn     *artifactupload.TurnQuota
		policy   *lint.Refusal
		edit     *edits.Refused
		content  *artifactupload.ContentRefused
		missing  *artifactupload.UploadMissing
		mismatch *artifactupload.UploadMismatch
		pending  *artifactupload.UploadsOutstanding
		quota    *artifactsdomain.QuotaExceeded
		gone     *artifactsdomain.CarriedObjectGone
	)
	said := artifactupload.Sentence(err)
	switch {
	case errors.Is(err, estate.ErrOutcomeUnknown):
		return tools.Result{Output: fmt.Sprintf("%s: %s. It may have landed and it may "+
			"not: do not report it as done, and do not report it as failed. Read the "+
			"artifact with "+GetArtifactTool+" before publishing again.", tool, Unanswered),
			Failed: true, Cause: UnknownCause(c.opID, err)}
	case errors.As(err, &turn):
		// THE TURN HAS DONE WHAT IT SHOULD: a thrashing loop of publishes is
		// what the guardrail is for, and the sentence tells the model to
		// stop and report rather than look for another way round it.
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that "+
			"(turn_quota): this turn has made %d of its %d %s. Stop publishing, report "+
			"what was done and what is left, and let the next turn continue.", tool,
			turn.Used, turn.Limit, strings.ReplaceAll(turn.What, "_", " ")))
	case errors.As(err, &policy):
		return toPerson(refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused "+
			"the version: it breaks the artifact policy, so nothing was published. Fix "+
			"every finding and publish again:\n%s", tool, policyFindings(policy.Findings))),
			caveat.ArtifactBreaksPolicy())
	case errors.As(err, &edit):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused edit %d on "+
			"%q (%s), so nothing was published. An `old_str` must be quoted exactly from "+
			"the version you based on and occur in it exactly once: read the file's "+
			"source with "+GetArtifactTool+" and `offset`, then quote more of it.", tool,
			edit.Index, edit.Path, edit.Reason))
	case errors.As(err, &content), errors.As(err, &mismatch),
		errors.Is(err, artifactupload.ErrMediaType), errors.Is(err, artifactupload.ErrCredential),
		errors.Is(err, artifactupload.ErrCapabilityUnknown),
		errors.Is(err, artifactupload.ErrContractUnknown),
		errors.Is(err, artifactupload.ErrInvalid), errors.Is(err, artifactsdomain.ErrInvalid):
		return refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s refused that: %s",
			tool, said))
	case errors.Is(err, artifactsdomain.ErrLabelTaken):
		// A COLLISION: another version holds the label, which another
		// label avoids now and which lands once that version gives it up.
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s refused that: %s. "+
			"Read the artifact's versions with "+GetArtifactTool+" and choose another label, "+
			"or publish without one.", tool, said))
	case errors.Is(err, artifactsdomain.ErrLabelsFull):
		// THE ARTIFACT'S STATE: every label it keeps is in use, so no other
		// label would land either until one is removed.
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s refused that: %s. "+
			"A label is removed with "+UpdateArtifactTool+", naming its `version` with an "+
			"empty `label`; or publish without one.", tool, said))
	case errors.As(err, &quota):
		return toPerson(refusedBy(tools.RefusalQuotaExceeded, err, fmt.Sprintf("%s refused "+
			"that: the company's artifact quota is reached (%d in use of %d, `%s`). "+
			"Nothing was published. Say so: an operator raises the quota, or somebody "+
			"removes artifacts the company no longer needs.", tool, quota.Usage,
			quota.Limit, quota.Key)), caveat.ArtifactQuotaReached())
	case errors.As(err, &pending):
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s refused that: %s",
			tool, said))
	case errors.As(err, &missing), errors.As(err, &gone),
		errors.Is(err, artifactsdomain.ErrUploadStale),
		errors.Is(err, artifactupload.ErrUploadExpired),
		errors.Is(err, artifactupload.ErrUploadConsumed),
		errors.Is(err, artifactupload.ErrUploadNotFound):
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s did not land: %s. "+
			"Nothing was published; make the call again.", tool, said))
	case errors.Is(err, artifactsdomain.ErrSourceTaken):
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s: %s", tool, said))
	case errors.Is(err, artifactsdomain.ErrExists):
		// WHAT AN EARLIER ATTEMPT OF THIS CREATE MADE, most often: read it
		// rather than make a second one.
		return refusedBy(tools.RefusalExists, err, fmt.Sprintf("%s: %s. Read it with "+
			GetArtifactTool+" or "+ListArtifactsTool+" rather than making it again.", tool, said))
	case errors.Is(err, artifactsdomain.ErrAgentCompanyShare),
		errors.Is(err, artifactsdomain.ErrAboveOwnRole):
		return refusedBy(tools.RefusalForbidden, err, fmt.Sprintf("%s refused that: %s",
			tool, said))
	case errors.Is(err, artifactsdomain.ErrTrashed):
		return toPerson(refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s: the "+
			"artifact is in the trash, and only a person restores it. Nothing was "+
			"changed.", tool)), caveat.ArtifactTrashed())
	case errors.Is(err, artifactsdomain.ErrNotFound), errors.Is(err, artifactsdomain.ErrPurged):
		return refusedBy(tools.RefusalNotFound, err, fmt.Sprintf("%s: %s", tool, said))
	case errors.Is(err, artifactsdomain.ErrConflict):
		return refusedBy(tools.RefusalConflict, err, fmt.Sprintf("%s could not land: "+
			"somebody else kept changing the artifact. Read it again with "+GetArtifactTool+" "+
			"before retrying.", tool))
	}
	if class, ok := WriterClass(err); ok {
		// ANY OTHER REFUSAL A DOMAIN DECIDED, in its own class — see
		// [writeFailure]'s same arm: a write the log refused because the
		// artifact's purge had begun, which no arm above names.
		return refusedBy(class, err, domainRefusal(tool, err))
	}
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s did not land "+
			"(%s). Nothing was changed: do not report it as done. %s", tool, why,
			conditionNext(err, "Try again shortly.", stuckWrite)))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s failed: %s. Do not report it as done, "+
		"and do not make it again by other means: say it could not be made.", tool, faultSaid))
}

// ---- publish_artifact --------------------------------------------------- //

type publishArtifact struct {
	artifactTarget
	deps ArtifactDeps
}

var _ tools.SeatCallable = (*publishArtifact)(nil)

func (t *publishArtifact) Name() string { return PublishArtifactTool }

func (t *publishArtifact) Description() string {
	return "Publish a new version of an artifact in this Crewlet company. You MUST " +
		"pass the `base_version` you read with " + GetArtifactTool + ": a publish over a version " +
		"somebody else made since is refused and hands you that version to merge onto. " +
		"Prefer `edits` (exact old_str and new_str pairs) for a change to an existing " +
		"page; `content` replaces the entry page whole; `files` adds, replaces or " +
		"removes files; `source` publishes a build's whole output from your coding run; " +
		"`restore_version` rolls back. When your company has a brand, read " +
		CompanyBrandTool + " before writing slides, logos or typefaces. Publishing reaches " +
		"nobody by itself: send the `url` to whoever is waiting."
}

func (t *publishArtifact) Parameters() map[string]any {
	return t.deps.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"base_version": map[string]any{
				"type": "integer",
				"description": "REQUIRED: the `head_version` you read. A publish over a " +
					"newer version is refused and hands you that version to merge onto.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Replaces the entry page whole. Prefer `edits`.",
			},
			"edits": map[string]any{
				"type": "array", "maxItems": artifacts.MaxEditsPerPublish,
				"items": map[string]any{
					"type":     "object",
					"required": []any{"old_str", "new_str"},
					"properties": map[string]any{
						"path": map[string]any{"type": "string",
							"description": "The file; default the entry page."},
						"old_str": map[string]any{"type": "string",
							"description": "Copied exactly from the file's source at " +
								"base_version; must occur in it exactly once."},
						"new_str": map[string]any{"type": "string"},
					},
				},
				"description": "Applied in order, all or none, to the files at base_version.",
			},
			"files":  filesParam(true),
			"source": sourceParam(),
			"entry": map[string]any{
				"type":        "string",
				"description": "Moves the page served at the artifact's root.",
			},
			"label": map[string]any{
				"type": "string",
				"description": fmt.Sprintf("Names this version, at most %d bytes. "+
					"Labelled versions are kept.", artifacts.MaxLabelBytes),
			},
			"contract": map[string]any{
				"type": "string",
				"description": "The page runtime contract, an exact version or `latest`. " +
					"Omit to keep the current one.",
			},
			"capabilities": capabilitiesParam(),
			"restore_version": map[string]any{
				"type": "integer",
				"description": "Roll back: publish this retained version's files as the " +
					"new version. Takes nothing but `label`, `contract` and `capabilities` " +
					"beside it.",
			},
		},
		"required": []any{"artifact", "base_version"},
	})
}

func (t *publishArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *publishArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	base := argInt(args, "base_version", 0)
	if base < 1 {
		return failed(PublishArtifactTool + " needs a `base_version`: read the artifact with " +
			GetArtifactTool + " and pass its `head_version` back, so a version somebody else " +
			"published in the meantime is a refusal rather than a silent overwrite."), nil
	}
	caps, why := capabilitiesArg(t.Name(), args)
	if why != "" {
		return failed(why), nil
	}
	restore := argInt(args, "restore_version", 0)
	_, hasContent := args["content"]
	_, hasEdits := args["edits"]
	_, hasFiles := args["files"]
	source, hasSource, why := sourceArg(t.Name(), args)
	if why != "" {
		return failed(why), nil
	}
	entry := strings.TrimSpace(argString(args, "entry"))
	label := strings.TrimSpace(argString(args, "label"))
	contract := strings.TrimSpace(argString(args, "contract"))
	switch {
	case restore < 0:
		return failed(PublishArtifactTool + " takes a `restore_version` of 1 or more."), nil
	case restore > 0 && (hasContent || hasEdits || hasFiles || hasSource || entry != ""):
		return failed(PublishArtifactTool + ": a rollback names the version it restores and " +
			"nothing else beside `label`, `contract` and `capabilities`."), nil
	case hasSource && (hasContent || hasEdits || hasFiles):
		return failed(PublishArtifactTool + " takes `source` alone: a build's output is the " +
			"whole file set, so `content`, `edits` and `files` are not combined with it."), nil
	case restore == 0 && !hasContent && !hasEdits && !hasFiles && !hasSource && entry == "" &&
		label == "" && contract == "" && caps == nil:
		return failed(PublishArtifactTool + " has nothing to publish: send `edits`, `content`, " +
			"`files`, `source`, `restore_version`, or a change of `entry`, `label`, " +
			"`contract` or `capabilities`."), nil
	}
	list, why := editsArg(args)
	if why != "" {
		return failed(why), nil
	}

	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactPublish, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	t.deps.present(c, h.ID, presence.Editing, presence.Focus{Version: h.HeadNumber})

	files := newFileSources(t.deps, c, t.Name())
	mode := manifest.ModePatch
	if hasContent {
		p := entry
		if p == "" {
			if p, refusal = t.baseEntry(ctx, h, base, ref); refusal != nil {
				return *refusal, nil
			}
		}
		clean, refusal := files.cleanPath(p) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		if refusal != nil {
			return *refusal, nil
		}
		if refusal := files.addInline(clean, argString(args, "content"), ""); refusal != nil {
			return *refusal, nil
		}
	}
	if refusal := files.parseFiles(ctx, args, true); refusal != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return *refusal, nil
	}
	if hasSource {
		mode = manifest.ModeWhole
		if refusal := files.addBoxDir(ctx, argString(source, "sandbox")); refusal != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
			return *refusal, nil
		}
	}

	call := t.deps.call(c, dec.Standing.Role, dec.Elevated)
	upload, specs, refusal := files.stage(ctx, call, h.ID, false)
	if refusal != nil {
		return *refusal, nil
	}
	published, report, err := t.deps.Publisher.Publish(ctx, call, artifactupload.PublishRequest{
		Artifact: h.ID, Base: base, RestoreFrom: restore, Upload: upload, Mode: mode,
		Files: specs, Edits: list, Entry: entry, Label: label, Contract: contract,
		Capabilities: caps,
	})
	var moved *artifactsdomain.VersionMoved
	switch {
	case errors.As(err, &moved):
		return t.stale(ctx, turn, c, moved, base, err), nil
	case err != nil:
		return publishFailure(ctx, t.Name(), c, err), nil
	case published.Result.Outcome == statelog.OutcomeUnknown:
		return artifactUnknown(t.Name(), c, published.Result, "Read the artifact with "+
			GetArtifactTool+": if its head moved past your base, the publish landed."), nil
	}
	t.deps.settle(ctx, published.Result.Position)
	out := t.deps.publishedAnswer(c, published, "published")
	out["base_version"] = base
	return caveated(out, findingCaveats(report))
}

// baseEntry is the entry page of the version a publish is based on, which a
// `content` with no `entry` replaces.
func (t *publishArtifact) baseEntry(ctx context.Context, h artifactsdomain.Head, base int,
	ref string) (string, *tools.Result) {

	if base > h.HeadNumber {
		r := failed(fmt.Sprintf(PublishArtifactTool+": artifact %q is at version %d, and "+
			"`base_version` %d is past it.", clip(ref), h.HeadNumber, base))
		return "", &r
	}
	v, err := t.deps.Reader.Version(ctx, h.ID, base)
	if err != nil {
		if errors.Is(err, artifactsdomain.ErrNotFound) {
			r := failed(fmt.Sprintf(PublishArtifactTool+": artifact %q no longer keeps "+
				"version %d. Read the head with "+GetArtifactTool+" and publish based on it.",
				clip(ref), base))
			return "", &r
		}
		r := artifactReadFailure(ctx, t.Name(), ref, err)
		return "", &r
	}
	if v.Manifest.Entry == "" {
		r := failed(PublishArtifactTool + ": the version has no entry page for `content` to " +
			"replace; name the file with `entry`.")
		return "", &r
	}
	return v.Manifest.Entry, nil
}

// editsArg reads `edits`.
func editsArg(args map[string]any) ([]edits.Edit, string) {
	raw, present := args["edits"]
	if !present || raw == nil {
		return nil, ""
	}
	list, ok := argval.List(raw)
	if !ok {
		return nil, PublishArtifactTool + " takes `edits` as a list of `{path, old_str, new_str}`."
	}
	if len(list) > artifacts.MaxEditsPerPublish {
		return nil, fmt.Sprintf(PublishArtifactTool+" takes at most %d `edits`; publish the "+
			"rest in another version, or replace the page with `content`.",
			artifacts.MaxEditsPerPublish)
	}
	out := make([]edits.Edit, 0, len(list))
	for i, v := range list {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Sprintf(PublishArtifactTool+": edit %d is not an object.", i)
		}
		old, hasOld := argval.String(m["old_str"])
		repl, hasNew := argval.String(m["new_str"])
		if !hasOld || !hasNew || m["old_str"] == nil || m["new_str"] == nil {
			return nil, fmt.Sprintf(PublishArtifactTool+": edit %d needs `old_str` and "+
				"`new_str`, both strings.", i)
		}
		out = append(out, edits.Edit{Path: strings.TrimSpace(argString(m, "path")),
			Old: old, New: repl})
	}
	return out, ""
}

// stale is a publish based on a version somebody has moved past: a failed
// result of class stale_version carrying the live version, who published it
// and when, the paths that changed since the caller's base, and the first page
// of each changed text file, fenced as text the live version's author wrote,
// so the model merges and publishes again on the live number.
//
// THE TEXT IS A READ OF THE LIVE VERSION, shown only where reading it would be
// ([ArtifactDeps.discloses]): a publish to the agent's own artifact is
// admitted without the requester, and an administrator's on the admin path is
// recorded first. Where it may not be shown, the answer carries the numbers,
// the author and the paths alone.
func (t *publishArtifact) stale(ctx context.Context, turn *turnctx.Turn, c artifactCaller,
	moved *artifactsdomain.VersionMoved, base int, cause error) tools.Result {

	live := moved.Live
	read := newForeignRead(turn, c.self())
	author := read.writer(live.Author.Name, live.Author.Kind)
	changed, baseKept := t.changedPaths(ctx, moved.Head.ID, base, live.Manifest)
	out := map[string]any{
		"error": "stale_version", "artifact": moved.Head.ID.String(),
		"live_version": moved.Head.HeadNumber, "your_base": base,
		"published_by":  actorJSON(live.Author),
		"published_at":  live.CreatedAt.UTC().Format(time.RFC3339),
		"changed_paths": changed,
		"retry": fmt.Sprintf("Merge your change onto version %d and publish again with "+
			"base_version %d. Nothing of yours was published.", moved.Head.HeadNumber,
			moved.Head.HeadNumber),
	}
	if !baseKept {
		out["base_retained"] = false
	}
	var inlined []map[string]any
	var omitted []string
	budget := staleTextBudget
	inline := changed
	if !t.deps.discloses(ctx, c, moved.Head, moved.Head.HeadNumber) {
		inline = nil
		out["live_files_withheld"] = "The live version's text is not shown in this answer: " +
			"this turn may not read the artifact's content. Merge from what you published " +
			"and what `changed_paths` names."
	}
	for _, p := range inline {
		e, ok := manifest.EntryAt(live.Manifest, p)
		if !ok {
			continue
		}
		typ, err := mediatype.Resolve(e.Path, e.ContentType)
		if err != nil || !typ.Text {
			continue
		}
		if budget <= 0 {
			omitted = append(omitted, p)
			continue
		}
		limit := min(artifacts.ToolReadPageBytes, int64(budget))
		text, complete, err := t.deps.Publisher.SourceText(ctx, e, limit)
		if err != nil {
			omitted = append(omitted, p)
			continue
		}
		budget -= len(text)
		i := len(inlined)
		read.fence(fieldPath{}.key("live_files").index(i).key("content"),
			provenance.SourceArtifactFile, []provenance.Author{author}, true, string(text))
		f := map[string]any{"path": p, "size": e.Object.Size, "content": string(text)}
		if !complete {
			f["next_offset"] = len(text)
		}
		inlined = append(inlined, f)
	}
	if len(inlined) > 0 {
		out["live_files"] = inlined
	}
	if len(omitted) > 0 {
		out["not_inlined"] = omitted
	}
	if url := t.deps.link(moved.Head.ID); url != "" {
		out["url"] = url
	}
	rendered, err := read.answer(ctx, t.Name(), out, "Read the changed files one at a "+
		"time with "+GetArtifactTool+".")
	if err != nil || rendered.Failed {
		return refusedBy(tools.RefusalStaleVersion, cause, fmt.Sprintf(PublishArtifactTool+" "+
			"refused: the artifact is at version %d and you based on %d. Read it with "+
			GetArtifactTool+", merge your change onto it, and publish again with base_version %d.",
			moved.Head.HeadNumber, base, moved.Head.HeadNumber))
	}
	return tools.Result{Output: rendered.Output, Failed: true,
		Cause: tools.Classify(tools.RefusalStaleVersion, cause)}
}

// changedPaths is every path whose bytes differ between the caller's base and
// the live version, the live entry first, and whether the base is still
// retained: a base the artifact no longer keeps has every live path changed.
func (t *publishArtifact) changedPaths(ctx context.Context, id artifacts.ID, base int,
	live artifacts.Manifest) ([]string, bool) {

	held := map[string]string{}
	kept := true
	if v, err := t.deps.Reader.Version(ctx, id, base); err == nil {
		for _, e := range v.Manifest.Files {
			held[e.Path] = string(e.Object.Hash)
		}
	} else {
		kept = false
	}
	var changed []string
	seen := map[string]bool{}
	for _, e := range live.Files {
		seen[e.Path] = true
		if !kept || held[e.Path] != string(e.Object.Hash) {
			changed = append(changed, e.Path)
		}
	}
	for p := range held {
		if !seen[p] {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	if i := slices.Index(changed, live.Entry); i > 0 {
		changed = append([]string{live.Entry}, slices.Delete(changed, i, i+1)...)
	}
	return changed, kept
}

// ---- update_artifact ---------------------------------------------------- //

type updateArtifact struct {
	artifactTarget
	deps ArtifactDeps
}

var _ tools.SeatCallable = (*updateArtifact)(nil)

func (t *updateArtifact) Name() string { return UpdateArtifactTool }

func (t *updateArtifact) Description() string {
	return "Rename an artifact in this Crewlet company, or change its description, its " +
		"icon or one version's label. Its content changes with " + PublishArtifactTool + "."
}

func (t *updateArtifact) Parameters() map[string]any {
	return t.deps.writeParams(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact":    artifactParam(),
			"title":       map[string]any{"type": "string", "description": "Two to four words."},
			"description": map[string]any{"type": "string", "description": "One sentence."},
			"icon":        map[string]any{"type": "string", "description": "One generic word."},
			"label": map[string]any{
				"type":        "string",
				"description": "A label for `version` (default the head); empty removes it.",
			},
			"version": map[string]any{
				"type":        "integer",
				"description": "The version `label` names. Default the head.",
			},
		},
		"required": []any{"artifact"},
	})
}

func (t *updateArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *updateArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	var p artifactsdomain.Patch
	text := func(field string) *string {
		if _, ok := args[field]; !ok {
			return nil
		}
		v := strings.TrimSpace(argString(args, field))
		return &v
	}
	p.Title, p.Description, p.Icon = text("title"), text("description"), text("icon")
	if _, versioned := args["version"]; versioned {
		if _, labelled := args["label"]; !labelled {
			return failed(UpdateArtifactTool + ": `version` names the version a `label` is for; " +
				"send the `label` too."), nil
		}
	}
	if p.Title != nil && *p.Title == "" {
		return failed(UpdateArtifactTool + ": an artifact keeps a title; send two to four words."), nil
	}
	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	if label := text("label"); label != nil {
		p.Label = &artifactsdomain.VersionLabel{Number: argInt(args, "version", h.HeadNumber),
			Label: *label}
	}
	if p.Title == nil && p.Description == nil && p.Icon == nil && p.Label == nil {
		return failed(UpdateArtifactTool + " has nothing to change: send `title`, " +
			"`description`, `icon` or `label`."), nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactUpdate, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	t.deps.present(c, h.ID, presence.Editing, presence.Focus{})
	written, err := t.deps.Writer.Patch(ctx, t.deps.call(c, dec.Standing.Role, dec.Elevated), h.ID, p)
	if err != nil {
		return publishFailure(ctx, t.Name(), c, err), nil
	}
	if written.Result.Outcome == statelog.OutcomeUnknown {
		return artifactUnknown(t.Name(), c, written.Result, "Read the artifact with "+
			GetArtifactTool+": it says whether the change landed."), nil
	}
	t.deps.settle(ctx, written.Result.Position)
	out := map[string]any{"artifact": h.ID.String(), "outcome": outcomeOf(written.Result),
		"position": positionOf(written.Result.Position)}
	if url := t.deps.link(h.ID); url != "" {
		out["url"] = url
	}
	return jsonResult(withArtifactOperation(out, c))
}
