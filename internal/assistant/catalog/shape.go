package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/redact"
)

// Result is one read's answer shaped twice: the cards the person sees and the
// text the model reads.
type Result struct {
	// Cards are what the person is shown, built from the route's own JSON.
	Cards []wire.Card
	// Model is what the model is handed: a compact summary of the cards,
	// fenced where the answer carries text somebody wrote, at most
	// [cards.ModelBytes] plus the fence.
	Model string
	// Refusal is the route's refusal, when the answer was one.
	Refusal *wire.Refusal
	// Trust is the label the model's text was fenced under: the tool's,
	// or what its TrustOf made of this answer. A refusal carries none.
	Trust provenance.Trust
	// Empty is an answer that held nothing: no card is drawn, and the
	// model is told what was asked and that nothing matched.
	Empty bool
}

// Failed reports a read the route refused or could not answer.
func (r Result) Failed() bool { return r.Refusal != nil }

// Shape is a read's answer as the person and the model receive it, in this
// order, each step before the next:
//
//  1. A refusal is an error card in the engine's own words, the tool's phrase
//     beside the refusal's sentence, and the model reads the refusal.
//  2. An answer past what a step reads (exec.Response.Truncated) is never
//     carded as though whole. The person is shown an oversize card naming
//     what it read, the screen that shows all of it and what would narrow
//     it; the model is told none of it reached it, which fields the cut
//     answer began with and what to narrow ([oversize], ADR-0061).
//  3. Every string in the answer passes redact.Secrets, so no credential the
//     answer carried reaches a card, the log or the model.
//  4. The card is built by the tool's spec, capped with a notice when cut.
//     AN ANSWER THAT HOLDS NOTHING DRAWS NO CARD (cards.Built.Empty): the
//     step already records the read, so an empty table under every search
//     that found nothing is noise and two download buttons over no rows.
//     The model is handed the call's own arguments, that nothing matched
//     and the answer's own reason where it gave one ([cards.EmptySummary]).
//     THE ONE PLACE this is decided, for every read the catalogue holds.
//     THE CARD'S TITLE CARRIES THE CALL'S SCOPE ([Tool.Scope]), so two reads
//     of one tool with different arguments are two different cards to the
//     person and to the model, and a seat column is drawn by the seat's
//     name where o says it ([cards.Options]).
//  5. The model's summary is the card's, every row or item whole, and is
//     fenced under the turn's boundary when the tool reads text a person or
//     an agent wrote (ADR-0040), or has every fence marker in it defused
//     when it does not, so no field of any answer can open or close a fence.
func Shape(t Tool, args Args, resp exec.Response, b provenance.Boundary, now time.Time,
	o cards.Options) (Result, error) {
	if !cards.Permitted(resp.Status) {
		refusal := exec.RefusalOf(resp)
		refusal.Message = redact.Secrets(refusal.Message)
		refusal.Hint = redact.Secrets(refusal.Hint)
		refusal.Detail = redact.Secrets(refusal.Detail)
		// THE MODEL READS THE ROUTE'S WHOLE ACCOUNT, the person the engine's
		// sentence for its kind (cards.PersonRefusal).
		model, err := json.Marshal(map[string]any{"refused": map[string]any{
			"code": refusal.Code, "message": refusal.ForModel(), "hint": refusal.Hint,
			"grants": refusal.Grants,
		}})
		if err != nil {
			return Result{}, fmt.Errorf("catalog: encode a refusal: %w", err)
		}
		return Result{Cards: []wire.Card{cards.Error(cards.PersonRefusal(refusal, resp.Status),
			t.Phrase, cards.Retryable(resp.Status))}, Model: provenance.Neutralise(b, string(model)),
			Refusal: &refusal}, nil
	}
	if resp.Truncated {
		return oversize(t, args, resp.Body, b)
	}
	if t.Infer {
		return shapeAnswer(t, args, resp, b, now)
	}
	spec, ok := t.CardFor(args)
	if !ok {
		return Result{}, fmt.Errorf("catalog: %s cards no answer of its kind", t.Name)
	}
	body, err := redacted(resp.Body)
	if err != nil {
		return Result{}, err
	}
	built, err := cards.BuildWith(scoped(spec, t.ScopeOf(args)), body, now, o)
	if err != nil {
		return Result{}, fmt.Errorf("catalog: card %s's answer: %w", t.Name, err)
	}
	trust := t.Trust
	if t.TrustOf != nil {
		answer, decodeErr := cards.Decode(body)
		if decodeErr != nil {
			return Result{}, fmt.Errorf("catalog: read %s's answer: %w", t.Name, decodeErr)
		}
		trust = t.TrustOf(args, answer)
	}
	if built.Empty {
		model, emptyErr := emptyModel(t, trust, args, built.Note, b)
		if emptyErr != nil {
			return Result{}, emptyErr
		}
		return Result{Model: model, Trust: trust, Empty: true}, nil
	}
	model, err := fitModel(t, trust, built, b)
	if err != nil {
		return Result{}, err
	}
	return Result{Cards: []wire.Card{built.Card}, Model: model, Trust: trust}, nil
}

// scoped is spec with its table's title carrying scope, the one spelling a
// step's label shares ([withScope]). An entity names its own record.
func scoped(spec cards.Spec, scope string) cards.Spec {
	if spec.Table == nil || scope == "" {
		return spec
	}
	table := *spec.Table
	table.Title = withScope(table.Title, scope)
	spec.Table = &table
	return spec
}

// modelText is a summary as the model reads it: fenced whole, with the
// envelope that labels it, for a tool that reads somebody's text; defused for
// one that does not.
//
// ONE FENCE PER RESULT, labelled with the tool's Source and Trust. A route's
// answer is the company's own rows, written by its people and its agents, and
// the answer does not name each field's writer the way a builtin read does;
// so the label is the least it can vouch for that is still true of every
// writer, and a model told it is a colleague's weighs a request in it as one.
//
// THE MODEL CUT INCLUDES THE FENCE: the summary is built to [cards.ModelBytes]
// less [fenceBytes], so what the model is handed never passes the bound.
func modelText(t Tool, trust provenance.Trust, b provenance.Boundary, summary []byte) (string, error) {
	if !t.Fence {
		return provenance.Neutralise(b, string(summary)), nil
	}
	out, err := json.Marshal(map[string]any{
		"provenance": provenance.NewEnvelope(trust, nil),
		"answer":     provenance.Fence(b, t.Source, trust, nil, string(summary)),
	})
	if err != nil {
		return "", fmt.Errorf("catalog: encode a fenced result: %w", err)
	}
	return string(out), nil
}

// shapeAnswer is a generic read's answer ([Tool.Infer]): the card drawn from
// the answer's own shape where a table can show it, and for the model the
// answer itself rather than a card's summary, because a generic read is how
// the model reaches what no card was designed for, nested fields included.
// The same steps every read takes, in the same order: every string through
// redact.Secrets, the whole fenced under the turn's boundary with the tool's
// Source and the call's trust, and cut, fence included, to [cards.ModelBytes].
// An answer that is not JSON (an export, a file's bytes) is its text, redacted
// and fenced the same way, with no card. A HEAD, or any answer with no body,
// is its status and the headers that describe the resource.
func shapeAnswer(t Tool, args Args, resp exec.Response, b provenance.Boundary,
	now time.Time) (Result, error) {

	if len(bytes.TrimSpace(resp.Body)) == 0 {
		described := map[string]any{"status": resp.Status}
		headers := map[string]string{}
		for _, h := range describingHeaders {
			if v := resp.Header.Get(h); v != "" {
				headers[h] = redact.Secrets(v)
			}
		}
		if len(headers) > 0 {
			described["headers"] = headers
		}
		encoded, err := json.Marshal(described)
		if err != nil {
			return Result{}, fmt.Errorf("catalog: encode %s's answer: %w", t.Name, err)
		}
		return Result{Model: provenance.Neutralise(b, string(encoded)),
			Trust: t.Trust, Empty: true}, nil
	}
	body, err := redacted(resp.Body)
	if err != nil {
		text := redact.Secrets(string(bytes.ToValidUTF8(resp.Body, nil)))
		model, fitErr := fitAnswer(t, t.Trust, b, text)
		return Result{Model: model, Trust: t.Trust}, fitErr
	}
	doc, err := cards.Decode(body)
	if err != nil {
		return Result{}, fmt.Errorf("catalog: read %s's answer: %w", t.Name, err)
	}
	trust := t.Trust
	if t.TrustOf != nil {
		trust = t.TrustOf(args, doc)
	}
	model, err := fitAnswer(t, trust, b, string(body))
	if err != nil {
		return Result{}, err
	}
	out := Result{Model: model, Trust: trust}
	if spec, rows, ok := cards.Infer(doc, callTitle(args)); ok {
		encoded, err := json.Marshal(rows)
		if err != nil {
			return Result{}, fmt.Errorf("catalog: encode %s's rows: %w", t.Name, err)
		}
		built, err := cards.Build(spec, encoded, now)
		if err != nil {
			return Result{}, fmt.Errorf("catalog: card %s's answer: %w", t.Name, err)
		}
		// AN ANSWER THAT HOLDS NOTHING DRAWS NO CARD, the rule every read
		// takes (step 4 of [Shape]), and its step says so.
		if built.Empty {
			out.Empty = true
		} else {
			out.Cards = []wire.Card{built.Card}
		}
	}
	return out, nil
}

// describingHeaders are the headers a body-less answer is described by.
var describingHeaders = []string{"Content-Type", "Content-Length", "ETag", "Last-Modified"}

// fitAnswer is an answer's text as the model reads it, fenced, within
// [cards.ModelBytes] fence included: cut on a character, saying it was cut and
// how to ask for less.
func fitAnswer(t Tool, trust provenance.Trust, b provenance.Boundary, answer string) (string, error) {
	const cutNote = " [cut here: the answer goes on; narrow it with the route's own " +
		"filters or paging]"
	budget := cards.ModelBytes - fenceBytes
	for budget > len(cutNote) {
		text := answer
		if len(text) > budget {
			text = strings.ToValidUTF8(text[:budget-len(cutNote)], "") + cutNote
		}
		model, err := modelText(t, trust, b, []byte(text))
		if err != nil {
			return "", err
		}
		over := len(model) - cards.ModelBytes
		if over <= 0 {
			return model, nil
		}
		budget -= over + over/4 + 64
	}
	return "", fmt.Errorf("catalog: %s's answer does not fit the %d bytes a model is handed",
		t.Name, cards.ModelBytes)
}

// notNarrowing are the arguments that do not narrow a read: they name the one
// record it is of, page through it or pick its view, or (a generic read's)
// carry the request itself.
var notNarrowing = map[string]bool{"limit": true, "cursor": true, "after": true,
	"before": true, "next": true, "view": true, "handle": true, "key": true, "id": true,
	"item": true, "route": true, "path": true, "query": true}

// narrowing is what a read takes that would narrow it, in the words a person
// reads them in, in a stable order.
func narrowing(t Tool) []string {
	if t.schema == nil {
		return nil
	}
	var out []string
	for _, name := range slices.Sorted(maps.Keys(t.schema.properties)) {
		switch {
		case notNarrowing[name]:
		case name == "q":
			out = append(out, "the search words")
		default:
			out = append(out, strings.ReplaceAll(name, "_", " "))
		}
	}
	return out
}

// fieldsOf is the top-level fields a cut answer began with, in order, read
// until the cut: what the model can ask the route to leave out.
func fieldsOf(body []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}
	var fields []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		name, ok := tok.(string)
		if !ok {
			break
		}
		fields = append(fields, name)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			break
		}
	}
	return fields
}

// cutNote is what the model is told of a read past what one step reads.
const cutNote = "None of this answer reached you or the person: it is larger than one " +
	"step reads, so do not answer as though you read it. Read it again narrowed: with " +
	"narrow_by, a smaller page, or (for a route you read with api_request) the route's " +
	"own filters or a flag that leaves out the large field its documentation names. Or " +
	"tell the person it is too large to show here, and that its screen shows all of it."

// oversize is the result of a read whose answer was past what one step reads:
// the oversize card for the person, and for the model what the read was,
// that none of it was read, the fields the cut answer began with and what
// narrows it.
func oversize(t Tool, args Args, body []byte, b provenance.Boundary) (Result, error) {
	title := t.StepLabel(args)
	var open *wire.Link
	if spec, ok := t.CardFor(args); ok {
		switch {
		case spec.Table != nil:
			title = withScope(spec.Table.Title, t.ScopeOf(args))
			open = cards.OpenFor(spec.Table.Open, args)
		case spec.Entity != nil:
			open = cards.OpenFor(spec.Entity.Open, args)
		}
	}
	narrow := narrowing(t)
	model, err := json.Marshal(map[string]any{"cut": map[string]any{
		"read": title, "shown": false, "limit_bytes": exec.MaxResponseBytes,
		"began_with_fields": fieldsOf(body), "narrow_by": narrow, "note": cutNote,
	}})
	if err != nil {
		return Result{}, fmt.Errorf("catalog: encode a cut read: %w", err)
	}
	// DEFUSED, as a refusal is: a field name is the answer's own text.
	return Result{Cards: []wire.Card{cards.Oversize(title, open, narrow)},
		Model: provenance.Neutralise(b, string(model))}, nil
}

// fenceBytes is a first guess at what the fence and its envelope add to a
// summary: two marker lines, the envelope's note, and the escaping a JSON
// string adds to a summary of JSON. [fitModel] measures rather than trusts it.
const fenceBytes = 2 << 10

// fitModel is the card's summary as the model reads it, at most
// [cards.ModelBytes] with the fence included: the summary is built to the
// bound less the fence's guess, and built again smaller by what the result
// went past it, whole rows still, until it fits.
func fitModel(t Tool, trust provenance.Trust, built cards.Built, b provenance.Boundary) (string, error) {
	budget := cards.ModelBytes - fenceBytes
	for budget > 0 {
		summary, err := cards.SummaryOf(built, budget)
		if err != nil {
			return "", fmt.Errorf("catalog: summarise %s's answer: %w", t.Name, err)
		}
		model, err := modelText(t, trust, b, summary)
		if err != nil {
			return "", err
		}
		over := len(model) - cards.ModelBytes
		if over <= 0 {
			return model, nil
		}
		budget -= over + over/4 + 64
	}
	return "", fmt.Errorf("catalog: %s's answer does not fit the %d bytes a model is handed",
		t.Name, cards.ModelBytes)
}

// emptyModel is an empty answer as the model reads it: [cards.EmptySummary] of
// the call's arguments (each string through redact.Secrets, as an answer's
// are) and the answer's note, fenced or defused as any summary of the tool
// is, within [cards.ModelBytes].
func emptyModel(t Tool, trust provenance.Trust, args Args, note string,
	b provenance.Boundary) (string, error) {

	var asked map[string]any
	if len(args) > 0 {
		encoded, err := json.Marshal(map[string]any(args))
		if err != nil {
			return "", fmt.Errorf("catalog: encode %s's arguments: %w", t.Name, err)
		}
		if encoded, err = redacted(encoded); err != nil {
			return "", err
		}
		if err = json.Unmarshal(encoded, &asked); err != nil {
			return "", fmt.Errorf("catalog: read %s's arguments back: %w", t.Name, err)
		}
	}
	summary, err := cards.EmptySummary(t.Phrase, asked, note, cards.ModelBytes-fenceBytes)
	if err != nil {
		return "", fmt.Errorf("catalog: summarise %s's empty answer: %w", t.Name, err)
	}
	model, err := modelText(t, trust, b, summary)
	if err != nil {
		return "", err
	}
	if len(model) > cards.ModelBytes {
		return "", fmt.Errorf("catalog: %s's empty answer does not fit the %d bytes a "+
			"model is handed", t.Name, cards.ModelBytes)
	}
	return model, nil
}

// redacted is body with redact.Secrets applied to every string it holds, keys
// and values alike, re-encoded. Redacting the encoded text instead could
// split a marker across a JSON escape, and a credential is a value wherever it
// sits.
func redacted(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%w: %w", cards.ErrNotJSON, err)
	}
	out, err := json.Marshal(scrub(doc))
	if err != nil {
		return nil, fmt.Errorf("catalog: re-encode a redacted answer: %w", err)
	}
	return out, nil
}

func scrub(v any) any {
	switch t := v.(type) {
	case string:
		return redact.Secrets(t)
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[redact.Secrets(k)] = scrub(val)
		}
		return out
	}
	return v
}
