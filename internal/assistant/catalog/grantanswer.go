package catalog

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// An answer to an agent's grant request (ADR-0118, ADR-0150) drawn in the
// engine's words rather than the question's option labels: the grant the
// stored request asks for, by the catalogue's name, where it applies, how long
// it lasts and whether it changes things at the vendor. Choosing a grant option
// IS the approval: the answer's route saves the grant before it posts the
// answer, at the access write's own proof, so the card asks the person to
// confirm it is them as a save on the Agents tab does. Declining is drawn as a
// decline of that grant, which changes nothing.

// accessRequestRoute is one agent's stored request for a grant.
const accessRequestRoute = "GET /setup/access/requests/{id}"

// grantChoice is a choice on an agent's grant request's question: the request
// its options carry, and whether the choice grants it (for good where
// permanent) or declines it.
type grantChoice struct {
	request   string
	grants    bool
	permanent bool
}

// grantChoiceOf is the grant request choice answers, where the question it
// answers is one: a grant option, or another option of a question whose
// options carry a request (its decline). False for any other answer, and for
// one in words alone.
func grantChoiceOf(options map[string]string, choice string) (grantChoice, bool) {
	if choice == "" {
		return grantChoice{}, false
	}
	if id, permanent, ok := seataccess.GrantOption(choice); ok {
		return grantChoice{request: id, grants: true, permanent: permanent}, true
	}
	for option := range options {
		if id, _, ok := seataccess.GrantOption(option); ok {
			return grantChoice{request: id}, true
		}
	}
	return grantChoice{}, false
}

// grantAnswerPreview is answer_decision's card for a grant request's question:
// the stored request, and the agent's lane with the requested permission looked
// up in its live catalogue, so every word on it is the engine's. A grant the
// company's connection cannot give yet is refused here, saying what a person
// adds at the vendor, rather than prepared for an approval the save would
// refuse.
func grantAnswerPreview(ctx context.Context, read proposal.Read, req exec.Request, thread any,
	a Args, g grantChoice) (proposal.Preview, error) {

	stored, err := readGet(ctx, read, accessRequestRoute, map[string]string{"id": g.request}, nil)
	if err != nil {
		return proposal.Preview{}, err
	}
	var r seataccess.GrantRequest
	if err = redecode(stored, &r); err != nil {
		return proposal.Preview{}, fmt.Errorf("%w: %s: %w", ErrPreviewRead, accessRequestRoute, err)
	}
	doc, err := readGet(ctx, read, accessSeatRoute, map[string]string{"kind": string(r.Kind),
		"handle": r.Handle, "lane": string(r.Lane)}, url.Values{"permission": {r.Permission}})
	if err != nil {
		return proposal.Preview{}, err
	}
	var view seataccess.LaneView
	if err = redecode(doc, &view); err != nil {
		return proposal.Preview{}, fmt.Errorf("%w: %s: %w", ErrPreviewRead, accessSeatRoute, err)
	}
	entry, listed := lookedUp(view, r.Permission)
	name := r.Permission
	if listed && entry.Name != "" {
		name = entry.Name
	}
	where := "on " + view.Integration + "'s whole reach for it"
	if len(r.Scope) > 0 {
		where = "on " + strings.Join(r.Scope, ", ")
	}

	var c changes
	c.fact("task", "Task", value(thread, "key")+" "+value(thread, "title"))
	c.fact("agent", "Agent", view.Name)
	if !g.grants {
		c.add("choice", "Answer", "Not now: decline the request for "+name+" "+where)
		c.move("body", "Comment", "", a.String("body"))
		return proposal.Preview{Title: "Decline an agent's request", Target: view.Name,
			Note:         view.Name + " is told no and may ask again. Nothing about its access changes.",
			ApproveLabel: "Approve and decline", Changes: c.rows(), Request: req}, nil
	}
	switch {
	case !listed && view.LookupNote != "":
		return proposal.Preview{}, fmt.Errorf("%w: %s cannot be granted now: %s", ErrInvalidArguments,
			name, view.LookupNote)
	case !listed:
		return proposal.Preview{}, fmt.Errorf("%w: %s's catalogue no longer offers %s; answer "+
			"the question Not now, and the agent asks again for what it offers", ErrInvalidArguments,
			view.Integration, name)
	case entry.Grantable != "Yes":
		return proposal.Preview{}, fmt.Errorf("%w: %s cannot be granted yet: %s", ErrInvalidArguments,
			name, missingWords(entry))
	}
	c.add("grant", "Grant", name+" "+where)
	lasts := "For good, until somebody removes it on the Agents tab"
	if !g.permanent {
		lasts = hoursWords(r.Hours) + " from when you approve, then it lapses"
	}
	c.add("lasts", "Lasts", lasts)
	c.fact("level", "Beside its level", view.Grant)
	if entry.Write {
		c.fact("raise", "Changes things", name+" may change things at "+view.Integration+", so "+
			"approving it asks you to confirm it is you, at a keyboard.")
	}
	c.move("body", "Comment", "", a.String("body"))
	recency, _ := authz.RecencyOf(authz.ActionSetupAccessWrite)
	return proposal.Preview{Title: "Grant an agent's request", Target: view.Name + " on " + view.Integration,
		Note: "Saved as one change to the company configuration, then " + view.Name + " is told yes. " +
			view.Integration + " applies it on the next sync.",
		ApproveLabel: "Approve and grant", Changes: c.rows(), Request: req,
		StepUp: recency == iam.RecencyStepUp || entry.Write}, nil
}

// lookedUp is the entry of a lane view's lookup that names permission.
func lookedUp(view seataccess.LaneView, permission string) (seataccess.LookupEntry, bool) {
	for _, e := range view.Lookup {
		if e.ID == permission {
			return e, true
		}
	}
	return seataccess.LookupEntry{}, false
}

// missingWords is why a catalogue entry cannot be granted yet, with what a
// person adds to the company's connection at the vendor and where.
func missingWords(e seataccess.LookupEntry) string {
	words := e.Grantable
	if e.Missing != "" {
		words += "; a person adds " + e.Missing + " to the company's connection"
		if e.ActionURL != "" {
			words += " at " + e.ActionURL
		}
		words += " first"
	}
	return words
}

// hoursWords is a grant's length as a person reads it.
func hoursWords(hours int) string {
	if hours == 1 {
		return "1 hour"
	}
	return strconv.Itoa(hours) + " hours"
}
