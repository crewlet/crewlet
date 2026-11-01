package builtin

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/presence"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// SHARING IS THE OWNER'S, BOUNDED TO THE COMPANY.
//
// A share names a seat (a person or an agent), a unit (everybody in it and
// below it, live) or the whole company, and grants one role up to editor: an
// artifact's owner changes only by a person's transfer. A share never grants
// above the sharer's own role. And an agent never widens a share to the whole
// company (D10): a model persuaded by a comment or a row would otherwise
// broadcast a page to every colleague, so an agent shares with seats and units
// and a person widens to the company. The tool refuses it before anything is
// written, and the domain's writer refuses it again whatever surface asks.

// ---- share_artifact ----------------------------------------------------- //

type shareArtifact struct {
	artifactTarget
	deps ArtifactDeps
}

var _ tools.SeatCallable = (*shareArtifact)(nil)

func (t *shareArtifact) Name() string { return ShareArtifactTool }

func (t *shareArtifact) Description() string {
	return "Share an artifact in this Crewlet company with a colleague (their seat " +
		"handle), a team (`unit:<key>`) or, for a person only, the whole company " +
		"(`company`), at one role: viewer, commenter, contributor or editor. Sharing is " +
		"the owner's. Sharing again with a new role changes the role."
}

func (t *shareArtifact) Parameters() map[string]any {
	roles := make([]any, 0, len(artifacts.ShareableRoles))
	for _, r := range artifacts.ShareableRoles {
		roles = append(roles, string(r))
	}
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"to": map[string]any{
				"type": "string",
				"description": "A seat handle (e.g. `ana`), `unit:<key>` for a team and " +
					"everybody below it, or `company`.",
			},
			"role": map[string]any{
				"type": "string", "enum": roles,
				"description": "viewer reads; commenter remarks; contributor also adds " +
					"to the data collections that take contributions; editor publishes.",
			},
		},
		"required": []any{"artifact", "to", "role"},
	})
}

func (t *shareArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *shareArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	audience, refusal := t.deps.audienceArg(t.deps.orgFor(turn), t.Name(), argString(args, "to"))
	if refusal != nil {
		return *refusal, nil
	}
	role := artifacts.Role(strings.TrimSpace(argString(args, "role")))
	if !slices.Contains(artifacts.ShareableRoles, role) {
		return failed(fmt.Sprintf(ShareArtifactTool+" takes `role` as one of %s: ownership "+
			"moves only by a person's transfer.", words.Or(artifacts.ShareableRoles))), nil
	}
	if audience.Kind == artifacts.AudienceCompany && c.agent() {
		return refusedBy(tools.RefusalForbidden, artifactsdomain.ErrAgentCompanyShare,
			ShareArtifactTool+" refused that: an agent never shares an artifact with the "+
				"whole company. Share it with the seats or units that need it, or ask a "+
				"person to widen it."), nil
	}
	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactShare, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	if !dec.Standing.Role.Includes(role) {
		return refusedBy(tools.RefusalForbidden, artifactsdomain.ErrAboveOwnRole, fmt.Sprintf(
			ShareArtifactTool+" refused that: a share never grants above your own role, and "+
				"you hold %s.", roleOrNone(dec.Standing.Role))), nil
	}
	written, err := t.deps.Writer.Share(ctx, t.deps.call(c, dec.Standing.Role, dec.Elevated),
		h.ID, artifacts.Share{Audience: audience, Role: role})
	return t.deps.shareAnswer(ctx, t.Name(), c, h, written, err, map[string]any{
		"shared_with": shareTarget(audience), "role": string(role)})
}

// ---- unshare_artifact --------------------------------------------------- //

type unshareArtifact struct {
	artifactTarget
	deps ArtifactDeps
}

var _ tools.SeatCallable = (*unshareArtifact)(nil)

func (t *unshareArtifact) Name() string { return UnshareArtifactTool }

func (t *unshareArtifact) Description() string {
	return "Withdraw a share of an artifact in this Crewlet company: from a seat, a " +
		"team (`unit:<key>`) or the whole company (`company`). The audience loses access " +
		"on its next request. Withdrawing is the owner's."
}

func (t *unshareArtifact) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"from": map[string]any{
				"type":        "string",
				"description": "The audience a share names: a seat handle, `unit:<key>` or `company`.",
			},
		},
		"required": []any{"artifact", "from"},
	})
}

func (t *unshareArtifact) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *unshareArtifact) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	// A WITHDRAWAL NAMES WHATEVER A STORED SHARE NAMES, which may outlive
	// the seat or the unit it was made to, so the chart is not asked.
	audience, refusal := t.deps.audienceArg(nil, t.Name(), argString(args, "from"))
	if refusal != nil {
		return *refusal, nil
	}
	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactUnshare, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	written, err := t.deps.Writer.Unshare(ctx, t.deps.call(c, dec.Standing.Role, dec.Elevated),
		h.ID, audience)
	return t.deps.shareAnswer(ctx, t.Name(), c, h, written, err, map[string]any{
		"withdrawn_from": shareTarget(audience)})
}

// audienceArg reads an audience as a tool names it: a bare seat handle,
// `seat:<handle>`, `unit:<key>` or `company`. With a chart (a share being
// GRANTED) the seat or unit must be one the company holds.
func (d ArtifactDeps) audienceArg(o *org.Organization, tool, raw string) (artifacts.Audience, *tools.Result) {
	s := strings.TrimSpace(raw)
	if s == "" {
		r := failed(tool + " needs an audience: a seat handle, `unit:<key>` or `company`.")
		return artifacts.Audience{}, &r
	}
	if !strings.Contains(s, ":") && s != string(artifacts.AudienceCompany) {
		s = string(artifacts.AudienceSeat) + ":" + s
	}
	a, err := artifacts.ParseAudience(s)
	if err != nil {
		r := refusedBy(tools.RefusalInvalid, err, fmt.Sprintf("%s: %q is not an audience: "+
			"name a seat handle, `unit:<key>` or `company`.", tool, clip(raw)))
		return artifacts.Audience{}, &r
	}
	if o == nil {
		return a, nil
	}
	switch a.Kind {
	case artifacts.AudienceSeat:
		if o.SeatByHandle(a.ID) == nil {
			r := failed(fmt.Sprintf("%s: the company has no seat %q. Find the colleague "+
				"with "+LookupColleagueTool+" and share with their handle.", tool, a.ID))
			return artifacts.Audience{}, &r
		}
	case artifacts.AudienceUnit:
		if o.Unit(a.ID) == nil {
			r := failed(fmt.Sprintf("%s: the company has no unit %q.", tool, a.ID))
			return artifacts.Audience{}, &r
		}
	}
	return a, nil
}

// shareAnswer is a share's or a withdrawal's answer.
func (d ArtifactDeps) shareAnswer(ctx context.Context, tool string, c artifactCaller,
	h artifactsdomain.Head, written artifactsdomain.Written, err error,
	said map[string]any) (tools.Result, error) {

	if err != nil {
		return publishFailure(ctx, tool, c, err), nil
	}
	if written.Result.Outcome == statelog.OutcomeUnknown {
		return artifactUnknown(tool, c, written.Result, "Read the artifact with "+
			GetArtifactTool+": its shares say whether the change landed."), nil
	}
	d.settle(ctx, written.Result.Position)
	out := map[string]any{"artifact": h.ID.String(), "outcome": outcomeOf(written.Result),
		"position": positionOf(written.Result.Position)}
	for k, v := range said {
		out[k] = v
	}
	if url := d.link(h.ID); url != "" {
		out["url"] = url
	}
	return jsonResult(withArtifactOperation(out, c))
}

// ---- upload_artifact_asset ---------------------------------------------- //

type uploadArtifactAsset struct {
	artifactTarget
	deps ArtifactDeps
}

var _ tools.SeatCallable = (*uploadArtifactAsset)(nil)

func (t *uploadArtifactAsset) Name() string { return UploadArtifactAssetTool }

func (t *uploadArtifactAsset) Description() string {
	return "Add a file to an artifact's asset store in this Crewlet company (an image, " +
		"a data file, a font) without publishing a new version. The artifact must " +
		"declare the `assets` capability. The answer's `reference` is the path a page " +
		"at the version's root uses for it (a page in a folder climbs to the root with `../`)."
}

func (t *uploadArtifactAsset) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"artifact": artifactParam(),
			"name": map[string]any{
				"type":        "string",
				"description": "The asset's name with its extension, e.g. `logo.png`.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "The asset's bytes inline, at most 1 MiB decoded.",
			},
			"encoding": map[string]any{
				"type": "string", "enum": []any{"text", "base64"},
				"description": "How `content` is written: `text` (the default) or `base64`.",
			},
			"from_sandbox": map[string]any{
				"type": "string",
				"description": "A file your coding run wrote in this turn, relative to " +
					"its workspace.",
			},
			"from_project_file": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project": map[string]any{"type": "string"},
					"path":    map[string]any{"type": "string"},
				},
				"description": "A project's file to copy in.",
			},
		},
		"required": []any{"artifact", "name"},
	})
}

func (t *uploadArtifactAsset) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *uploadArtifactAsset) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	c, refusal := t.deps.caller(ctx, turn, t.Name(), args, true)
	if refusal != nil {
		return *refusal, nil
	}
	h, ref, refusal := t.deps.head(ctx, t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	dec, refusal := t.deps.decide(ctx, c, t.Name(), authz.ActionArtifactAsset, h, ref)
	if refusal != nil {
		return *refusal, nil
	}
	t.deps.present(c, h.ID, presence.Editing, presence.Focus{})
	if !slices.Contains(h.Capabilities, artifacts.CapabilityAssets) {
		return failed(fmt.Sprintf(UploadArtifactAssetTool+": artifact %q does not declare "+
			"the `assets` capability, so its page cannot reference an asset. Publish a "+
			"version declaring it first.", clip(ref))), nil
	}
	files := newFileSources(t.deps, c, t.Name())
	name, refusal := files.cleanPath(argString(args, "name"))
	if refusal != nil {
		return *refusal, nil
	}
	spec := map[string]any{}
	for _, field := range []string{"content", "encoding", "from_sandbox", "from_project_file"} {
		if v, ok := args[field]; ok {
			spec[field] = v
		}
	}
	if refusal := files.parseSpec(ctx, name, spec); refusal != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
		return *refusal, nil
	}
	// AN ASSET IS ALWAYS AN UPLOAD of its own, never carried and never
	// inline in a record, so inline bytes are streamed through the same
	// reservation as a box's.
	files.files[0] = streamedInline(files.files[0])
	call := t.deps.call(c, dec.Standing.Role, dec.Elevated)
	upload, _, refusal := files.stage(ctx, call, h.ID, true)
	if refusal != nil {
		return *refusal, nil
	}
	asset, err := t.deps.Publisher.AddAsset(ctx, call, h.ID, upload, name)
	if err != nil {
		return publishFailure(ctx, t.Name(), c, err), nil
	}
	out := map[string]any{
		"artifact": h.ID.String(), "asset": asset.ID.String(), "name": asset.Name,
		"content_type": asset.ContentType, "size": asset.Object.Size,
		"reference": "_assets/" + asset.ID.String(),
	}
	if url := t.deps.link(h.ID); url != "" {
		out["url"] = url
	}
	return jsonResult(withArtifactOperation(out, c))
}
