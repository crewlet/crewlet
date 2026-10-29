package configapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// Removing a model: `DELETE /config/llm-providers/{id}`.
//
// # One revision, every dependant repointed in it
//
// A providers.llm entry is named by every seat field and assistant field that
// runs on it, so removing it alone leaves a document validation refuses: a seat
// naming a key that is gone. The route therefore takes the removal and the
// repoint of every dependant as ONE write, addressed by IDENTITY (a seat's
// handle and the job the field points at a model, or the assistant's chat or
// deep work), never by a list index, for the reason the entity routes give: a
// position moves when anything above it does, and a repoint by index would
// rewrite another seat's field. Each written reference
// ([config.Company.ProviderReferences], the one implementation validation
// reads too) has to be answered, and a reference left unanswered refuses the
// write naming it, with the list of every reference, so a page can draw the
// repoint list from the refusal.
//
// # A move nobody wrote is a warning
//
// A seat whose job names no entry runs on the company's fallback, the entry
// keyed `default` or else the first in `llm_order`, so removing that entry
// moves the job although no field named it. Resolution has one implementation,
// [phase.ResolveWithSource]; the route asks it of both documents and says each
// such move as a `resolution_moved` warning, which a dry run shows before the
// write ("pm's memory and summaries will run on aux").

// WarningResolutionMoved is the kind of a [config.Warning] about a seat's job
// whose resolved model a write moves without a field naming the entry it
// moved from: Seat is the seat, Ref the job, From and To the chain's head
// before and after.
const WarningResolutionMoved = "resolution_moved"

// RemovalAt addresses one written reference by identity: a seat's handle and
// the job its field points at a model, or the assistant's `chat` (its `llm`)
// or `deep_work`.
type RemovalAt struct {
	Seat      string `json:"seat,omitempty"`
	Job       string `json:"job,omitempty"`
	Assistant string `json:"assistant,omitempty"`
}

// Assistant targets a [RemovalAt] can name.
const (
	AssistantChat     = "chat"
	AssistantDeepWork = "deep_work"
)

// Repoint is what one reference to the removed entry becomes: To replaces the
// entry in the field's chain where it stood, and nil removes it, a chain left
// empty deleting the field so the job resolves as an unset one does.
type Repoint struct {
	At RemovalAt `json:"at"`
	To *string   `json:"to"`
}

// RemovalRequest is the body `DELETE /config/llm-providers/{id}` reads.
type RemovalRequest struct {
	Repoint []Repoint `json:"repoint"`
	// AssistantOff turns the assistant off when the removal leaves its
	// chat with no model: `assistant.enabled: false` in the same revision.
	AssistantOff bool `json:"assistant_off"`
}

// RemovalReference is one written reference to the entry, as a refusal names
// it so a page can draw its repoint list: its address, the field it is written
// in and the whole chain that field writes.
type RemovalReference struct {
	At       RemovalAt   `json:"at"`
	Segments config.Path `json:"segments"`
	Chain    []string    `json:"chain"`
}

// RemovalError is a removal whose repoints do not answer every reference, or
// answer one that is not there: the problems, each located at the field, and
// every reference the entry has.
type RemovalError struct {
	Problems   []config.Problem
	References []RemovalReference
}

func (e *RemovalError) Error() string {
	lines := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		lines = append(lines, p.Message)
	}
	return "configapi: " + strings.Join(lines, "; ")
}

// jobWords is how a job reads in a sentence a person is shown.
var jobWords = map[config.ProviderJob]string{
	config.JobExecutor:  "coding",
	config.JobReview:    "reviewing work",
	config.JobSubagent:  "delegated tasks",
	config.JobAuxiliary: "memory and summaries",
	config.JobJudge:     "onboarding checks",
	config.JobAssistant: "the assistant's chat",
	config.JobDeepWork:  "the assistant's deep work",
}

// phaseJobs is the job each phase's chain is resolved for, the phases a
// written reference can name: onboarding runs on its seat's executor chain
// and is reported as that.
var phaseJobs = []struct {
	phase phase.Phase
	job   config.ProviderJob
}{
	{phase.Execute, config.JobExecutor},
	{phase.Review, config.JobReview},
	{phase.Subagent, config.JobSubagent},
	{phase.Auxiliary, config.JobAuxiliary},
	{phase.Judge, config.JobJudge},
}

// addressOf is where a reference is answered.
func addressOf(ref config.ProviderReference) RemovalAt {
	switch ref.Job {
	case config.JobAssistant:
		return RemovalAt{Assistant: AssistantChat}
	case config.JobDeepWork:
		return RemovalAt{Assistant: AssistantDeepWork}
	}
	return RemovalAt{Seat: ref.Seat, Job: string(ref.Job)}
}

// readRemoval reads a removal's body strictly; an empty body is a removal
// with no repoint, which a dry run of an entry with dependants answers with
// the list of them.
func readRemoval(text []byte) (RemovalRequest, error) {
	var req RemovalRequest
	if len(bytes.TrimSpace(text)) == 0 {
		return req, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return RemovalRequest{}, fmt.Errorf("the body is not {\"repoint\": [...], "+
			"\"assistant_off\": false}: %w", err)
	}
	for i, r := range req.Repoint {
		seat := r.At.Seat != "" || r.At.Job != ""
		switch {
		case seat && r.At.Assistant != "":
			return RemovalRequest{}, fmt.Errorf("repoint[%d].at names a seat and the "+
				"assistant: address one", i)
		case seat && (r.At.Seat == "" || !config.ProviderJob(r.At.Job).Valid() ||
			config.ProviderJob(r.At.Job) == config.JobAssistant ||
			config.ProviderJob(r.At.Job) == config.JobDeepWork):
			return RemovalRequest{}, fmt.Errorf("repoint[%d].at needs a seat and its job, "+
				"one of executor, review, subagent, auxiliary or judge", i)
		case !seat && r.At.Assistant != AssistantChat && r.At.Assistant != AssistantDeepWork:
			return RemovalRequest{}, fmt.Errorf("repoint[%d].at needs {seat, job} or "+
				"{assistant: chat | deep_work}", i)
		}
	}
	return req, nil
}

// removalDraft removes the entry id and repoints every reference to it, as one
// revision built on expect.
func removalDraft(id string, req RemovalRequest, expect string) draft {
	return draft{
		expect: expect, requireActive: true,
		rules: authoredRules,
		build: func(b base) (*config.Company, []byte, error) {
			next, err := config.DecodeCompany(b.document)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: decode the active revision: %w", err)
			}
			if _, ok := next.Providers.LLM[id]; !ok {
				return nil, nil, &EntityError{Err: ErrNoSuchEntity}
			}
			if refused := repoint(next, id, req); refused != nil {
				return nil, nil, refused
			}
			delete(next.Providers.LLM, id)
			next.Providers.LLMOrder = slices.DeleteFunc(next.Providers.ProviderOrder(),
				func(key string) bool { return key == id })
			document, err := json.Marshal(next)
			if err != nil {
				return nil, nil, fmt.Errorf("configapi: encode the config: %w", err)
			}
			// WHAT THIS BUILD CANNOT REPRESENT STAYS, as on every write of
			// the whole document: see unknown.go.
			if document, err = carryUnknown(b.document, document); err != nil {
				return nil, nil, err
			}
			return next, document, nil
		},
		warnings: resolutionMoves(id),
	}
}

// repoint answers every written reference to id in c with the request's
// repoints, refusing one left unanswered and one that answers nothing.
func repoint(c *config.Company, id string, req RemovalRequest) error {
	refs := c.ProviderReferences(id)
	answers := map[RemovalAt]*string{}
	for _, r := range req.Repoint {
		answers[r.At] = r.To
	}
	var references []RemovalReference
	var problems []config.Problem
	answered := map[RemovalAt]bool{}
	for _, ref := range refs {
		at := addressOf(ref)
		references = append(references, RemovalReference{At: at, Segments: ref.Segments,
			Chain: slices.Clone(ref.Chain)})
		to, ok := answers[at]
		if !ok {
			problems = append(problems, config.Problem{
				Path: ref.Segments.String(), Segments: ref.Segments, Kind: "conflict",
				Seat: ref.Seat,
				Message: fmt.Sprintf("%s still names %s: repoint it or leave it unset in "+
					"the same write", whose(ref), id),
			})
			continue
		}
		answered[at] = true
		if to != nil && *to == id {
			problems = append(problems, config.Problem{Path: ref.Segments.String(),
				Segments: ref.Segments, Kind: "invalid", Seat: ref.Seat,
				Message: fmt.Sprintf("%s is repointed to %s, the entry this write removes", whose(ref), id)})
			continue
		}
		rewrite(c, ref, id, to)
	}
	for _, r := range req.Repoint {
		if !answered[r.At] {
			problems = append(problems, config.Problem{Path: "repoint", Segments: config.Path{"repoint"},
				Kind: "unknown_value", Seat: r.At.Seat,
				Message: fmt.Sprintf("%s names no field of %s: nothing there to repoint", describeAt(r.At), id)})
		}
	}
	// THE ASSISTANT'S CHAT LEFT WITH NO MODEL while it is on is a choice the
	// write has to make out loud: turn it off, or name another model.
	if c.Assistant.On() && len(c.Assistant.LLM) == 0 {
		if req.AssistantOff {
			off := false
			c.Assistant.Enabled = &off
		} else {
			path := config.Path{"assistant", "llm"}
			problems = append(problems, config.Problem{Path: path.String(), Segments: path,
				Kind: "missing",
				Message: "the assistant is on and this write leaves its chat with no model: " +
					"repoint it to another model, or send assistant_off to turn the assistant off"})
		}
	}
	if len(problems) > 0 {
		if references == nil {
			references = []RemovalReference{}
		}
		return &RemovalError{Problems: problems, References: references}
	}
	return nil
}

// whose is the reference in a person's words: "qa's reviewing work".
func whose(ref config.ProviderReference) string {
	if ref.Seat == "" {
		return jobWords[ref.Job]
	}
	return ref.Seat + "'s " + jobWords[ref.Job]
}

// describeAt is an address in a person's words.
func describeAt(at RemovalAt) string {
	if at.Assistant != "" {
		return "the assistant's " + strings.ReplaceAll(at.Assistant, "_", " ")
	}
	return at.Seat + "'s " + jobWords[config.ProviderJob(at.Job)]
}

// rewrite makes the field ref names answer to without id: to in id's place,
// each key once, or id removed; a chain left empty clears the field.
func rewrite(c *config.Company, ref config.ProviderReference, id string, to *string) {
	edit := func(chain config.ProviderKeys) config.ProviderKeys {
		var out config.ProviderKeys
		for _, key := range chain {
			if key == id {
				if to == nil {
					continue
				}
				key = *to
			}
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
		return out
	}
	switch ref.Job {
	case config.JobAssistant:
		c.Assistant.LLM = edit(c.Assistant.LLM)
		return
	case config.JobDeepWork:
		if to == nil {
			c.Assistant.DeepWork = nil
		} else if c.Assistant.DeepWork != nil {
			c.Assistant.DeepWork.LLM = *to
		}
		return
	}
	// THE FIELD THE SEGMENTS END IN, since a seat can write one job in two
	// places (`llm_review` and `llm.review`) and each is its own reference.
	last := ""
	if n := len(ref.Segments); n > 0 {
		last, _ = ref.Segments[n-1].(string)
	}
	for role := range c.EachRole() {
		if role.IdentityKey() != ref.Seat {
			continue
		}
		switch {
		case ref.Job == config.JobExecutor:
			role.LLM.Default = edit(role.LLM.Default)
		case last == "llm_review":
			role.LLMReview = edit(role.LLMReview)
		case last == "llm_subagent":
			role.LLMSubagent = edit(role.LLMSubagent)
		case last == "llm_auxiliary":
			role.LLMAuxiliary = edit(role.LLMAuxiliary)
		case last == "llm_judge":
			role.LLMJudge = edit(role.LLMJudge)
		case ref.Job == config.JobReview:
			role.LLM.Review = edit(role.LLM.Review)
		case ref.Job == config.JobSubagent:
			role.LLM.Subagent = edit(role.LLM.Subagent)
		case ref.Job == config.JobAuxiliary:
			role.LLM.Auxiliary = edit(role.LLM.Auxiliary)
		case ref.Job == config.JobJudge:
			role.LLM.Judge = edit(role.LLM.Judge)
		}
		return
	}
}

// resolutionMoves is the warnings of a removal of id: every agent seat's job
// whose resolved chain the write moves although no field of it named id, so
// the company's fallback was what it ran on. Resolution is
// [phase.ResolveWithSource]'s, asked of both documents.
func resolutionMoves(id string) func(next, prior *config.Company) []config.Warning {
	return func(next, prior *config.Company) []config.Warning {
		if prior == nil {
			return nil
		}
		written := map[RemovalAt]bool{}
		for _, ref := range prior.ProviderReferences(id) {
			written[addressOf(ref)] = true
		}
		before, after := prior.Providers.ProviderOrder(), next.Providers.ProviderOrder()
		nextRoles := map[string]*org.Role{}
		for role := range next.EachRole() {
			nextRoles[role.IdentityKey()] = role.Seat()
		}
		var out []config.Warning
		for role, path := range prior.EachRole() {
			if role.Kind == org.KindHuman {
				continue
			}
			seat := role.IdentityKey()
			moved, ok := nextRoles[seat]
			if !ok {
				continue
			}
			for _, pj := range phaseJobs {
				if written[RemovalAt{Seat: seat, Job: string(pj.job)}] {
					continue
				}
				from, _ := phase.ResolveWithSource(role.Seat(), pj.phase, before)
				to, _ := phase.ResolveWithSource(moved, pj.phase, after)
				if slices.Equal(from, to) || !slices.Contains(from, id) {
					continue
				}
				head := "nothing"
				if len(to) > 0 {
					head = to[0]
				}
				out = append(out, config.Warning{
					Kind: WarningResolutionMoved, Ref: string(pj.job), Seat: seat,
					Path: path.String(), Segments: path,
					From: from[0], To: head,
					Message: fmt.Sprintf("%s's %s will run on %s", seat, jobWords[pj.job], head),
				})
			}
		}
		return out
	}
}

// deleteEntity serves DELETE /config/llm-providers/{id}: the entry removed and
// every dependant repointed in one revision, or with `dry_run=true` the same
// request checked and stored nowhere.
func (s *Service) deleteEntity(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.refusedManaged(w, r) {
			return
		}
		id := r.PathValue("id")
		dryRun, ok := dryRunOf(w, r)
		if !ok {
			return
		}
		body, err := readBody(w, r)
		if err != nil {
			refuseBody(w, err)
			return
		}
		summary := strings.TrimSpace(r.Header.Get("X-Summary"))
		req, err := readRemoval(body)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
				"detail": err.Error(),
				"hint": "name every field that runs on the model by its seat and job, or " +
					"the assistant's chat or deep work, with the model it moves to " +
					"(null to leave it unset)",
			})
			return
		}
		expected, create, ok := entityPrecondition(w, r)
		if !ok {
			return
		}
		if create {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadIfMatch, map[string]string{
				"hint": "a removal takes If-Match with the ETag its GET answered",
			})
			return
		}
		principal, ok := principalOf(w, r)
		if !ok {
			return
		}
		var raced error
		for range entityAttempts {
			done, err := s.removeEntity(w, r, removal{kind: kind, id: id, req: req,
				summary: summary, dryRun: dryRun, expected: expected, principal: principal})
			var lost *RacedError
			if errors.As(err, &lost) {
				raced = err
				continue
			}
			if done {
				return
			}
		}
		s.refuseApply(w, raced)
	}
}

// removal is one removal, as every attempt makes it.
type removal struct {
	kind, id  string
	req       RemovalRequest
	summary   string
	dryRun    bool
	expected  string
	principal *iam.Principal
}

// removeEntity makes one attempt: it reads the fleet's revision now, decides
// the entry's precondition against it, and writes the removal onto it. done
// reports that the request has been answered; a [*RacedError] is a revision
// that moved meanwhile, which the caller tries again.
func (s *Service) removeEntity(w http.ResponseWriter, r *http.Request, e removal) (done bool, err error) {
	ctx := r.Context()
	cur, found, err := s.current(ctx)
	if err != nil {
		s.fail(w, "read the fleet's revision", err)
		return true, nil
	}
	if !found {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{"hint": "the fleet runs no company to remove a model from"})
		return true, nil
	}
	active := cur.revision
	company, err := s.open(active)
	if err != nil {
		if !RefuseUnreadable(w, err) {
			s.fail(w, "open the active revision", err)
		}
		return true, nil
	}
	current, held := entityKinds[e.kind].find(company.Redact(), e.id)
	if !held {
		s.refuseEntity(w, e.kind, e.id, &EntityError{Err: ErrNoSuchEntity})
		return true, nil
	}
	// THE PRECONDITION IS THE ENTRY'S, as an edit's is: what its GET served.
	if e.expected != "" {
		tag, tagErr := entityTag(e.kind, current)
		if tagErr != nil {
			s.fail(w, "tag the entity", tagErr)
			return true, nil
		}
		if !matchesTag(e.expected, tag) {
			w.Header().Set("ETag", tag)
			httpjson.FailWithFields(w, http.StatusPreconditionFailed,
				httpjson.CodeVersionMoved, httpjson.Detail{
					"current": current, "etag": tag,
					"hint": e.kind + "/" + e.id + " changed after you read it; read it " +
						"again and decide the removal on top of it",
				})
			return true, nil
		}
	}
	d := removalDraft(e.id, e.req, active.ID)
	d.principal = e.principal
	d.by = attributionOf(r)
	prepared, err := s.prepare(ctx, d)
	var lost *RacedError
	var refused *RemovalError
	switch {
	case errors.As(err, &lost):
		return false, err
	case errors.As(err, &refused):
		refuseRemoval(w, refused)
		return true, nil
	case err != nil:
		s.refuseRemovalApply(w, e.kind, e.id, err)
		return true, nil
	}
	if e.dryRun {
		writeChecked(w, prepared)
		return true, nil
	}
	if !requireSummary(w, e.summary, "a removal needs an audit summary in the "+
		"X-Summary header: name the model removed and where its work moves") {
		return true, nil
	}
	applied, err := s.commit(ctx, prepared, e.summary)
	switch {
	case errors.As(err, &lost):
		return false, err
	case err != nil:
		s.refuseApply(w, err)
		return true, nil
	}
	writeApplied(w, applied)
	return true, nil
}

// refuseRemoval answers repoints that do not answer the entry's references:
// 422, each problem at its field, and every reference, so a page draws the
// repoint list from the refusal.
func refuseRemoval(w http.ResponseWriter, e *RemovalError) {
	httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
		httpjson.Detail{
			"detail":     e.Error(),
			"problems":   e.Problems,
			"references": e.References,
			"hint": "answer every reference in repoint, by its seat and job or the " +
				"assistant's chat or deep work, with the model it moves to or null",
		})
}

// refuseRemovalApply answers a removal the document refused: 422 with the
// problems for a company the removal leaves invalid (an executor job moved to
// a CLI that cannot run one, an assistant target that allows web tools), the
// entity's own refusals, and every other as any write's.
func (s *Service) refuseRemovalApply(w http.ResponseWriter, kind, id string, err error) {
	var invalid *ValidationError
	var entityErr *EntityError
	switch {
	case errors.As(err, &invalid):
		fields := RefusalFields(err)
		fields["detail"] = invalid.Err.Error()
		fields["hint"] = "the WHOLE document the removal produces is validated, so a " +
			"repoint the company cannot run is refused at its field"
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError, fields)
	case errors.As(err, &entityErr):
		s.refuseEntity(w, kind, id, err)
	default:
		s.refuseApply(w, err)
	}
}
