package queries

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/named"
)

// What the lines a page draws name (package named), resolved in one ask for
// the reader who will draw them: a reference is a link only where the reader
// may open it and it still exists, and its marked words otherwise.
//
// EACH IS ASKED OF THE QUESTION ITS OBJECT'S OWN PAGE ASKS, through this
// registry and so as the caller, as chat_unfurls asks a link's record, so a
// reference grants nothing. Refused and gone are both absent, never told
// apart; a question the node could not answer (an outage, not a refusal) names
// the reference under `unresolved`, so a screen asks again.

// MaxReferences bounds one ask: a page of the event log (50 rows), each row
// naming a couple of objects, is the largest set one screen draws at once.
const MaxReferences = 100

// resolvedRef is one reference the caller may open.
type resolvedRef struct {
	Kind named.Kind `json:"kind"`
	ID   string     `json:"id"`
	// Title is what the object is called now, for the reference's hover; empty
	// where its page names it by its id alone (a turn, a channel).
	Title string `json:"title,omitempty" untrusted:"name"`
}

// referencesAnswer is the references a page asked about that the caller may
// open, and those this node could not settle.
type referencesAnswer struct {
	References []resolvedRef `json:"references"`
	Unresolved []string      `json:"unresolved"`
}

// refAsk is how one kind is resolved: the question its page asks, the
// parameters an id makes, and where its title is in the answer. A kind whose
// page reads a list (`list`) is open when the list holds it.
type refAsk struct {
	question string
	params   func(id string) (map[string]any, bool)
	title    []string
	list     func(answer any, id string) (title string, held bool)
}

// refAsks is every kind a reference resolves, by its page's own question.
var refAsks = map[named.Kind]refAsk{
	named.WorkItem: {question: "work_item", params: byParam("id"), title: []string{"task", "title"}},
	named.Change: {question: "code_change", title: []string{"change", "title"},
		params: func(id string) (map[string]any, bool) {
			repo, number, ok := strings.Cut(id, "!")
			n, err := strconv.ParseInt(number, 10, 64)
			if !ok || repo == "" || err != nil || n <= 0 {
				return nil, false
			}
			return map[string]any{"name": repo, "change": n}, true
		}},
	named.Commit: {question: "code_commit", title: []string{"commit", "subject"},
		params: func(id string) (map[string]any, bool) {
			repo, sha, ok := strings.Cut(id, "@")
			if !ok || repo == "" || sha == "" {
				return nil, false
			}
			return map[string]any{"name": repo, "sha": sha}, true
		}},
	named.Repository: {question: "code_repo", params: byParam("name"), title: []string{"repo", "description"}},
	named.Page:       {question: "page", params: byParam("id"), title: []string{"page", "title"}},
	named.Artifact:   {question: "artifact", params: byParam("id"), title: []string{"title"}},
	named.Turn:       {question: "turn", params: byParam("turn_id")},
	named.Room:       {question: "chat_channel", params: byParam("channel_id"), title: []string{"channel", "name"}},
	named.Seat: {question: "colleague", params: byParam("q"),
		list: func(answer any, id string) (string, bool) {
			match, _ := lookup(answer, "match").(map[string]any)
			return "", match != nil && match["handle"] == id
		}},
	named.Schedule: {question: "schedules", params: func(string) (map[string]any, bool) { return nil, true },
		list: func(answer any, id string) (string, bool) {
			rows, _ := lookup(answer, "schedules").([]any)
			for _, row := range rows {
				s, _ := row.(map[string]any)
				if fmt.Sprint(s["scope_type"], "/", s["scope_id"], "/", s["name"]) == id {
					return "", true
				}
			}
			return "", false
		}},
	named.Channel: {question: "a2a_channels",
		params: func(string) (map[string]any, bool) { return map[string]any{"state": "all"}, true },
		list: func(answer any, id string) (string, bool) {
			rows, _ := lookup(answer, "channels").([]any)
			for _, row := range rows {
				if c, _ := row.(map[string]any); c["id"] == id {
					return "", true
				}
			}
			return "", false
		}},
}

// byParam is the parameters of a question that takes the id under one name.
func byParam(name string) func(string) (map[string]any, bool) {
	return func(id string) (map[string]any, bool) { return map[string]any{name: id}, true }
}

// lookup is the value at a path of object keys in a decoded answer, or nil.
func lookup(v any, path ...string) any {
	for _, key := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}

// references answers which of the references a page draws (`refs`, each
// `<kind>:<id>`, at most [MaxReferences]) the caller may open, with titles.
func (s Sources) references(r *Registry) Answer {
	return func(ctx context.Context, p Params) (any, error) {
		refs, err := referenceParams(p)
		if err != nil {
			return nil, err
		}
		out := referencesAnswer{References: []resolvedRef{}, Unresolved: []string{}}
		// ONE ANSWER PER QUESTION AND PARAMETERS within one ask: the
		// schedules and channels lists are read once however many of
		// their entries a page names.
		answers := map[string]refAnswer{}
		for _, ref := range refs {
			title, open, err := s.resolveRef(ctx, r, ref, answers)
			switch {
			case err != nil:
				log.DebugContext(ctx, "reference_unresolved", "kind", string(ref.Kind), "error", err)
				out.Unresolved = append(out.Unresolved, string(ref.Kind)+":"+ref.ID)
			case open:
				out.References = append(out.References, resolvedRef{Kind: ref.Kind, ID: ref.ID, Title: title})
			}
		}
		return out, nil
	}
}

// refAnswer is one question's decoded answer, or the failure it was.
type refAnswer struct {
	value any
	err   error
}

// resolveRef is one reference's title and whether the caller may open it. An
// error is the node's own: a refusal or an absence is a closed reference.
func (s Sources) resolveRef(ctx context.Context, r *Registry, ref named.Ref,
	answers map[string]refAnswer) (string, bool, error) {

	ask, known := refAsks[ref.Kind]
	if !known {
		return "", false, nil
	}
	params, ok := ask.params(ref.ID)
	if !ok {
		return "", false, nil
	}
	key, err := json.Marshal([]any{ask.question, params})
	if err != nil {
		return "", false, fmt.Errorf("queries: key a reference's question: %w", err)
	}
	got, asked := answers[string(key)]
	if !asked {
		got.value, got.err = decodedAnswer(r.Answer(ctx, ask.question, params))
		answers[string(key)] = got
	}
	switch {
	case got.err == nil:
	case unansweredRecord(got.err):
		return "", false, nil
	default:
		return "", false, got.err
	}
	if ask.list != nil {
		title, held := ask.list(got.value, ref.ID)
		return title, held, nil
	}
	title, _ := lookup(got.value, ask.title...).(string)
	return title, true, nil
}

// decodedAnswer is an answer as the wire carries it, decoded, so a title is
// read where the record's own page reads it.
func decodedAnswer(answer any, err error) (any, error) {
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(answer)
	if err != nil {
		return nil, fmt.Errorf("queries: encode a reference's answer: %w", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("queries: decode a reference's answer: %w", err)
	}
	return out, nil
}

// referenceParams reads `refs`: each `<kind>:<id>` of a kind this build names,
// at most [MaxReferences], repeats folded.
func referenceParams(p Params) ([]named.Ref, error) {
	list, ok := p.Values()["refs"].([]any)
	if !ok || len(list) == 0 {
		return nil, badParams("refs", "", nil)
	}
	if len(list) > MaxReferences {
		return nil, fmt.Errorf("%w: `refs` names %d references and one ask resolves at most %d; "+
			"ask for the ones a page draws", ErrBadParams, len(list), MaxReferences)
	}
	out, seen := make([]named.Ref, 0, len(list)), map[string]bool{}
	for _, v := range list {
		text, _ := v.(string)
		kind, id, ok := strings.Cut(strings.TrimSpace(text), ":")
		if !ok || id == "" || !named.Kind(kind).Valid() {
			return nil, badParams("refs", fmt.Sprint(v), nil)
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, named.Ref{Kind: named.Kind(kind), ID: id, Text: id})
	}
	return out, nil
}
