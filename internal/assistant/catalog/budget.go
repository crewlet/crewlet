package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/proposal"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A TOKEN CEILING, CHANGED THE WAY BUDGETS CHANGES ONE.
//
// ONE WINDOW, ONE SCOPE, ONE WRITE PATH. A seat's ceilings are the seat's own
// `token_budget`, written by `PUT /config/roles/{handle}` with the seat as it
// was read and only that window changed; the company's are the document's,
// written by `PATCH /config` with a merge patch of that one window. Those are
// exactly the requests Company › Budgets sends (the dashboard's
// `lib/useCeilingWrite.ts`), decided by the same verb (`config.org.write`, a
// lead's for the seats they lead, an administrator's for the company), with
// the same step-up and the same conditional write, so an approved change is
// the person's own Save under the same rules. Before this tool, the model
// either opened the editor for the person to save (the page's prefill) or
// replaced a whole seat through the generic change, showing every field of it.
//
// THE CARD SHOWS THE ONE WINDOW. The preview reads the scope as it is now,
// through the turn's grant, and draws one row (the window's ceiling from what
// it is to what it would be); the history line the revision records is the
// card's; and the warnings are the route's own check of the change, read by
// the one function every write preview reads them with ([checked]), so the
// rule that keeps a warning the company already carried off the card is the
// same for this card as for every other.

// The two scopes' routes.
const (
	seatReadRoute     = "GET /config/roles/{id}"
	seatWriteRoute    = "PUT /config/roles/{id}"
	companyReadRoute  = "GET /config"
	companyWriteRoute = "PATCH /config"
)

// budgetWindows are the windows a ceiling is set for, in the order Budgets
// draws them (internal/config.TokenBudget), each with its adjective.
var budgetWindows = []string{"day", "week", "month"}

var windowAdjective = map[string]string{"day": "daily", "week": "weekly", "month": "monthly"}

// maxCeiling is the largest ceiling a call may name: the largest integer a
// JSON number carries exactly, so the value the card shows is the value sent.
const maxCeiling = 1<<53 - 1

// budgetWrite is set_token_budget.
func budgetWrite() Tool {
	act := authz.ActionOrgWrite
	return Tool{
		Name: "set_token_budget", Act: string(act), Kind: KindWrite,
		Phrase: "change a token budget",
		Description: "Propose setting or removing one token ceiling: a seat's or the " +
			"company's, for one window (day, week or month). Name the seat by its handle or name, or " +
			"leave seat out for the company's own ceiling; give tokens as a whole number, or " +
			"clear: true to remove that window's ceiling. Read the current ceilings with " +
			"list_budgets first. The person sees the ceiling now and the new one, and approves " +
			"it before anything is saved, exactly as their own Save on Budgets. Use this for " +
			"any request to change a budget or a ceiling; the Budgets page's " +
			"budgets.ceiling.prefill only fills the editor in for the person to save " +
			"themselves, when they ask for that.",
		schema: object([]string{"window"}, map[string]*schema{
			"seat": text("The seat's handle or name, as list_budgets or list_agents names it. "+
				"Leave it out for the company's own ceiling.", nameBytes),
			"window": oneOf("The window the ceiling is for.", budgetWindows...),
			"tokens": {kind: "integer", description: "The new ceiling, a whole number of " +
				"tokens (1.5M is 1500000).", min: 1, max: maxCeiling, hasMin: true, hasMax: true},
			"clear": flag("Remove this window's ceiling instead of setting one."),
		}),
		Names:  map[string]Directory{"seat": DirSeat},
		Routes: []string{seatWriteRoute, companyWriteRoute},
		Tier:   DeriveTier(act, false),
		// OFFERED TO AN ADMINISTRATOR, whose every ceiling it reaches. A lead
		// may raise the ceilings of the seats they lead too, and the route
		// admits them, but no grant says who leads: offered by a grant, the
		// tool would be offered to every member for a change most cannot
		// make. A lead's change goes through the generic change, decided by
		// the same route, and through Budgets itself.
		Apps: adminOnly,
		Build: func(a map[string]any) (exec.Request, error) {
			args := Args(a)
			if _, err := ceilingOf(args); err != nil {
				return exec.Request{}, err
			}
			if seat := args.String("seat"); seat != "" {
				req, err := at(seatWriteRoute, map[string]string{"id": seat}, nil)
				if err != nil {
					return exec.Request{}, err
				}
				// THE BODY IS THE SEAT AS READ, which only the preview has:
				// a call built without it states nothing a PUT could keep.
				return req, nil
			}
			return at(companyWriteRoute, nil, nil)
		},
		Preview: budgetPreview,
		Label: func(a map[string]any) string {
			args := Args(a)
			adjective := windowAdjective[args.String("window")]
			if seat := args.String("seat"); seat != "" && len(seat) <= nameBytes {
				return "Preparing a change to " + seat + "'s " + adjective + " token budget"
			}
			return "Preparing a change to the company's " + adjective + " token budget"
		},
	}
}

// ceilingOf is the ceiling a call names: its tokens, or nil to remove it.
// Exactly one of the two.
func ceilingOf(args Args) (*int, error) {
	tokens, given := args.Int("tokens")
	switch {
	case given && args.Bool("clear"):
		return nil, fmt.Errorf("%w: give tokens or clear, not both", ErrInvalidArguments)
	case given:
		return &tokens, nil
	case args.Bool("clear"):
		return nil, nil
	}
	return nil, fmt.Errorf("%w: give the new ceiling as tokens, or clear: true to remove it",
		ErrInvalidArguments)
}

// budgetPreview reads the scope as it is now and renders the one window it
// changes, bound to the entity tag the read answered.
func budgetPreview(ctx context.Context, read proposal.Read, _ time.Time,
	a map[string]any) (proposal.Preview, error) {

	args := Args(a)
	ceiling, err := ceilingOf(args)
	if err != nil {
		return proposal.Preview{}, err
	}
	window := args.String("window")
	seat := args.String("seat")
	readRoute, writeRoute, wildcards := companyReadRoute, companyWriteRoute, map[string]string(nil)
	if seat != "" {
		readRoute, writeRoute = seatReadRoute, seatWriteRoute
		wildcards = map[string]string{"id": seat}
	}
	readReq, err := get(readRoute, wildcards, nil)
	if err != nil {
		return proposal.Preview{}, err
	}
	rep, err := readRepresentation(ctx, read, Route{Pattern: readRoute}, readReq.Path)
	switch {
	case err != nil:
		return proposal.Preview{}, err
	case rep.absent != nil:
		return proposal.Preview{}, rep.absent
	case rep.revision == "":
		return proposal.Preview{}, fmt.Errorf("%w: %s was answered without an entity tag to "+
			"bind the change to", ErrPreviewRead, readReq.Path)
	}
	doc, _ := rep.doc.(map[string]any)
	if doc == nil {
		return proposal.Preview{}, fmt.Errorf("%w: %s answered no object", ErrPreviewRead,
			readReq.Path)
	}
	held := heldCeiling(doc, window)
	if equalCeiling(held, ceiling) {
		return proposal.Preview{}, fmt.Errorf("%w: that is already the %s ceiling", ErrInvalidArguments,
			windowAdjective[window])
	}

	whose := "the company's"
	target := "The company"
	if seat != "" {
		name := strings.TrimSpace(cards.Text(doc["name"]))
		if name == "" {
			name = seat
		}
		whose, target = name+"'s", name
	}
	summary := ceilingSummary(whose, window, held, ceiling)

	// THE CHANGE, the request Budgets sends.
	var body map[string]any
	if seat != "" {
		body = seatWithCeiling(doc, window, ceiling)
	} else {
		var value any
		if ceiling != nil {
			value = *ceiling
		}
		body = map[string]any{"token_budget": map[string]any{window: value}}
	}
	req, err := at(writeRoute, wildcards, nil)
	if err != nil {
		return proposal.Preview{}, err
	}
	if req.Body, err = withSummary(body, summary); err != nil {
		return proposal.Preview{}, err
	}
	// THE ROUTE'S OWN CHECK, as every write preview asks it ([checked]).
	warnings, err := checked(ctx, read, req, rep.revision)
	if err != nil {
		return proposal.Preview{}, err
	}

	label := strings.ToUpper(whose[:1]) + whose[1:] + " " + windowAdjective[window] + " token budget"
	var c changes
	switch {
	case held == nil:
		c = append(c, proposal.Added("token_budget."+window, label, tokensWords(*ceiling), false))
	case ceiling == nil:
		c = append(c, proposal.Removed("token_budget."+window, label, tokensWords(*held)))
	default:
		c = append(c, proposal.Changed("token_budget."+window, label, tokensWords(*held),
			tokensWords(*ceiling)))
	}
	for i, warning := range warnings {
		c.fact("check.warning."+strconv.Itoa(i+1), "Warning", warning)
	}
	c.fact("request.summary", "History note", summary)

	recency, _ := authz.RecencyOf(authz.ActionOrgWrite)
	title := "Change a token budget"
	if ceiling == nil {
		title = "Remove a token budget"
	}
	return proposal.Preview{Title: title, Target: target,
		Note: "Turns are counted against the new ceiling once this node applies it; " +
			"nothing already spent is reset.",
		ApproveLabel: "Approve and save", Changes: c.rows(), Request: req,
		StepUp:         recency == iam.RecencyStepUp,
		SourceRevision: rep.revision}, nil
}

// heldCeiling is a scope's ceiling for window as the document holds it, or nil
// for none.
func heldCeiling(doc map[string]any, window string) *int {
	budget, _ := doc["token_budget"].(map[string]any)
	n, ok := number(budget[window])
	if !ok {
		return nil
	}
	v := int(n)
	return &v
}

func equalCeiling(a, b *int) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// seatWithCeiling is the seat as read with window's ceiling changed, the body
// its PUT takes back: everything else as read, masks included, and no
// `token_budget` at all once it caps nothing, as the engine writes a seat
// nobody capped (the dashboard's seatWithCeilings, the same rule).
func seatWithCeiling(seat map[string]any, window string, ceiling *int) map[string]any {
	out := clone(seat).(map[string]any)
	budget := map[string]any{}
	if held, ok := out["token_budget"].(map[string]any); ok {
		for k, v := range held {
			budget[k] = v
		}
	}
	if ceiling == nil {
		delete(budget, window)
	} else {
		budget[window] = *ceiling
	}
	if len(budget) == 0 {
		delete(out, "token_budget")
	} else {
		out["token_budget"] = budget
	}
	return out
}

// withSummary is a write's body with its revision summary in it, encoded.
func withSummary(body map[string]any, summary string) ([]byte, error) {
	doc := make(map[string]any, len(body)+1)
	for k, v := range body {
		doc[k] = v
	}
	doc[SummaryKey] = summary
	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("catalog: encode the change: %w", err)
	}
	return encoded, nil
}

// ceilingSummary is the history line: what the change does, in the words
// Budgets writes its own.
func ceilingSummary(whose, window string, held, ceiling *int) string {
	name := whose + " " + windowAdjective[window] + " token budget"
	var line string
	switch {
	case ceiling == nil:
		line = "remove " + name + " (was " + tokensWords(*held) + ")"
	case held == nil:
		line = "set " + name + " to " + tokensWords(*ceiling)
	case *ceiling > *held:
		line = "raise " + name + " from " + tokensWords(*held) + " to " + tokensWords(*ceiling)
	default:
		line = "lower " + name + " from " + tokensWords(*held) + " to " + tokensWords(*ceiling)
	}
	return strings.ToUpper(line[:1]) + line[1:]
}

// tokensWords is a count of tokens as a person reads it: 1,500,000 tokens.
func tokensWords(n int) string {
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if n == 1 {
		return b.String() + " token"
	}
	return b.String() + " tokens"
}
