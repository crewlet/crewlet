package configapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// Every seat and assistant field running on an entry names it, so removing it
// alone leaves a document validation refuses. The removal and the repoint of
// every dependant are ONE write, addressed by IDENTITY (a seat's handle and
// job, or the assistant's chat), never a list index (a position
// moves with what is above it). Each reference
// ([config.Company.ProviderReferences], validation's too) must be answered; an
// unanswered one refuses the write naming every reference.

// WarningResolutionMoved is the kind of a [config.Warning] about a seat's job
// whose resolved model a write moves without a field naming the entry it
// moved from: Seat is the seat, Ref the job, From and To the chain's head
// before and after.
//
// A seat whose job names no entry runs on the fallback (`default`, else the
// first in `llm_order`), so removing that entry moves it unnamed. The route
// asks [phase.Resolve] of both documents; a dry run shows it.
const WarningResolutionMoved = "resolution_moved"

// RemovalAt addresses one written reference by identity: a seat's handle and
// the job its field points at a model, the assistant's `chat` (its `model`),
// the Advisor's `findings`, or a fleet by its key (its `members`).
type RemovalAt struct {
	Seat      string `json:"seat,omitempty"`
	Job       string `json:"job,omitempty"`
	Assistant string `json:"assistant,omitempty"`
	Advisor   string `json:"advisor,omitempty"`
	Fleet     string `json:"fleet,omitempty"`
}

// AssistantChat is the one assistant target a [RemovalAt] can name.
const AssistantChat = "chat"

// AdvisorFindings is the one Advisor target a [RemovalAt] can name: the model
// it explains its findings with.
const AdvisorFindings = "findings"

// Repoint is what one reference to the removed entry becomes: To replaces the
// entry in the field's chain where it stood, and nil removes it, a chain left
// empty deleting the field so the job resolves as an unset one does. Ask
// Crewlet's model repointed to nil turns Ask Crewlet off; a fleet's member
// repointed to nil leaves the fleet.
type Repoint struct {
	At RemovalAt `json:"at"`
	To *string   `json:"to"`
}

// RemovalRequest is the body `DELETE /config/llm-providers/{id}` and
// `DELETE /config/llm-fleets/{id}` read; a fleet's references are repointed
// to `fleet:<key>`, an entry, or null.
type RemovalRequest struct {
	Repoint []Repoint `json:"repoint"`
	SecretsRequest
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
	config.JobTriage:    "sorting messages to a busy seat",
	config.JobAssistant: "Ask Crewlet's chat",
	config.JobAdvisor:   "the Advisor's findings",
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
	{phase.Triage, config.JobTriage},
}

// addressOf is where a reference is answered.
func addressOf(ref config.ProviderReference) RemovalAt {
	if ref.Fleet != "" {
		return RemovalAt{Fleet: ref.Fleet}
	}
	switch ref.Job {
	case config.JobAssistant:
		return RemovalAt{Assistant: AssistantChat}
	case config.JobAdvisor:
		return RemovalAt{Advisor: AdvisorFindings}
	}
	return RemovalAt{Seat: ref.Seat, Job: string(ref.Job)}
}

// readRemoval reads a removal's body strictly; an empty body is a removal
// with no repoint, which a dry run of an entry with dependants answers with
// the list of them. It answers the body's `_summary` beside the removal, and
// refuses a field named twice.
func readRemoval(text []byte) (RemovalRequest, string, error) {
	var body struct {
		RemovalRequest
		// Summary is the body's `_summary`, the channel a caller with no
		// headers of its own (the assistant's proposed change) states the
		// history line in, as every other config write takes it.
		Summary string `json:"_summary"`
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return RemovalRequest{}, "", nil
	}
	decoder := json.NewDecoder(bytes.NewReader(text))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return RemovalRequest{}, "", fmt.Errorf("the body is not {\"repoint\": [...], "+
			"\"delete_secrets\": [...]}: %w", err)
	}
	req, summary := body.RemovalRequest, strings.TrimSpace(body.Summary)
	if err := req.check(); err != nil {
		return RemovalRequest{}, "", err
	}
	// ONE ANSWER PER REFERENCE: a field named twice would move to whichever
	// came last, a choice the caller never saw made.
	first := map[RemovalAt]int{}
	for i, r := range req.Repoint {
		if j, seen := first[r.At]; seen {
			return RemovalRequest{}, "", fmt.Errorf("repoint[%d].at names the same field as "+
				"repoint[%d]: answer each reference once", i, j)
		}
		first[r.At] = i
	}
	for i, r := range req.Repoint {
		seat := r.At.Seat != "" || r.At.Job != ""
		others := 0
		for _, named := range []string{r.At.Assistant, r.At.Advisor, r.At.Fleet} {
			if named != "" {
				others++
			}
		}
		job := config.ProviderJob(r.At.Job)
		switch {
		case (seat && others > 0) || others > 1:
			return RemovalRequest{}, "", fmt.Errorf("repoint[%d].at names more than one of "+
				"a seat, Ask Crewlet, the Advisor and a fleet: address one", i)
		case seat && (r.At.Seat == "" || !job.Valid() ||
			job == config.JobAssistant || job == config.JobAdvisor):
			return RemovalRequest{}, "", fmt.Errorf("repoint[%d].at needs a seat and its job, "+
				"one of executor, review, subagent, auxiliary, judge or triage", i)
		case !seat && r.At.Fleet == "" && r.At.Assistant != AssistantChat && r.At.Advisor != AdvisorFindings:
			return RemovalRequest{}, "", fmt.Errorf("repoint[%d].at needs {seat, job}, "+
				"{assistant: chat}, {advisor: findings} or {fleet: <key>}", i)
		}
	}
	return req, summary, nil
}

// removalDraft removes the entry id and repoints every reference to it, as one
// revision built on expect.
func removalDraft(id string, req RemovalRequest, expect string) draft {
	return draft{
		expect: expect, requireActive: true,
		rules: authoredRules, authored: true,
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

// referencesOf is every written reference to the model id in c, as a
// removal's refusal lists them and its repoint answers them.
func referencesOf(c *config.Company, id string) []RemovalReference {
	references := []RemovalReference{}
	for _, ref := range c.ProviderReferences(id) {
		references = append(references, RemovalReference{At: addressOf(ref),
			Segments: ref.Segments, Chain: slices.Clone(ref.Chain)})
	}
	return references
}

// modelReferences serves `GET /config/llm-providers/{id}/references`: every
// written reference to the model, read with no write asked. A removal's page
// draws its list from it; asked as a dry run with no repoint, the same list
// came back as a 422, which every browser logs as a failed request.
func (s *Service) modelReferences(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	company, revision, err := s.activeCompany(r.Context())
	switch {
	case errors.Is(err, ErrNoActiveRevision):
		refuseNoCompany(w)
		return
	case err != nil:
		s.fail(w, r, "read the active revision", err)
		return
	}
	if _, held := company.Providers.LLM[id]; !held {
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{"hint": NotFound(EntityLLMProviders, id)})
		return
	}
	values, err := s.removalSecrets(r.Context(), company.VariablesRead(), modelReadsAfter(company, id))
	if err != nil {
		s.fail(w, r, "read which values the credential store holds", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"revision": revision.ID, "references": referencesOf(company, id), "dependants": values,
	})
}

// modelReadsAfter is every variable c reads once the model id is gone. Its
// repoints name no variable, so the dry run and the references read share it.
func modelReadsAfter(c *config.Company, id string) []config.Reference {
	next := *c
	next.Providers.LLM = maps.Clone(c.Providers.LLM)
	delete(next.Providers.LLM, id)
	return next.VariablesRead()
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
	for i, r := range req.Repoint {
		if !answered[r.At] {
			// AT THE REPOINT ITSELF, by its index in the body, so a page
			// marks the row that answers nothing rather than the whole list.
			path := config.Path{"repoint", i, "at"}
			problems = append(problems, config.Problem{Path: path.String(), Segments: path,
				Kind: "unknown_value", Seat: r.At.Seat,
				Message: fmt.Sprintf("%s names no field of %s: nothing there to repoint", describeAt(r.At), id)})
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
	if ref.Fleet != "" {
		return "the fleet " + ref.Fleet
	}
	if ref.Seat == "" {
		return jobWords[ref.Job]
	}
	return ref.Seat + "'s " + jobWords[ref.Job]
}

// describeAt is an address in a person's words.
func describeAt(at RemovalAt) string {
	if at.Fleet != "" {
		return "the fleet " + at.Fleet
	}
	if at.Assistant != "" {
		return "Ask Crewlet's " + strings.ReplaceAll(at.Assistant, "_", " ")
	}
	if at.Advisor != "" {
		return "the Advisor's " + at.Advisor
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
	// A FLEET'S MEMBERS, edited as a chain: the entry replaced where it
	// stood, or dropped from the fleet.
	if ref.Fleet != "" {
		fleet := c.Providers.LLMFleets[ref.Fleet]
		fleet.Members = edit(fleet.Members)
		c.Providers.LLMFleets[ref.Fleet] = fleet
		return
	}
	if ref.Job == config.JobAssistant {
		c.Assistant.Model = ""
		if to != nil {
			c.Assistant.Model = *to
		}
		return
	}
	// THE ADVISOR'S MODEL moves with the removal, or is cleared: its checks
	// run with no model.
	if ref.Job == config.JobAdvisor {
		c.Advisor.Model = ""
		if to != nil {
			c.Advisor.Model = *to
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
		case last == "llm_triage":
			role.LLMTriage = edit(role.LLMTriage)
		case ref.Job == config.JobReview:
			role.LLM.Review = edit(role.LLM.Review)
		case ref.Job == config.JobSubagent:
			role.LLM.Subagent = edit(role.LLM.Subagent)
		case ref.Job == config.JobAuxiliary:
			role.LLM.Auxiliary = edit(role.LLM.Auxiliary)
		case ref.Job == config.JobJudge:
			role.LLM.Judge = edit(role.LLM.Judge)
		case ref.Job == config.JobTriage:
			role.LLM.Triage = edit(role.LLM.Triage)
		}
		return
	}
}

// resolutionMoves is the warnings of a removal of id: every agent seat's job
// whose resolved chain the write moves although no field of it named id, so
// the company's fallback was what it ran on. Resolution is [phase.Resolve]'s,
// asked of both documents. A job that stays on its fleet is the fleet's repoint.
func resolutionMoves(id string) func(next, prior *config.Company) []config.Warning {
	return func(next, prior *config.Company) []config.Warning {
		if prior == nil {
			return nil
		}
		written := map[RemovalAt]bool{}
		for _, ref := range prior.ProviderReferences(id) {
			written[addressOf(ref)] = true
		}
		before, after := prior.Providers.PhaseModels(), next.Providers.PhaseModels()
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
				from := phase.Resolve(role.Seat(), pj.phase, before)
				to := phase.Resolve(moved, pj.phase, after)
				if slices.Equal(from.Keys, to.Keys) || !slices.Contains(from.Keys, id) {
					continue
				}
				if from.Source == phase.SourceSeat || from.Source == phase.SourceExecutor ||
					(from.Fleet != "" && from.Fleet == to.Fleet) {
					// THE SEAT CHOSE IT, in a field the write answers: a job
					// that runs on its seat's executor chain moves with the
					// executor's repoint, and one on a fleet with the fleet's,
					// moves the caller wrote.
					continue
				}
				head, message := "", fmt.Sprintf("%s's %s will have no model to run on: "+
					"the company configures none", seat, jobWords[pj.job])
				if len(to.Keys) > 0 {
					head = to.Keys[0]
					message = fmt.Sprintf("%s's %s will run on %s", seat, jobWords[pj.job], head)
				}
				out = append(out, config.Warning{
					Kind: WarningResolutionMoved, Ref: string(pj.job), Seat: seat,
					Path: path.String(), Segments: path,
					From: from.Keys[0], To: head, Message: message,
				})
			}
		}
		return out
	}
}

// THE SKELETON EVERY REMOVAL SHARES.
//
// A collection whose members something else names is removed by address, one
// route per collection: `DELETE /config/<kind>/{id}`, its body read strictly,
// held to the entry's own If-Match, summarised, tried again while other writes
// move the revision, and checked by `?dry_run=true`, deleting the stored values
// its entry alone read where asked (removalsecrets.go). What differs between two
// collections is what their body says, the write it drafts, and what only it
// answers: a [removalRoute] carries that, and [Service.deleteEntity] is the rest.

// removalRoute is one collection's removal: how its body is read, and the words
// only it has.
type removalRoute struct {
	// kind is the collection, as its URL segment names it.
	kind string
	// what is one member in a sentence: "a model"; noun names one: "model".
	what, noun string
	// read reads the removal of id from its body strictly, answering it and
	// the body's `_summary`. An empty body is a removal that answers nothing,
	// which a dry run of an entry with dependants answers with the list of
	// them.
	read func(s *Service, id string, text []byte) (removing, string, error)
	// bodyHint is what a body that does not read is told to send.
	bodyHint string
	// summaryHint is what a removal sent with no summary is told to name.
	summaryHint string
	// logAs is the log attribute naming the removed entry.
	logAs string
}

// removing is the removal of one entry, read from its body.
type removing interface {
	// deleting is the stored values the request names to delete.
	deleting() []string
	// draft is the removal, built on the revision expect.
	draft(expect string) draft
	// refuse answers a refusal only this removal makes, reporting whether
	// err was one.
	refuse(w http.ResponseWriter, r *http.Request, err error) bool
	// readsAfter is every variable prior reads once the removal is written,
	// at prior's paths ([entryReads]).
	readsAfter(prior *config.Company) ([]config.Reference, error)
	// checked is what a dry run that passed answers beside the check.
	checked(ctx context.Context, p *prepared, values RemovalSecrets) (map[string]any, error)
	// written does what the write leaves to do before its values go, and
	// answers beside the write.
	written(ctx context.Context, p *prepared, by iam.Actor) map[string]any
}

// removalRoutes is every collection removed by address.
var removalRoutes = []removalRoute{modelRemoval, fleetRemoval, serverRemoval}

// modelRemoval is `DELETE /config/llm-providers/{id}`.
var modelRemoval = removalRoute{
	kind: EntityLLMProviders, what: "a model", noun: "model",
	read: func(_ *Service, id string, text []byte) (removing, string, error) {
		req, summary, err := readRemoval(text)
		return providerRemoval{id: id, req: req}, summary, err
	},
	bodyHint: "name every field that runs on the model by its seat and job, or " +
		"Ask Crewlet's chat, with the model it moves to " +
		"(null to leave it unset), and \"delete_secrets\" with the stored values it read " +
		"that should go with it",
	summaryHint: "name the model removed and where its work moves",
	logAs:       "provider",
}

// providerRemoval is a model's removal, read: the entry's key, the repoint of
// every reference to it, and the values to delete.
type providerRemoval struct {
	id  string
	req RemovalRequest
}

func (m providerRemoval) deleting() []string { return m.req.DeleteSecrets }

func (m providerRemoval) draft(expect string) draft { return removalDraft(m.id, m.req, expect) }

func (providerRemoval) refuse(w http.ResponseWriter, _ *http.Request, err error) bool {
	var refused *RemovalError
	if !errors.As(err, &refused) {
		return false
	}
	refuseRemoval(w, refused)
	return true
}

func (m providerRemoval) readsAfter(prior *config.Company) ([]config.Reference, error) {
	return modelReadsAfter(prior, m.id), nil
}

func (providerRemoval) checked(_ context.Context, _ *prepared, values RemovalSecrets) (map[string]any, error) {
	return map[string]any{"dependants": values}, nil
}

func (providerRemoval) written(context.Context, *prepared, iam.Actor) map[string]any { return nil }

// deleteEntity serves one collection's `DELETE /config/<kind>/{id}`: the entry
// removed, with what its route takes with it, in one revision, or with
// `dry_run=true` the same request checked and stored nowhere.
func (s *Service) deleteEntity(route removalRoute) http.HandlerFunc {
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
		req, summary, err := route.read(s, id, body)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
				"detail": err.Error(), "hint": route.bodyHint,
			})
			return
		}
		if header := strings.TrimSpace(r.Header.Get("X-Summary")); header != "" {
			// THE HEADER WINS, as [takeSummary] has it on every route that
			// reads a document.
			summary = header
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
		// DELETING A VALUE ASKS `secrets.write` beyond the route's verb, decided
		// once and before anything is read, so nothing is written when it is refused.
		if !admitSecretDeletion(w, r, req.deleting()) {
			return
		}
		var raced error
		for range entityAttempts {
			done, err := s.removeEntity(w, r, route, removal{id: id, req: req,
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
		s.refuseApply(w, r, raced)
	}
}

// removal is one removal, as every attempt makes it.
type removal struct {
	id        string
	req       removing
	summary   string
	dryRun    bool
	expected  string
	principal *iam.Principal
}

// removeEntity makes one attempt: it reads the fleet's revision now, decides
// the entry's precondition against it, and writes the removal onto it. done
// reports that the request has been answered; a [*RacedError] is a revision
// that moved meanwhile, which the caller tries again.
func (s *Service) removeEntity(w http.ResponseWriter, r *http.Request, route removalRoute,
	e removal) (done bool, err error) {

	ctx := r.Context()
	cur, found, err := s.current(ctx)
	if err != nil {
		s.fail(w, r, "read the fleet's revision", err)
		return true, nil
	}
	if !found {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{"hint": "the fleet runs no company to remove " + route.what + " from"})
		return true, nil
	}
	active := cur.revision
	company, err := s.open(cur)
	if err != nil {
		if !RefuseUnreadable(w, err) {
			s.fail(w, r, "open the active revision", err)
		}
		return true, nil
	}
	current, held := entityKinds[route.kind].find(company.Redact(), e.id)
	if !held {
		// NOTHING TO REMOVE, said as that: the entity routes' 404 tells a
		// PUT how to add what it named, which a removal never wants.
		httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNoSuchEntity,
			map[string]string{"hint": NotFound(route.kind, e.id) + ", so there is nothing to remove"})
		return true, nil
	}
	// THE PRECONDITION IS THE ENTRY'S, as an edit's is: what its GET served.
	if e.expected != "" {
		tag, tagErr := entityTag(route.kind, current)
		if tagErr != nil {
			s.fail(w, r, "tag the entity", tagErr)
			return true, nil
		}
		if !matchesTag(e.expected, tag) {
			w.Header().Set("ETag", tag)
			httpjson.FailWithFields(w, http.StatusPreconditionFailed,
				httpjson.CodeVersionMoved, httpjson.Detail{
					"current": current, "etag": tag,
					"hint": route.kind + "/" + e.id + " changed after you read it; read it " +
						"again and decide the removal on top of it",
				})
			return true, nil
		}
	}
	d := e.req.draft(active.ID)
	d.principal = e.principal
	d.by = attributionOf(r)
	prepared, err := s.prepare(ctx, d)
	var lost *RacedError
	switch {
	case errors.As(err, &lost):
		return false, err
	case err != nil && e.req.refuse(w, r, err):
		return true, nil
	case err != nil:
		s.refuseRemovalApply(w, r, route.kind, e.id, err)
		return true, nil
	}
	before := prepared.prior.VariablesRead()
	after, err := e.req.readsAfter(prepared.prior)
	if err != nil {
		s.fail(w, r, "read what the removal leaves the document reading", err)
		return true, nil
	}
	named := fmt.Sprintf("the %s %q", route.noun, e.id)
	if problems := secretProblems(e.req.deleting(), before, after, named); len(problems) > 0 {
		refuseSecretDeletion(w, problems)
		return true, nil
	}
	if e.dryRun {
		if err = s.checks(prepared, e.summary); err != nil {
			s.refuseApply(w, r, err)
			return true, nil
		}
		values, readErr := s.removalSecrets(ctx, before, after)
		if readErr != nil {
			s.fail(w, r, "read which values the credential store holds", readErr)
			return true, nil
		}
		extra, readErr := e.req.checked(ctx, prepared, values)
		if readErr != nil {
			s.fail(w, r, "read what the removal takes with it", readErr)
			return true, nil
		}
		answer := checkedAnswer(prepared)
		maps.Copy(answer, extra)
		writeJSON(w, http.StatusOK, answer)
		return true, nil
	}
	if !requireSummary(w, e.summary, "a removal needs an audit summary in the "+
		"X-Summary header or the body's `_summary`: "+route.summaryHint) {
		return true, nil
	}
	applied, err := s.commit(ctx, prepared, e.summary)
	switch {
	case errors.As(err, &lost):
		return false, err
	case err != nil:
		s.refuseApply(w, r, err)
		return true, nil
	}
	by := attributionOf(r)
	answer := appliedAnswer(applied)
	maps.Copy(answer, e.req.written(ctx, prepared, by))
	answer["secrets"] = s.deleteSecrets(ctx, e.req.deleting(), by, route.logAs, e.id)
	writeJSON(w, http.StatusCreated, answer)
	return true, nil
}

// refuseSecretDeletion answers values a removal may not delete: 422, each at
// its index in `delete_secrets`.
func refuseSecretDeletion(w http.ResponseWriter, problems []config.Problem) {
	messages := make([]string, 0, len(problems))
	for _, p := range problems {
		messages = append(messages, p.Message)
	}
	httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
		httpjson.Detail{
			"detail": "configapi: " + strings.Join(messages, "; "), "problems": problems,
			"hint": "delete_secrets may name only values a dry run lists under " +
				"dependants.secrets or dependants.kept_secrets",
		})
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
			"hint": "answer every reference in repoint, by its seat and job, " +
				"Ask Crewlet's chat, the Advisor's findings or a fleet, with the " +
				"model or fleet it moves to or null",
		})
}

// refuseRemovalApply answers a removal the document refused: 422 with the
// problems for a company the removal leaves invalid (an executor job moved to
// a harness that cannot run one, an assistant target that allows web tools),
// the entity's own refusals, and every other as any write's.
func (s *Service) refuseRemovalApply(w http.ResponseWriter, r *http.Request, kind, id string, err error) {
	var invalid *ValidationError
	var entityErr *EntityError
	switch {
	case errors.As(err, &invalid):
		fields := RefusalFields(err)
		fields["detail"] = invalid.Err.Error()
		fields["hint"] = "The whole company is checked on every removal, so a " +
			"change it makes that the company cannot run is refused at its field."
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError, fields)
	case errors.As(err, &entityErr):
		s.refuseEntity(w, r, kind, id, err)
	default:
		s.refuseApply(w, r, err)
	}
}
