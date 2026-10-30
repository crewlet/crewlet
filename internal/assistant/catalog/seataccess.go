package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// WHAT AN AGENT MAY DO AT ONE INTEGRATION, CHANGED THE WAY ITS AGENTS TAB DOES.
//
// ONE SEAT, ONE LANE, ONE WRITE PATH. The change is the request the tab's Save
// sends (`PUT /setup/integrations/{kind}/access`), decided by the same verb
// (`setup.access.write`), with the same step-up, the same raise rule for an
// administration role (`setup.access.raise`) and the same conditional write, so
// an approved change is the person's own Save. The preview reads the tab as it
// is now and asks the route's own check for the plan, which is the sync's own
// function: the card shows what the engine will do at the vendor, step by step,
// before anybody approves it.
//
// IT NEVER TAKES A VARIABLE NAME. The agent's credential is sealed under the
// name the engine derives for it, so the call carries on or off, the vendor's
// level, its permissions and its scope, and nothing else. The incident this
// exists for was a generic change that pointed an agent's credential at a
// company variable; the document's own rules now refuse that, and this tool
// leaves no field to try it with.

// The Agents tab's two routes.
const (
	accessReadRoute  = "GET /setup/integrations/{kind}/access"
	accessWriteRoute = "PUT /setup/integrations/{kind}/access"
)

// laneTitles are each lane's vendor, by its own name, for the card.
var laneTitles = map[seataccess.Lane]string{
	seataccess.LaneDatadog: "Datadog", seataccess.LaneGitHub: "GitHub",
	seataccess.LaneGitLab: "GitLab", seataccess.LaneJira: "Jira",
	seataccess.LaneConfluence: "Confluence", seataccess.LaneSlack: "Slack",
	seataccess.LaneMattermost: "Mattermost",
}

// githubGrantWords are GitHub's own words for a permission's grant.
var githubGrantWords = map[seataccess.Access]string{
	"": "No access", seataccess.AccessRead: "Read-only", seataccess.AccessWrite: "Read and write",
}

// seatAccessWrite is set_seat_access.
func seatAccessWrite() Tool {
	act := authz.ActionSetupAccessWrite
	lanes := make([]string, 0, len(seataccess.Lanes()))
	for _, lane := range seataccess.Lanes() {
		lanes = append(lanes, string(lane))
	}
	permissions := map[string]*schema{}
	for _, key := range config.GitHubPermissionKeys() {
		permissions[key] = oneOf("none, read or write.", "none", "read", "write")
	}
	return Tool{
		Name: "set_seat_access", Act: string(act), Kind: KindWrite,
		Phrase: "change an agent's access to an integration",
		Description: "Propose turning an agent on or off at one integration, or changing " +
			"what it may do there, in the vendor's own terms: its role (access, by the " +
			"vendor's id or exact name, such as Datadog Standard Role, Developer or " +
			"Viewers), its GitHub permissions, and its scope (the repositories, " +
			"projects, spaces or channels it is narrowed to). Give only what changes; " +
			"everything else stays as it is. Read the integration with " +
			"list_integrations first. The person sees what the engine will do at the " +
			"vendor and approves it before anything is saved. The engine names and " +
			"seals the agent's credential itself, so never name a variable, a key or a " +
			"token. Use this for any change to an agent's access at an integration, " +
			"never propose_api_change on the company document.",
		schema: object([]string{"handle", "integration"}, map[string]*schema{
			"handle": text("The agent's handle or name.", nameBytes),
			"integration": oneOf("The integration. Jira and Confluence are set "+
				"separately, on the agent's one Atlassian account.", lanes...),
			"enabled": flag("true turns the agent on there; false turns it off, which " +
				"disables its account at the vendor and keeps it."),
			"access": text("The role or level, by the vendor's id or its exact name. "+
				"Not for GitHub (use permissions) or Slack (it has none).", nameBytes),
			"permissions": object(nil, permissions),
			"scope": names("Every repository, project, space or channel the agent is "+
				"narrowed to, replacing the list; an empty list is the vendor's default "+
				"reach.", 100),
		}),
		Names:  map[string]Directory{"handle": DirSeat},
		Routes: []string{accessWriteRoute},
		Tier:   DeriveTier(act, false),
		// OFFERED TO AN ADMINISTRATOR, whom the route admits: setting an
		// agent's access at a vendor is admin's, as connecting one is.
		Apps: adminOnly,
		Build: func(a map[string]any) (exec.Request, error) {
			lane := seataccess.Lane(Args(a).String("integration"))
			if !lane.Valid() {
				return exec.Request{}, fmt.Errorf("%w: no integration %q", ErrInvalidArguments,
					lane)
			}
			// THE BODY IS THE SEAT'S LANE AS READ, with the call's change
			// over it, which only the preview has.
			return at(accessWriteRoute, map[string]string{"kind": string(lane.Kind())}, nil)
		},
		Preview: seatAccessPreview,
		Label: func(a map[string]any) string {
			args := Args(a)
			title := laneTitles[seataccess.Lane(args.String("integration"))]
			if handle := args.String("handle"); handle != "" && len(handle) <= nameBytes &&
				title != "" {
				return "Preparing a change to " + handle + "'s " + title + " access"
			}
			return "Preparing a change to an agent's access"
		},
	}
}

// seatAccessPreview reads the integration's Agents tab as it is now, applies
// the call's change to the one seat's lane, asks the route's own check for the
// plan, and draws the change and the plan, bound to the revision the tab was
// read at.
func seatAccessPreview(ctx context.Context, read proposal.Read, _ time.Time,
	a map[string]any) (proposal.Preview, error) {

	args := Args(a)
	lane := seataccess.Lane(args.String("integration"))
	if !lane.Valid() {
		return proposal.Preview{}, fmt.Errorf("%w: no integration %q", ErrInvalidArguments, lane)
	}
	title := laneTitles[lane]
	path := map[string]string{"kind": string(lane.Kind())}
	readReq, err := get(accessReadRoute, path, nil)
	if err != nil {
		return proposal.Preview{}, err
	}
	rep, err := readRepresentation(ctx, read, Route{Pattern: accessReadRoute}, readReq.Path)
	switch {
	case err != nil:
		return proposal.Preview{}, err
	case rep.absent != nil:
		return proposal.Preview{}, rep.absent
	case rep.revision == "":
		return proposal.Preview{}, fmt.Errorf("%w: %s was answered without an entity tag to "+
			"bind the change to", ErrPreviewRead, readReq.Path)
	}
	var answer seataccess.Answer
	if err = redecode(rep.doc, &answer); err != nil {
		return proposal.Preview{}, fmt.Errorf("%w: %s: %w", ErrPreviewRead, readReq.Path, err)
	}
	seat, err := accessSeat(answer, args.String("handle"), title)
	if err != nil {
		return proposal.Preview{}, err
	}
	var offer seataccess.Options
	for _, o := range answer.Options {
		if o.Lane == lane {
			offer = o
		}
	}
	before := seat.Lanes[lane].Desired
	after, err := accessAfter(a, before, offer, title)
	if err != nil {
		return proposal.Preview{}, err
	}
	if after.Enabled == before.Enabled && after.SameAccess(before) {
		return proposal.Preview{}, fmt.Errorf("%w: that is already %s's %s access",
			ErrInvalidArguments, seat.Name, title)
	}

	req, err := at(accessWriteRoute, path, nil)
	if err != nil {
		return proposal.Preview{}, err
	}
	req.Body, err = json.Marshal(map[string]any{
		"seats": map[string]map[seataccess.Lane]seataccess.Desired{seat.Handle: {lane: after}},
	})
	if err != nil {
		return proposal.Preview{}, fmt.Errorf("catalog: encode the change: %w", err)
	}
	// THE ROUTE'S OWN CHECK, which answers the plan the sync would take and
	// the warnings the revision would introduce ([checkAnswer]).
	body, err := checkAnswer(ctx, read, req, rep.revision)
	if err != nil {
		return proposal.Preview{}, err
	}
	var plan seataccess.PlanAnswer
	if err := json.Unmarshal(body, &plan); err != nil {
		return proposal.Preview{}, fmt.Errorf("%w: the check of %s answered no plan: %w",
			ErrPreviewRead, req.Path, err)
	}

	var c changes
	c.move("enabled", title, onWords(before.Enabled), onWords(after.Enabled))
	c.move("level", title+" role", levelWords(offer, before.Level), levelWords(offer, after.Level))
	for _, p := range offer.Permissions {
		c.move("permissions."+p.Key, p.Name, githubGrantWords[before.Permissions[p.Key]],
			githubGrantWords[after.Permissions[p.Key]])
	}
	c.move("scope", "Scope", scopeWords(before.Scope), scopeWords(after.Scope))
	step := 0
	for _, sp := range plan.Plans {
		if sp.Handle != seat.Handle || sp.Lane != lane {
			continue
		}
		for _, s := range sp.Steps {
			step++
			who := "Crewlet, at " + title
			if s.By == seataccess.ByYou {
				who = "You, at " + title
			}
			line := s.Text
			if s.Note != "" {
				line += " (" + s.Note + ")"
			}
			c.fact("plan."+strconv.Itoa(step), who, line)
		}
	}
	if plan.StepUp {
		c.fact("raise", "Administration", "This may give "+seat.Name+" an administration "+
			"role at "+title+", or "+title+" could not confirm it does not, so approving it "+
			"asks you to confirm it is you, at a keyboard.")
	}
	for i, warning := range checkWarnings(body) {
		c.fact("check.warning."+strconv.Itoa(i+1), "Warning", warning)
	}
	recency, _ := authz.RecencyOf(authz.ActionSetupAccessWrite)
	return proposal.Preview{Title: "Change an agent's access",
		Target: seat.Name + " on " + title,
		Note: "Saved as one change to the company configuration, then applied at " + title +
			" by the next sync. Turning an agent off keeps its account, so it can be " +
			"turned back on.",
		ApproveLabel: "Approve and save", Changes: c.rows(), Request: req,
		StepUp:         recency == iam.RecencyStepUp || plan.StepUp,
		SourceRevision: rep.revision}, nil
}

// accessSeat is the seat a call names on an access answer, or the refusal
// naming the agents the integration lists.
//
// A HANDLE NAMES ONE SEAT, and wins; a name may be shared, so it names a seat
// only where exactly one carries it, and two agents of the same name are
// refused, naming both handles, rather than one of them changed on a guess.
func accessSeat(answer seataccess.Answer, named, title string) (seataccess.SeatAnswer, error) {
	named = strings.TrimSpace(named)
	var listed []string
	var byName []seataccess.SeatAnswer
	for _, seat := range answer.Seats {
		if seat.Departed {
			continue
		}
		if seat.Handle == named {
			return seat, nil
		}
		if strings.EqualFold(seat.Name, named) {
			byName = append(byName, seat)
		}
		listed = append(listed, seat.Name+" ("+seat.Handle+")")
	}
	switch len(byName) {
	case 1:
		return byName[0], nil
	case 0:
		return seataccess.SeatAnswer{}, fmt.Errorf("%w: %s lists no agent %q; it lists %s",
			ErrInvalidArguments, title, named, strings.Join(listed, ", "))
	}
	handles := make([]string, 0, len(byName))
	for _, seat := range byName {
		handles = append(handles, seat.Handle)
	}
	return seataccess.SeatAnswer{}, fmt.Errorf("%w: %d agents on %s are called %q; name "+
		"one by its handle: %s", ErrInvalidArguments, len(byName), title, named,
		strings.Join(handles, ", "))
}

// accessAfter is the seat's lane with the call's change over it: only what the
// call gives changes.
func accessAfter(a map[string]any, before seataccess.Desired, offer seataccess.Options,
	title string) (seataccess.Desired, error) {

	args := Args(a)
	after := before.Normalized()
	if _, given := a["enabled"]; given {
		after.Enabled = args.Bool("enabled")
	}
	if named := strings.TrimSpace(args.String("access")); named != "" {
		level, err := accessLevel(offer, named, title)
		if err != nil {
			return seataccess.Desired{}, err
		}
		after.Level = level
	}
	if given, ok := a["permissions"].(map[string]any); ok {
		if len(offer.Permissions) == 0 {
			return seataccess.Desired{}, fmt.Errorf("%w: only GitHub takes permissions",
				ErrInvalidArguments)
		}
		next := maps.Clone(after.Permissions)
		if next == nil {
			next = seataccess.Permissions{}
		}
		for key, v := range given {
			switch grant, _ := v.(string); grant {
			case "none":
				delete(next, key)
			default:
				next[key] = seataccess.Access(grant)
			}
		}
		after.Permissions = next
	}
	if _, given := a["scope"]; given {
		after.Scope = args.Strings("scope")
	}
	return after.Normalized(), nil
}

// accessLevel is the level a call names on offer: by the vendor's id, which
// names one level and wins, or by its exact name where exactly one level
// carries it (a custom GitLab role may share a built-in one's name).
func accessLevel(offer seataccess.Options, named, title string) (string, error) {
	switch {
	case offer.Lane == seataccess.LaneGitHub || offer.Lane == seataccess.LaneSlack:
		return "", fmt.Errorf("%w: %s has no roles to choose from; GitHub takes "+
			"permissions, and Slack's scopes are set in each app", ErrInvalidArguments, title)
	case len(offer.Levels) == 0:
		return "", fmt.Errorf("%w: %s did not answer when its roles were read, so none "+
			"can be chosen now; try again shortly", ErrInvalidArguments, title)
	}
	if i := slices.IndexFunc(offer.Levels, func(l seataccess.Level) bool {
		return l.ID == named
	}); i >= 0 {
		return offer.Levels[i].ID, nil
	}
	var ids, offered []string
	for _, l := range offer.Levels {
		offered = append(offered, l.Name)
		if strings.EqualFold(l.Name, named) {
			ids = append(ids, l.ID)
		}
	}
	switch len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		return "", fmt.Errorf("%w: %s offers no role %q; it offers %s",
			ErrInvalidArguments, title, named, strings.Join(offered, ", "))
	}
	return "", fmt.Errorf("%w: %s offers %d roles called %q; name one by its id: %s",
		ErrInvalidArguments, title, len(ids), named, strings.Join(ids, ", "))
}

// redecode reads a decoded answer into a typed one.
func redecode(doc any, into any) error {
	raw, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, into)
}

func onWords(on bool) string {
	if on {
		return "On"
	}
	return "Off"
}

// levelWords is a level by the vendor's name for it.
func levelWords(offer seataccess.Options, id string) string {
	if id == "" {
		return ""
	}
	// A SET OF LEVELS (Confluence's space permissions by operation) is
	// named level by level, as the picker draws it.
	if levels, ok := offer.LevelsOf(id); ok {
		names := make([]string, 0, len(levels))
		for _, level := range levels {
			names = append(names, level.Name)
		}
		return strings.Join(names, ", ")
	}
	return id
}

// scopeWords is a scope as a list, or the vendor's default reach.
func scopeWords(scope []string) string {
	if len(scope) == 0 {
		return "Default reach"
	}
	return strings.Join(scope, ", ")
}
