package llmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
	"github.com/crewlet/crewlet/internal/secrets"
)

// CredentialBudget is the longest a candidate credential's test may take:
// the entry's own `timeout_seconds`, capped here. A one-turn run measures 3 to
// 10 s; 120 s outlasts a slow vendor and stays under the 300 s idle timeout
// common to proxies in front of the API, so the person's request is answered
// rather than cut.
const CredentialBudget = 120 * time.Second

// credentialRequest is the body `PUT /llm/providers/{key}/credential` reads.
type credentialRequest struct {
	Values map[string]string `json:"values"`
	// Entity is a draft of the entry: a create, or an edit that changes how
	// it signs in. Absent, the active entry is tested.
	Entity json.RawMessage `json:"entity"`
}

// CheckLine is one check as an answer renders it.
type CheckLine struct {
	Check  string `json:"check"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Kind   string `json:"kind,omitempty"`
	Remedy string `json:"remedy,omitempty"`
}

// CheckAnswer is a check's answer: when, on which node, whether it passed,
// the first failure's kind and every line.
type CheckAnswer struct {
	At      time.Time   `json:"at"`
	Node    string      `json:"node"`
	OK      bool        `json:"ok"`
	Failure string      `json:"failure,omitempty"`
	Lines   []CheckLine `json:"lines"`
}

// Limited is a vendor's limit a candidate credential met: the test passes,
// since a 429 proves the credential authenticated, and the answer says so.
type Limited struct {
	// Until is when the run's sit-out ends; null for a test's 429, which
	// sits nothing out and knows no end the vendor did not say.
	Until    *time.Time `json:"until"`
	ResetsAt *time.Time `json:"resets_at"`
	Window   string     `json:"window"`
	Said     string     `json:"said,omitempty"`
}

func answerOf(c Check) CheckAnswer {
	out := CheckAnswer{At: c.At, Node: c.Node, OK: c.OK, Failure: c.Failure,
		Lines: make([]CheckLine, 0, len(c.Lines))}
	for _, line := range c.Lines {
		out.Lines = append(out.Lines, CheckLine{Check: string(line.Name), OK: line.OK,
			Detail: line.Detail, Kind: line.Kind, Remedy: line.Remedy})
	}
	return out
}

func limitedOf(sit *coord.ModelSitOut) *Limited {
	if sit == nil {
		return nil
	}
	out := &Limited{Window: sit.Window, Said: sit.Said}
	if !sit.Until.IsZero() {
		until := sit.Until
		out.Until = &until
	}
	if !sit.ResetsAt.IsZero() {
		at := sit.ResetsAt
		out.ResetsAt = &at
	}
	return out
}

// credential serves PUT /llm/providers/{key}/credential: candidate values
// tested in an isolated home, sealed only if they sign the model in, and the
// revision that reads them re-activated.
//
// IN THE SETUP WRITER'S ORDER, each step for a reason:
//  1. The target: the active entry, or the draft the body carries.
//  2. Validation, every fault at once where it can be: the draft whole, as
//     the entity route's dry run is; each name, which the entry must read;
//     each value's kind, by the prefixes the vendor gives them.
//  3. `If-None-Match: *`, refusing a name already stored, so a create never
//     overwrites a credential another model reads.
//  4. `config.write` when a re-activation will follow, before anything is
//     tested or sealed.
//  5. The test: the CLI itself, with only these values.
//  6. With `dry_run=true`, the answer and nothing else.
//  7. The seal, then the one re-activation, held to `If-Match`.
func (s *Service) credential(w http.ResponseWriter, r *http.Request, rec *types.ModelActed) {
	ctx := r.Context()
	key := r.PathValue("key")
	dryRun, ok := flag(w, r, "dry_run")
	if !ok {
		return
	}
	rec.DryRun = dryRun
	create, ok := createOnly(w, r)
	if !ok {
		return
	}
	var req credentialRequest
	if !decode(w, r, &req, `{"values": {"NAME": "value"}, "entity": {...}}`) {
		return
	}
	names := slices.Sorted(maps.Keys(req.Values))
	if !checkValues(w, req.Values, names) {
		return
	}
	// RECORDED ONLY ONCE THEY ARE NAMES: a refused one may be a key pasted
	// into the name's place, and the audit is read by everyone who reads
	// the feed.
	rec.Names = names
	by := actor(r)

	company, active, ok := s.activeEntry(w, r, key)
	if !ok {
		return
	}
	draft := active
	if len(req.Entity) > 0 && string(req.Entity) != "null" {
		if !mayRunDraft(w, r, s.guard) {
			return
		}
		next, err := s.config.CheckEntity(ctx, configapi.EntityLLMProviders, key, req.Entity,
			active == nil, by)
		if !refuseDraft(w, err) {
			return
		}
		company = next
		entry := next.Providers.LLM[key]
		draft = &entry
	} else {
		req.Entity = nil
	}
	if draft == nil {
		notFound(w, key)
		return
	}
	if problems := nameProblems(company, key, draft, req.Values, names); len(problems) > 0 {
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
			httpjson.Detail{"problems": problems,
				"detail": problems[0].Message,
				"hint":   "send the values this model reads, each the kind its slot takes"})
		return
	}
	if create {
		if !s.refuseStored(w, r, names) {
			return
		}
	}
	readBy := map[string][]string{}
	if !dryRun {
		var err error
		if readBy, err = s.config.Reads(ctx, names); err != nil {
			s.fail(w, "read which paths read the credential", err)
			return
		}
		if len(readBy) > 0 && !authz.Admit(w, r, s.guard, authz.Policy{Action: authz.ActionConfigWrite}) {
			return
		}
	}

	// THE TEST, of the draft when the body carries one: a value is only
	// worth storing if the CLI the entry runs signs in with it.
	var testDraft *config.LLMProvider
	if req.Entity != nil {
		testDraft = draft
	}
	checked, err := s.engine.CheckModel(ctx, key, testDraft, req.Values, false)
	switch {
	case errors.Is(err, ErrNoSuchModel):
		notFound(w, key)
		return
	case err != nil:
		s.fail(w, "test the credential", err)
		return
	}
	if !s.passed(w, key, names, checked) {
		return
	}
	verified := fmt.Sprintf("Answered a one-turn %s run in %.1f s with only %s.",
		agentName(draft), checked.Elapsed.Seconds(), plural(len(names), "this value", "these values"))
	hints := checked.Hints
	if hints == nil {
		hints = map[string]string{}
	}
	if dryRun {
		httpjson.Write(w, http.StatusOK, map[string]any{
			"ok": true, "verified": verified, "hints": hints, "check": answerOf(checked),
			"limited": limitedOf(checked.Limited),
		})
		return
	}

	// PAST THE TEST, THE WRITE FINISHES whatever the caller does: a person
	// who closes the page after a two-minute test would otherwise leave
	// some values sealed and some not, and the revision not re-activated.
	ctx = context.WithoutCancel(ctx)

	// WHICH ENTRIES READ THESE NAMES, and which of them sat out here on the
	// sign-in this write replaces: read before anything is sealed.
	entries := entriesReading(company, names)
	cleared, err := s.sittingOutHere(ctx, entries)
	if err != nil {
		s.fail(w, "read this node's model report", err)
		return
	}
	author := secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
	sealed := make([]string, 0, len(names))
	for _, name := range names {
		if sealErr := s.secrets.Set(ctx, name, req.Values[name], author, secrets.SourceModels,
			s.now()); sealErr != nil {
			// SAID BY NAME, as `PUT /secrets` says a seal that stopped part
			// way: what is stored now, and that nothing was re-activated.
			log.ErrorContext(ctx, "llm_request_failed", "what", "seal the credential",
				"model", key, "sealed", sealed, "error", sealErr)
			httpjson.FailWithFields(w, http.StatusInternalServerError, httpjson.CodeInternalError,
				httpjson.Detail{"sealed": sealed,
					"detail": "The store refused a value part way. " + sealedSentence(sealed),
					"hint":   "send the same values again: the running company still reads the ones it read"})
			return
		}
		sealed = append(sealed, name)
		log.InfoContext(ctx, "model_credential_written", "model", key, "name", name,
			"bytes", len(req.Values[name]), "by", by.Name, "operator", by.OperatorID)
	}
	var reload *configapi.Reloaded
	if len(readBy) > 0 {
		applied, reloadErr := s.config.Reload(ctx, "rotate "+strings.Join(names, ", ")+" for "+key,
			by, strings.TrimSpace(r.Header.Get("If-Match")))
		reload = configapi.ReloadOutcome(applied, reloadErr)
		rec.RevisionID = applied.RevisionID
	}
	running, err := s.engine.RunningRunsOn(ctx, entries)
	if err != nil {
		// THE WRITE LANDED; only the count of runs still on the old value
		// is missing, which the answer says rather than fails over.
		log.WarnContext(ctx, "model_running_runs_unread", "model", key, "error", err)
		running = -1
	}
	answer := map[string]any{
		"verified": verified, "check": answerOf(checked), "names": names, "hints": hints,
		"entries": entries, "reload": reload, "cleared": cleared,
		"running_runs": running, "limited": limitedOf(checked.Limited),
	}
	if running < 0 {
		answer["running_runs"] = nil
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// passed answers a test that did not pass, and reports whether it did.
func (s *Service) passed(w http.ResponseWriter, key string, names []string, c Check) bool {
	switch c.Failure {
	case "", "rate_limit":
		// A 429 PROVES THE CREDENTIAL AUTHENTICATED: the vendor knew who
		// was asking and said they had spent their plan.
		return true
	case "auth":
		said := strings.TrimSuffix(strings.TrimSpace(c.Said), ".")
		message := "Nothing was stored, and the current credential still signs the model in."
		if said != "" {
			message = said + ". " + message
		}
		problems := make([]config.Problem, 0, len(names))
		for _, name := range names {
			path := config.Path{"values", name}
			problems = append(problems, config.Problem{Path: path.String(), Segments: path,
				Kind: "invalid", Message: message})
		}
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeCredentialRejected,
			httpjson.Detail{"detail": message, "problems": problems, "check": answerOf(c)})
	case "timeout", "server":
		httpjson.FailWithFields(w, http.StatusBadGateway, httpjson.CodeCredentialUntested,
			httpjson.Detail{"detail": "The vendor did not answer the test; nothing was stored. Try again.",
				"check": answerOf(c)})
	default:
		// THE CLI ITSELF, not the credential: no binary, a profile it
		// cannot run. Placed at the entry's CLI, with the doctor's remedy.
		path := config.Path{"providers", "llm", key, "agent"}
		message, remedy := c.Said, ""
		for _, line := range c.Lines {
			if !line.OK {
				message, remedy = line.Detail, line.Remedy
				break
			}
		}
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError,
			httpjson.Detail{"detail": message, "hint": remedy, "check": answerOf(c),
				"problems": []config.Problem{{Path: path.String(), Segments: path, Kind: "invalid",
					Message: strings.TrimSpace(message + " " + remedy)}}})
	}
	return false
}

// checkValues holds the values to the store's rules before anything else
// reads them, answering the refusal itself.
func checkValues(w http.ResponseWriter, values map[string]string, names []string) bool {
	switch {
	case len(values) == 0:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "values is empty", "hint": "send the credential under the name the model reads"})
		return false
	case len(values) > MaxValues:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": fmt.Sprintf("%d values, and one model reads at most %d", len(values), MaxValues)})
		return false
	}
	for _, name := range names {
		if secrets.Reserved(name) {
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeReservedName,
				map[string]string{"detail": secrets.ErrReservedName.Error()})
			return false
		}
		if err := errors.Join(secrets.CheckName(name), secrets.CheckValue(values[name])); err != nil {
			httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				httpjson.Detail{"detail": err.Error(), "name": name})
			return false
		}
	}
	return true
}

// refuseDraft answers a draft entry the document refused, and reports whether
// it was admitted.
func refuseDraft(w http.ResponseWriter, err error) bool {
	var managed *configapi.ManagedError
	var invalid *configapi.ValidationError
	var entity *configapi.EntityError
	switch {
	case err == nil:
		return true
	case errors.As(err, &managed):
		configapi.RefuseManaged(w, managed)
	case errors.As(err, &invalid):
		fields := configapi.RefusalFields(err)
		fields["detail"] = invalid.Err.Error()
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError, fields)
	case errors.As(err, &entity):
		fields := configapi.RefusalFields(&configapi.DocumentError{Err: entity.Err})
		fields["detail"] = entity.Err.Error()
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeValidationError, fields)
	case errors.Is(err, configapi.ErrNoActiveRevision):
		httpjson.Fail(w, http.StatusConflict, httpjson.CodeNoActiveRevision)
	default:
		log.Error("llm_draft_check_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
	}
	return false
}

// nameProblems is every value the draft entry does not read, and every value
// whose kind is not its slot's, each located at the value.
func nameProblems(company *config.Company, key string, entry *config.LLMProvider,
	values map[string]string, names []string) []config.Problem {

	reads := company.LLMEntryReads(key)
	sources := entry.CredentialSources(key)
	tokenVar, keyVar := slotVar(sources.Token), slotVar(sources.APIKey)
	profile, known := cliprofile.Builtin(string(entry.Agent))
	if entry.Agent == "" {
		profile, known = cliprofile.Builtin("claude-code")
	}
	var out []config.Problem
	for _, name := range names {
		path := config.Path{"values", name}
		if !slices.Contains(reads, name) {
			reading := "nothing"
			if len(reads) > 0 {
				reading = strings.Join(reads, ", ")
			}
			out = append(out, config.Problem{Path: path.String(), Segments: path, Kind: "unknown_value",
				Message: fmt.Sprintf("%s is not read by this model: it signs in with %s", name, reading)})
			continue
		}
		if !known {
			continue
		}
		value := strings.TrimSpace(values[name])
		isKey := profile.APIKeyPrefix != "" && strings.HasPrefix(value, profile.APIKeyPrefix)
		isToken := profile.TokenPrefix != "" && strings.HasPrefix(value, profile.TokenPrefix)
		switch {
		case name == tokenVar && isKey:
			out = append(out, config.Problem{Path: path.String(), Segments: path, Kind: "invalid",
				Message: "That is an API key. This model signs in with a subscription token " +
					"from its CLI's setup-token; to use a key, add a model that signs in with " +
					"an API key instead."})
		case name == keyVar && isToken:
			out = append(out, config.Problem{Path: path.String(), Segments: path, Kind: "invalid",
				Message: "That is a subscription token. This model signs in with an API key " +
					"from the vendor's console."})
		}
	}
	return out
}

// slotVar is the variable a credential slot reads: the one `${VAR}` it
// writes, or the convention it falls back to.
func slotVar(src config.CredentialSource) string {
	if name, ok := envref.Whole(src.Value); ok {
		return name
	}
	if src.Value == "" {
		return src.Fallback
	}
	return ""
}

// refuseStored refuses a create-only write naming a secret already stored,
// and reports whether none is.
func (s *Service) refuseStored(w http.ResponseWriter, r *http.Request, names []string) bool {
	var held []string
	for _, name := range names {
		_, found, err := s.secrets.Describe(r.Context(), name)
		if err != nil {
			s.fail(w, "read the secret store", err)
			return false
		}
		if found {
			held = append(held, name)
		}
	}
	if len(held) == 0 {
		return true
	}
	httpjson.FailWithFields(w, http.StatusPreconditionFailed, httpjson.CodeAlreadyExists,
		httpjson.Detail{"names": held,
			"hint": "If-None-Match: * asked for new secrets, and these are stored: another model " +
				"may read them. Choose other names, or rotate them without If-None-Match"})
	return false
}

// entriesReading is every entry of company that reads one of names, in the
// company's order.
func entriesReading(company *config.Company, names []string) []string {
	out := []string{}
	for _, key := range company.Providers.ProviderOrder() {
		if slices.ContainsFunc(company.LLMEntryReads(key), func(n string) bool {
			return slices.Contains(names, n)
		}) {
			out = append(out, key)
		}
	}
	return out
}

// sittingOutHere is which of keys THIS node's latest beat reports sitting
// out: the entries whose previous sign-in a new credential replaces, and so
// starts clean.
func (s *Service) sittingOutHere(ctx context.Context, keys []string) ([]string, error) {
	leases, err := s.leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, err
	}
	out := []string{}
	now := s.now()
	for _, lease := range leases {
		if coord.NodeResource(s.node) != lease.Resource {
			continue
		}
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok {
			break
		}
		for _, m := range status.Models {
			if m.SittingOut != nil && m.SittingOut.Until.After(now) && slices.Contains(keys, m.Key) {
				out = append(out, m.Key)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// agentName is an entry's CLI in a sentence.
func agentName(entry *config.LLMProvider) string {
	switch entry.Agent {
	case "", "claude-code":
		return "Claude Code"
	case "codex":
		return "Codex"
	case "opencode":
		return "OpenCode"
	}
	return string(entry.Agent)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// flag reads a boolean query parameter: exactly `true` or `false`, given once,
// or absent for false.
func flag(w http.ResponseWriter, r *http.Request, name string) (bool, bool) {
	values, present := r.URL.Query()[name]
	if !present {
		return false, true
	}
	if len(values) == 1 && (values[0] == "true" || values[0] == "false") {
		return values[0] == "true", true
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery, map[string]string{
		"detail": name + " must be true or false, given once"})
	return false, false
}

// createOnly reads `If-None-Match`: `*` is a create, which refuses a name
// already stored; any other value is no condition this route takes.
func createOnly(w http.ResponseWriter, r *http.Request) (bool, bool) {
	switch none := strings.TrimSpace(r.Header.Get("If-None-Match")); none {
	case "":
		return false, true
	case "*":
		return true, true
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadIfMatch, map[string]string{
		"hint": "a credential write takes If-None-Match: * to store only names nothing holds"})
	return false, false
}

// sealedSentence says which values a seal that stopped part way stored.
func sealedSentence(sealed []string) string {
	if len(sealed) == 0 {
		return "Nothing was stored."
	}
	return strings.Join(sealed, ", ") + " " + plural(len(sealed), "is", "are") +
		" stored, and nothing was re-activated."
}
