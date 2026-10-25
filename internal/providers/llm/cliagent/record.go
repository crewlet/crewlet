package cliagent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// recordReader reads one run's record as the CLI writes it — its stdout, line
// by line, or a file it keeps — and tells the tool host what the record shows:
// each model request in order ([llm.ToolHost.Observe]), what each cost as soon
// as it is read ([llm.ToolHost.Spent]), and the prose as it is written
// ([llm.Request.Send]).
//
// # Why as it goes, and not once at the end
//
// A run is a phase's whole loop now, and the host gates it while it runs: the
// round budget counts requests, and the token budget is charged as each one's
// usage is read, so a refusal stops the run at the next call rather than after
// the phase has spent what it was refused. A record read only after the CLI
// exits would gate nothing.
//
// # What it never does
//
// It never puts a line of the record anywhere a person reads. A run's record
// carries every call's arguments and every tool's result; what leaves this
// reader is the model's prose, the calls' names and arguments for the host
// (which has them anyway), the CLI's ids and counts, and — for a failed run —
// the CLI's own failure events, by field ([cliprofile.MCP.FailureEvents]).
type recordReader struct {
	p    cliprofile.Profile
	rule *cliprofile.Requests
	host llm.ToolHost
	req  llm.Request
	// sent is what the engine handed the CLI, for the estimates a record
	// with no counts is charged.
	sent string

	// streams says the profile streams prose apart from the requests'
	// text, so the text is not streamed a second time.
	streams bool
	// perRequestUsage says the record carries each request's counts, so no
	// request is estimated while the run goes.
	perRequestUsage bool

	mu       sync.Mutex
	requests []*request
	current  *request
	// offered is the tools the CLI offers its model, where its record says.
	offered []string
	// lines is how many lines were read, and kinds the discriminators of
	// the events among them, for a failure that has to say what the record
	// looked like without quoting it.
	lines int
	kinds []string
	// stdout is the extraction of a stdout record, shared with the batch
	// path — nil for a file source.
	stdout *streamExtractor
	// failures are the CLI's own failure events, rendered by field.
	failures []string
	// capped is a line matching the profile's cap signature.
	capped bool
	// spent is everything reported through the host so far, and pending
	// the estimates made under the lock and not yet reported.
	spent   llm.Usage
	pending []llm.Usage
}

// request is one model request as the record shows it so far.
type request struct {
	id    string
	model string
	text  []string
	calls []llm.TurnCall
	stop  llm.StopReason
	// streamed is the prose streamed for it, which an estimate of its
	// output reads when the record never gave its count.
	streamed strings.Builder

	// figures are the record's raw counts, by figure — the last value read
	// for each — and has says which were read at all.
	figures map[string]int
	has     map[string]bool

	// reported is what the host was told this request cost.
	reported llm.Usage
	// estimated says reported holds an estimate of its input, made when the
	// request was seen because the record carries no counts per request.
	estimatedIn, estimatedOut bool

	observed bool
	dirty    bool
}

// Figure names, as [request.figures] keys them.
const (
	figInput      = "input"
	figOutput     = "output"
	figCacheRead  = "cache_read"
	figCacheWrite = "cache_write"
	figReasoning  = "reasoning"
	figToolPrompt = "tool_prompt"
)

func newRecordReader(p cliprofile.Profile, req llm.Request, sent string) *recordReader {
	r := &recordReader{p: p, rule: p.Requests, host: req.Tools, req: req, sent: sent}
	if p.EffectiveOutput() == cliprofile.OutputJSONL {
		r.stdout = &streamExtractor{p: p}
	}
	if r.rule != nil {
		for _, e := range r.rule.Events {
			if len(e.Delta) > 0 {
				r.streams = true
			}
			if usageDeclared(e.Usage) {
				r.perRequestUsage = true
			}
		}
	}
	return r
}

func usageDeclared(u cliprofile.UsagePaths) bool {
	return len(u.Input)+len(u.Output)+len(u.CacheRead)+len(u.CacheWrite)+
		len(u.Reasoning)+len(u.ToolPrompt) > 0
}

// observes reports whether this reader reports requests at all — the
// [llm.ServeOptions.ObservesRequests] the host is told.
func (r *recordReader) observes() bool { return r.rule != nil }

// stdoutLine is one line of the run's stdout. Every line feeds the
// extraction a finished stdout would have; it is the record too where the
// profile reads its requests there.
func (r *recordReader) stdoutLine(line string) {
	doc, ok := decodeRecordLine(line)
	if !ok {
		return
	}
	r.mu.Lock()
	if r.stdout != nil {
		r.stdout.event(doc)
	}
	r.noteLocked(doc)
	r.mu.Unlock()
	if r.rule != nil && r.rule.Source == cliprofile.SourceStdout {
		r.event(doc)
	}
}

// fileLine is one line of a file record.
func (r *recordReader) fileLine(line string) {
	if doc, ok := decodeRecordLine(line); ok {
		r.event(doc)
	}
}

// decodeRecordLine decodes one line THROUGH json.Number, so a call's arguments
// read from the record compare equal to the same call's as the server decoded
// them — a large id rounded through a float64 would not — and survive.
func decodeRecordLine(line string) (map[string]any, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	return doc, true
}

// noteLocked keeps what a failure may say about a stdout line: its
// discriminator, whether it is one of the CLI's own failure reports, and
// whether it is the cap's signature.
func (r *recordReader) noteLocked(doc map[string]any) {
	r.lines++
	if kind, ok := lookup(doc, r.p.EventTypePath); ok && len(r.p.EventTypePath) > 0 {
		if s, isString := kind.(string); isString && !slices.Contains(r.kinds, s) && len(r.kinds) < maxKinds {
			r.kinds = append(r.kinds, s)
		}
	}
	if r.p.Cap != nil && len(r.p.Cap.Match) > 0 && matches(doc, r.p.Cap.Match) {
		r.capped = true
	}
	if r.p.MCP == nil {
		return
	}
	for _, f := range r.p.MCP.FailureEvents {
		if !matches(doc, f.Match) {
			continue
		}
		if rendered := renderFields(doc, f.Fields); rendered != "" {
			r.failures = append(r.failures, rendered)
			if len(r.failures) > maxFailureEvents {
				r.failures = r.failures[1:]
			}
		}
		break
	}
}

// maxKinds bounds the discriminators a drift message lists, and
// maxFailureEvents the failure events a failed run's error carries — the LAST
// ones, which say how the run ended. Enough to read a stream's shape and a
// failure's cause by; a stream that says more is saying it the same way.
const (
	maxKinds         = 16
	maxFailureEvents = 5
)

// renderFields renders the fields of a failure event that resolve, one
// `path=value` each, values JSON-encoded so a field is one line.
func renderFields(doc map[string]any, fields []cliprofile.Path) string {
	var parts []string
	for _, f := range fields {
		v, ok := lookup(doc, f)
		if !ok || v == nil {
			continue
		}
		var s string
		if str, isString := v.(string); isString {
			s = str
		} else {
			raw, err := json.Marshal(v)
			if err != nil {
				continue
			}
			s = string(raw)
		}
		if strings.TrimSpace(s) == "" {
			continue
		}
		parts = append(parts, f.String()+"="+strings.Join(strings.Fields(s), " "))
	}
	return strings.Join(parts, " ")
}

// event applies every rule of the record to one event, then tells the host
// what changed.
func (r *recordReader) event(doc map[string]any) {
	r.mu.Lock()
	var deltas []string
	touched := map[*request]bool{}
	for _, e := range r.rule.Events {
		if !matches(doc, e.Match) {
			continue
		}
		req := r.requestForLocked(doc, e)
		if req != nil {
			touched[req] = true
		}
		if len(e.Tools) > 0 {
			if v, ok := lookup(doc, e.Tools); ok {
				r.offered = stringsOf(v)
			}
		}
		if req == nil {
			continue
		}
		if len(e.Model) > 0 {
			if s, ok := stringAt(doc, e.Model); ok && s != "" {
				req.model, req.dirty = s, true
			}
		}
		if len(e.Stop) > 0 {
			if s, ok := stringAt(doc, e.Stop); ok {
				if stop := r.stopOf(s); stop != "" {
					req.stop, req.dirty = stop, true
				}
			}
		}
		if len(e.Delta) > 0 {
			if s, ok := stringAt(doc, e.Delta); ok && s != "" {
				req.streamed.WriteString(s)
				deltas = append(deltas, s)
			}
		}
		if len(e.Text) > 0 {
			if s, ok := stringAt(doc, e.Text); ok && s != "" {
				req.text, req.dirty = append(req.text, s), true
				if !r.streams {
					deltas = append(deltas, s)
				}
			}
		}
		if e.Call != nil {
			req.calls, req.dirty = append(req.calls, r.callOf(doc, e.Call)), true
		}
		if e.Blocks != nil {
			deltas = append(deltas, r.blocksLocked(doc, e.Blocks, req)...)
		}
		r.figuresLocked(doc, e.Usage, req)
	}
	turns, spends := r.flushLocked(touched, false)
	r.mu.Unlock()
	r.tell(deltas, turns, spends)
}

// requestForLocked is the request an event belongs to: a new one where the
// rule opens one; the one of its id, opened by it if none is; else the one
// open now. Nil for an event that names none while none is open.
func (r *recordReader) requestForLocked(doc map[string]any, e cliprofile.RequestEvent) *request {
	id := ""
	if len(e.ID) > 0 {
		id, _ = stringAt(doc, e.ID)
	}
	if id != "" {
		for _, req := range r.requests {
			if req.id == id {
				r.current = req
				return req
			}
		}
	} else if !e.Opens {
		return r.current
	}
	req := &request{id: id, figures: map[string]int{}, has: map[string]bool{}}
	if req.id == "" {
		// A request the record names no id for is still keyed apart from
		// every other: the host keys requests by id.
		req.id = fmt.Sprintf("request-%d", len(r.requests)+1)
	}
	r.requests = append(r.requests, req)
	r.current = req
	if !r.perRequestUsage {
		r.estimateInputLocked(req)
	}
	return req
}

// estimateInputLocked charges a request the record gives no counts for its
// input, as it is seen: the prompt the engine sent, and everything the
// requests before it wrote — each request re-sends the conversation so far, so
// that is a floor under what it read. Its tool results are not in it (the
// record of most such CLIs does not carry them), so it is low rather than
// high; the CLI's own total, where it reports one, settles the difference.
func (r *recordReader) estimateInputLocked(req *request) {
	var prior int
	for _, earlier := range r.requests {
		if earlier == req {
			break
		}
		written := r.writtenBy(earlier)
		prior += written
		if !earlier.estimatedOut && !earlier.has[figOutput] {
			// The request before this one is over: what it wrote is
			// charged now, as its output.
			earlier.reported.Output += written
			earlier.estimatedOut = true
			r.pending = append(r.pending, llm.Usage{Output: written, Estimated: true})
		}
	}
	est := EstimateTokens(r.sent) + prior
	req.reported.Input += est
	req.estimatedIn = true
	r.pending = append(r.pending, llm.Usage{Input: est, Estimated: true})
}

// writtenBy is the estimate of what a request wrote: its prose, and its calls'
// names and arguments.
func (r *recordReader) writtenBy(req *request) int {
	n := EstimateTokens(strings.Join(req.text, "\n"))
	if n == 0 {
		n = EstimateTokens(req.streamed.String())
	}
	for _, c := range req.calls {
		raw, _ := json.Marshal(c.Arguments)
		n += EstimateTokens(c.Name) + EstimateTokens(string(raw))
	}
	return n
}

// blocksLocked reads an event's content blocks into a request, and returns
// the prose to stream.
func (r *recordReader) blocksLocked(doc map[string]any, b *cliprofile.Blocks, req *request) []string {
	v, ok := lookup(doc, b.Path)
	if !ok {
		return nil
	}
	items, isList := v.([]any)
	if !isList {
		return nil
	}
	var deltas []string
	for _, item := range items {
		block, isObject := item.(map[string]any)
		if !isObject {
			continue
		}
		for _, rule := range b.Each {
			if !matches(block, rule.Match) {
				continue
			}
			switch {
			case len(rule.Text) > 0:
				if s, ok := stringAt(block, rule.Text); ok && s != "" {
					req.text, req.dirty = append(req.text, s), true
					if !r.streams {
						deltas = append(deltas, s)
					}
				}
			case rule.Call != nil:
				req.calls, req.dirty = append(req.calls, r.callOf(block, rule.Call)), true
			}
			break
		}
	}
	return deltas
}

// callOf reads one call. A tool named with the host's prefix is the host's,
// reported by the host's own name; any other is one of the CLI's own.
func (r *recordReader) callOf(doc map[string]any, paths *cliprofile.CallPaths) llm.TurnCall {
	var call llm.TurnCall
	if len(paths.ID) > 0 {
		call.ID, _ = stringAt(doc, paths.ID)
	}
	name, _ := stringAt(doc, paths.Name)
	if prefix := r.rule.HostPrefix; prefix != "" && strings.HasPrefix(name, prefix) {
		call.Name, call.Host = strings.TrimPrefix(name, prefix), true
	} else {
		call.Name = name
	}
	call.Arguments = map[string]any{}
	if len(paths.Args) > 0 {
		if v, ok := lookup(doc, paths.Args); ok {
			call.Arguments = argumentsOf(v)
		}
	}
	return call
}

// argumentsOf is a call's arguments as the record carries them: an object, or
// an object encoded as a JSON string, as some CLIs record them. Anything else
// is no arguments.
func argumentsOf(v any) map[string]any {
	switch a := v.(type) {
	case map[string]any:
		return a
	case string:
		if doc, ok := decodeRecordLine(a); ok {
			return doc
		}
	}
	return map[string]any{}
}

// figuresLocked reads an event's counts into a request: the last value read
// for each figure, never a lower one — a stream reports a running count, and
// a figure that went down is a later event counting something else.
func (r *recordReader) figuresLocked(doc map[string]any, u cliprofile.UsagePaths, req *request) {
	for _, f := range []struct {
		name  string
		paths []cliprofile.Path
	}{
		{figInput, u.Input}, {figOutput, u.Output}, {figCacheRead, u.CacheRead},
		{figCacheWrite, u.CacheWrite}, {figReasoning, u.Reasoning}, {figToolPrompt, u.ToolPrompt},
	} {
		if n, ok := firstInt(doc, f.paths); ok {
			if !req.has[f.name] || n > req.figures[f.name] {
				req.figures[f.name] = n
			}
			req.has[f.name] = true
			req.dirty = true
		}
	}
}

// usageOf is what the record says a request cost, on the contract's rule: the
// whole prompt as Input, by the vendor's arithmetic.
func (r *recordReader) usageOf(req *request) llm.Usage {
	f := req.figures
	return llm.Usage{
		Input:      r.p.Usage.PromptTokens(f[figInput], f[figCacheRead], f[figCacheWrite]) + f[figToolPrompt],
		Output:     f[figOutput] + f[figReasoning],
		CacheRead:  f[figCacheRead],
		CacheWrite: f[figCacheWrite],
	}
}

// stopOf maps the record's stop reason onto the engine's, or "" for one the
// profile does not name.
func (r *recordReader) stopOf(s string) llm.StopReason {
	switch r.rule.Stops[s] {
	case "end":
		return llm.StopEnd
	case "tool_use":
		return llm.StopToolUse
	default:
		return ""
	}
}

// flushLocked collects what the host must be told: each touched request worth
// observing — one with prose or calls, or every one when the run is over —
// and the spend no report has carried yet.
func (r *recordReader) flushLocked(touched map[*request]bool, final bool) ([]llm.Turn, []llm.Usage) {
	var turns []llm.Turn
	spends := r.pending
	r.pending = nil
	for _, req := range r.requests {
		if !touched[req] && !final {
			continue
		}
		// THE COUNTS FIRST: an increment over what was reported, figure by
		// figure, never a refund.
		if got := r.usageOf(req); req.has[figInput] || req.has[figOutput] {
			inc := llm.Usage{
				Input:      max(0, got.Input-req.reported.Input),
				Output:     max(0, got.Output-req.reported.Output),
				CacheRead:  max(0, got.CacheRead-req.reported.CacheRead),
				CacheWrite: max(0, got.CacheWrite-req.reported.CacheWrite),
			}
			if inc != (llm.Usage{}) {
				req.reported = addUsage(req.reported, inc)
				spends = append(spends, inc)
			}
		}
		if !req.dirty && !(final && !req.observed) {
			continue
		}
		if !final && len(req.text) == 0 && len(req.calls) == 0 {
			// Seen, but with nothing in it yet: observed once it says
			// something, or when the run ends — so text streamed before
			// it is attributed to it rather than to the request after.
			continue
		}
		req.observed, req.dirty = true, false
		turns = append(turns, r.turnOf(req))
	}
	for _, s := range spends {
		r.spent = addUsage(r.spent, s)
	}
	return turns, spends
}

// turnOf is one request as the host is told it.
func (r *recordReader) turnOf(req *request) llm.Turn {
	t := llm.Turn{
		ID: req.id, Text: strings.TrimSpace(strings.Join(req.text, "\n")),
		Calls: slices.Clone(req.calls), Model: req.model, Stop: req.stop,
	}
	if req.has[figInput] || req.has[figOutput] {
		u := r.usageOf(req)
		t.Usage = &u
	}
	return t
}

// tell hands the host what one event changed, outside the reader's lock:
// the host may stop the run over a spend, and that must not wait on a reader
// that is waiting on it.
func (r *recordReader) tell(deltas []string, turns []llm.Turn, spends []llm.Usage) {
	for _, d := range deltas {
		r.req.Send(llm.Delta{Content: d})
	}
	for _, u := range spends {
		r.host.Spent(u)
	}
	for _, t := range turns {
		r.host.Observe(t)
	}
}

// finish tells the host every request it has not been told of, now that the
// record is complete.
func (r *recordReader) finish() {
	if r.rule == nil {
		return
	}
	r.mu.Lock()
	turns, spends := r.flushLocked(nil, true)
	r.mu.Unlock()
	r.tell(nil, turns, spends)
}

// settle reports what the run cost that nothing has reported yet — on every
// outcome — and returns the run's whole cost.
//
// total is the CLI's own count of the run, where it gave one: the remainder is
// the difference, figure by figure, never below what was already charged.
// Where it gave none, what is estimated instead is what the record could not
// count: each request's output it never gave a count for, from what the request
// wrote, and — where it showed no request at all — one request of the prompt
// the engine sent and the answer.
func (r *recordReader) settle(total *llm.Usage, answer string) llm.Usage {
	r.mu.Lock()
	var rem llm.Usage
	if total != nil {
		rem = llm.Usage{
			Input:      max(0, total.Input-r.spent.Input),
			Output:     max(0, total.Output-r.spent.Output),
			CacheRead:  max(0, total.CacheRead-r.spent.CacheRead),
			CacheWrite: max(0, total.CacheWrite-r.spent.CacheWrite),
		}
	} else {
		rem.Estimated = true
		if len(r.requests) == 0 {
			rem.Input = max(0, EstimateTokens(r.sent)-r.spent.Input)
			rem.Output = max(0, EstimateTokens(answer)-r.spent.Output)
		}
		for i, req := range r.requests {
			if !req.has[figInput] && !req.estimatedIn {
				prior := 0
				for _, earlier := range r.requests[:i] {
					prior += r.writtenBy(earlier)
				}
				rem.Input += EstimateTokens(r.sent) + prior
			}
			if !req.has[figOutput] && !req.estimatedOut {
				rem.Output += r.writtenBy(req)
				req.estimatedOut = true
			}
		}
	}
	if rem.Input > 0 || rem.Output > 0 || rem.CacheRead > 0 || rem.CacheWrite > 0 {
		r.spent = addUsage(r.spent, rem)
	} else {
		rem = llm.Usage{}
	}
	spent := r.spent
	r.mu.Unlock()
	if rem != (llm.Usage{}) {
		r.host.Spent(rem)
	}
	return spent
}

// last is the last request's prose and model, where the record showed any.
func (r *recordReader) last() (text, model string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.requests) == 0 {
		return "", "", false
	}
	req := r.requests[len(r.requests)-1]
	model = req.model
	for i := len(r.requests) - 1; i >= 0 && model == ""; i-- {
		model = r.requests[i].model
	}
	return strings.TrimSpace(strings.Join(req.text, "\n")), model, true
}

// observed is how many requests the record showed.
func (r *recordReader) observed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// offeredTools is what the CLI's record says it offered its model.
func (r *recordReader) offeredTools() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.offered)
}

// stdoutResult is the extraction of a stdout record, as [extract] would have
// read the whole of it, and whether there is one.
func (r *recordReader) stdoutResult() (extracted, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdout == nil {
		return extracted{}, false
	}
	return r.stdout.result(), true
}

// failureDetail is what a failed run's error says beyond stderr: the CLI's own
// failure events, by field.
func (r *recordReader) failureDetail() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.failures) == 0 {
		return ""
	}
	return "its failure events: " + strings.Join(r.failures, "; ")
}

// shape says what the record looked like, for a message that may not quote it.
func (r *recordReader) shape() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.kinds) == 0 {
		return fmt.Sprintf("%d lines of JSON", r.lines)
	}
	return fmt.Sprintf("%d lines of JSON, of the kinds %s", r.lines, strings.Join(r.kinds, ", "))
}

// hitCap reports whether the record carried the profile's cap signature.
func (r *recordReader) hitCap() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capped
}

// matches reports whether every condition holds in doc.
func matches(doc map[string]any, conditions []cliprofile.Match) bool {
	for _, c := range conditions {
		v, ok := lookup(doc, c.Path)
		if !ok || !equalJSON(v, c.Equals) {
			return false
		}
	}
	return true
}

// equalJSON compares a decoded JSON value with a profile's scalar: a string
// with a string, a boolean with a boolean, a number with a number.
func equalJSON(v, want any) bool {
	switch w := want.(type) {
	case string:
		s, ok := v.(string)
		return ok && s == w
	case bool:
		b, ok := v.(bool)
		return ok && b == w
	}
	wn, ok := numberOf(want)
	if !ok {
		return false
	}
	vn, ok := numberOf(v)
	return ok && vn == wn
}

func numberOf(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func stringAt(doc map[string]any, path cliprofile.Path) (string, bool) {
	v, ok := lookup(doc, path)
	if !ok {
		return "", false
	}
	s, isString := v.(string)
	return s, isString
}

// stringsOf is a list of strings, or of objects named by their `name`.
func stringsOf(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range items {
		switch it := item.(type) {
		case string:
			out = append(out, it)
		case map[string]any:
			if name, ok := it["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite,
		Estimated: a.Estimated || b.Estimated,
	}
}
