package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE COMPANY'S PRINCIPLES: its mission, vision and standing policies, the one
// part of the company document the assistant reaches (ADR-0052's exception).
//
// ONE RESOURCE, ONE WRITE PATH. The read and the write are the routes Company
// › Principles itself sends (`GET` and `PUT /config/charter`), decided by
// verbs of their own (`config.principles.read` and `.write`), so an approved
// change is the same request, under the same rules, refusals and step-up, as
// the person's own Save. Nothing else of the document is named here or
// reachable through it.
//
// A CHANGE IS THE WHOLE OF EACH PART IT NAMES. The call names the parts it
// changes (the mission, the vision, the policies), each replaced whole; the
// preview reads the principles as they are now and fills in every part the
// call leaves out, so the request the approval sends states all three, as the
// route requires, and keeps what the call did not mean to touch exactly as it
// is.

// The principles' two routes.
const (
	PrinciplesRoute      = "GET /config/charter"
	PrinciplesWriteRoute = "PUT /config/charter"
)

// principlesParts are the three parts, in the order the page shows them.
var principlesParts = []string{"mission", "vision", "policies"}

// The schema's bounds are the route's, in bytes: the route counts characters
// (internal/config's charter bounds), and a character is at most
// [utf8.UTFMax] bytes, so a value the schema refuses is one the route would.
var (
	missionBytes = config.MaxMissionChars * utf8.UTFMax
	visionBytes  = config.MaxVisionChars * utf8.UTFMax
	policyBytes  = config.MaxPolicyChars * utf8.UTFMax
)

// principlesRead is get_company_principles.
func principlesRead() Tool {
	return Tool{
		Name: "get_company_principles", Phrase: "read the company's principles",
		Description: "The company's principles as Company › Principles shows them: its " +
			"mission, its vision and its standing policies. Read them here, never by " +
			"searching the knowledge base, whenever the person asks about or wants to " +
			"change the mission, the vision or a policy.",
		schema: object(nil, map[string]*schema{}),
		Routes: []string{PrinciplesRoute},
		// OFFERED AS THE ROUTE ADMITS: an administrator's, as the whole
		// document is ([authz.ActionPrinciplesRead]). The page itself is
		// everybody's, read from the org chart, which get_org_chart answers.
		Apps: adminOnly,
		// THE PRINCIPLES ARE PEOPLE'S PROSE, written by the company's own
		// administrators: colleague text, fenced.
		Fence: true,
		Build: func(map[string]any) (exec.Request, error) {
			return get(PrinciplesRoute, nil, nil)
		},
		Cards: one(cards.Spec{Entity: &cards.Entity{Kind: wire.ResourceUnit,
			Name: "Company principles", Prose: true,
			Facts: []cards.Fact{fact("Mission", "mission", tText),
				fact("Vision", "vision", tText)},
			Items: &cards.Items{Path: "policies", Kind: wire.ResourceUnit, Plain: true,
				Prose: true, Noun: wire.Noun{One: "policy", Many: "policies"}},
			Open: &cards.Open{Screen: "principles"}}}),
		Label: func(map[string]any) string { return "Reading the company's principles" },
	}
}

// principlesWrite is update_company_principles.
func principlesWrite() Tool {
	act := authz.ActionPrinciplesWrite
	return Tool{
		Name: "update_company_principles", Act: string(act), Kind: KindWrite,
		Phrase: "change the company's principles",
		Description: "Propose changing the company's mission, vision or standing policies. " +
			"Give only the parts that change, each as its whole new text (policies as " +
			"the whole new list); the parts you leave out stay exactly as they are. Read " +
			"them with get_company_principles first. The person sees the before and " +
			"after and approves it before anything is saved. Use this for any request " +
			"to change them; propose_page_change into the Principles editor only " +
			"drafts into a part the person has open and asked you to draft.",
		schema: object(nil, map[string]*schema{
			"mission": text("The new mission, whole.", missionBytes),
			"vision":  text("The new vision, whole.", visionBytes),
			"policies": {kind: "array", description: "The new standing policies, the " +
				"whole list in order, one sentence or two each.", maxItems: config.MaxPolicies,
				items: &schema{kind: "string", maxLen: policyBytes}},
		}),
		Routes: []string{PrinciplesWriteRoute},
		Tier:   DeriveTier(act, false),
		// OFFERED AS THE ROUTE ADMITS: no operator-catalogue tool executes
		// it, so the viewer's acts never name it ([Offered]).
		Apps: adminOnly,
		Build: func(a map[string]any) (exec.Request, error) {
			args := Args(a)
			for _, part := range principlesParts {
				if _, given := a[part]; !given {
					return exec.Request{}, fmt.Errorf("a change of the principles states "+
						"all three parts, and this one leaves out %s; its preview fills in "+
						"the parts a call does not change", part)
				}
			}
			body, err := json.Marshal(map[string]any{"mission": args.String("mission"),
				"vision": args.String("vision"), "policies": args.Strings("policies")})
			if err != nil {
				return exec.Request{}, fmt.Errorf("catalog: encode the principles: %w", err)
			}
			req, err := at(PrinciplesWriteRoute, nil, nil)
			req.Body = body
			return req, err
		},
		Label: func(map[string]any) string { return "Preparing a change to the company's principles" },
	}
}

// principlesPreview reads the principles as they are now, through the turn's
// grant, and renders what the call changes: a word diff of the mission and the
// vision, and one row per policy added, removed or reworded. The proposal is
// bound to the principles' entity tag, so a change made meanwhile makes it
// stale, and the approval's write is sent under that tag as its If-Match.
func principlesPreview(t Tool) proposal.Previewer {
	return func(ctx context.Context, read proposal.Read, _ time.Time, args map[string]any) (proposal.Preview, error) {
		now, version, err := readPrinciples(ctx, read)
		if err != nil {
			return proposal.Preview{}, err
		}
		merged := maps.Clone(args)
		if merged == nil {
			merged = map[string]any{}
		}
		for _, part := range principlesParts {
			if _, given := merged[part]; given {
				continue
			}
			if part == "policies" {
				merged[part] = anyList(now.policies)
				continue
			}
			merged[part] = now.part(part)
		}
		req, err := built(t, merged)
		if err != nil {
			return proposal.Preview{}, err
		}
		next := Args(merged)
		var c changes
		c = append(c, textChange("mission", "Mission", now.mission, next.String("mission"))...)
		c = append(c, textChange("vision", "Vision", now.vision, next.String("vision"))...)
		c = append(c, policyChanges(now.policies, next.Strings("policies"))...)
		if len(c) == 0 {
			return proposal.Preview{}, fmt.Errorf("%w: the change leaves the principles "+
				"exactly as they are", ErrInvalidArguments)
		}
		revision := proposal.Revision{Kind: proposal.RevisionCharter,
			ID: proposal.CharterID, Version: version}.String()
		// THE ROUTE'S OWN CHECK, as every route declaring one is asked
		// (ADR-0055): a policy the charter refuses is the step's refusal,
		// never a card, and its warnings are rows.
		warnings, err := checked(ctx, read, req, revision)
		if err != nil {
			return proposal.Preview{}, err
		}
		for i, warning := range warnings {
			c.fact("check.warning."+strconv.Itoa(i+1), "Warning", warning)
		}
		recency, _ := authz.RecencyOf(authz.Action(t.Act))
		return proposal.Preview{Title: "Update the company's principles",
			Target: "Company principles",
			Note: "Every agent works from the principles, so each one follows the new " +
				"wording from its next turn.",
			ApproveLabel: "Approve and save", Changes: c.rows(), Request: req,
			// THE ROW'S OWN STEP-UP, shown on the card: the approval asks it
			// either way (authz.RecencyOf), and the card says so beforehand.
			StepUp:         recency == iam.RecencyStepUp,
			SourceRevision: revision}, nil
	}
}

// principles are the three parts as the route answered them.
type principles struct {
	mission, vision string
	policies        []string
}

func (p principles) part(name string) string {
	if name == "vision" {
		return p.vision
	}
	return p.mission
}

// readPrinciples reads the principles through the grant: the three parts, and
// the entity tag the route answers, unquoted, as a charter revision's version.
func readPrinciples(ctx context.Context, read proposal.Read) (principles, string, error) {
	req, err := get(PrinciplesRoute, nil, nil)
	if err != nil {
		return principles{}, "", err
	}
	resp, doc, err := readAnswer(ctx, read, req)
	if err != nil {
		return principles{}, "", err
	}
	tag := resp.Header.Get("ETag")
	if !exec.ValidEntityTag(tag) {
		return principles{}, "", fmt.Errorf("%w: the principles were answered without "+
			"an entity tag to bind the change to", ErrPreviewRead)
	}
	out := principles{mission: value(doc, "mission"), vision: value(doc, "vision")}
	list, _ := cards.Lookup(doc, "policies").([]any)
	for _, p := range list {
		out.policies = append(out.policies, cards.Text(p))
	}
	return out, strings.Trim(tag, `"`), nil
}

// textChange is the row for one statement moving from was to is: none where it
// is the same, an added or a removed value where one side is empty, and a word
// diff otherwise, so the person reads exactly which words change.
func textChange(field, label, was, is string) []wire.Change {
	switch {
	case was == is:
		return nil
	case was == "":
		return []wire.Change{proposal.Added(field, label, is, false)}
	case is == "":
		return []wire.Change{proposal.Removed(field, label, was)}
	}
	return []wire.Change{proposal.Text(field, label, was, is)}
}

// policyChanges are one row per policy the change touches, matched by a
// longest common subsequence of the two lists so a policy that only moved up
// one place is not shown as every policy below it rewritten: a policy only
// the old list holds is removed, one only the new list holds is added, and a
// removal followed at once by an addition is the one policy reworded, shown as
// a word diff. Each row names the policy's place in the list it belongs to.
func policyChanges(was, is []string) []wire.Change {
	n, m := len(was), len(is)
	// lcs[i][j] is the longest common subsequence of was[i:] and is[j:].
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if was[i] == is[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []wire.Change
	label := func(at int) string { return "Policy " + strconv.Itoa(at+1) }
	field := func(at int) string { return "policies." + strconv.Itoa(at+1) }
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && was[i] == is[j]:
			i, j = i+1, j+1
		case i < n && j < m && lcs[i+1][j+1] == lcs[i][j]:
			// NEITHER KEPT: the policy at this place was reworded.
			out = append(out, proposal.Text(field(j), label(j), was[i], is[j]))
			i, j = i+1, j+1
		case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
			out = append(out, proposal.Added(field(j), label(j), is[j], false))
			j++
		default:
			out = append(out, proposal.Removed("policies.was."+strconv.Itoa(i+1),
				label(i), was[i]))
			i++
		}
	}
	return out
}

// anyList is a list of strings as a call's arguments carry one.
func anyList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, v := range values {
		out = append(out, v)
	}
	return out
}

// readAnswer sends one read through the grant and answers the response beside
// its decoded body, for a preview that reads a header as well (an entity
// tag); a refusal is [ErrPreviewRead] in the route's own words.
func readAnswer(ctx context.Context, read proposal.Read, req exec.Request) (exec.Response, any, error) {
	resp, err := read(ctx, req)
	if err != nil {
		return exec.Response{}, nil, fmt.Errorf("%w: %w", ErrPreviewRead, err)
	}
	if !cards.Permitted(resp.Status) {
		r := exec.RefusalOf(resp)
		return exec.Response{}, nil, fmt.Errorf("%w: %s (%s)", ErrPreviewRead, r.Message, r.Code)
	}
	if resp.Truncated {
		return exec.Response{}, nil, fmt.Errorf("%w: the answer is larger than a change's preview "+
			"reads", ErrPreviewRead)
	}
	doc, err := cards.Decode(resp.Body)
	return resp, doc, err
}

// principlesRevision is the charter revision the principles are at now.
func principlesRevision(ctx context.Context, read proposal.Read) (string, error) {
	_, version, err := readPrinciples(ctx, read)
	if err != nil {
		return "", err
	}
	return proposal.Revision{Kind: proposal.RevisionCharter, ID: proposal.CharterID,
		Version: version}.String(), nil
}

// isDotted reports a verb no operator-catalogue tool is named for (the
// authority table spells such a verb with a dot), which the viewer's acts
// therefore never list.
func isDotted(act string) bool { return strings.Contains(act, ".") }
