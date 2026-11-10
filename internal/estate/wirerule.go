package estate

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// THE WIRE'S RULE FOR WHAT AN OPERATION TAKES: a key on the wire is a value the
// serving build must read, and a build that cannot read one runs nothing.
//
// # Why there is a rule at all
//
// A rolling upgrade mixes builds, and the node that asks is not always the
// build of the data node that answers. A seats-only node upgraded first sends
// an operation whose arguments carry a field the data node's older build does
// not declare — `executor.end`'s ending growing a fact about the turn, as
// #519's `went_on` did — and a data node that decoded them with a plain
// json.Unmarshal dropped the key without a word: the write ran as though the
// writer had never sent it, the record was stamped with the lowest version
// that reads what was LEFT, and every node, the newer ones included, applied
// it by the older rule. Nothing logged it. The rest of the upgrade path does
// the opposite — a state-log record a build cannot read is deferred, never
// dropped; a parked run's unknown keys ride its row's Extra; an owed end
// holding keys a build does not know is left owed for one that does — and so
// does this wire now.
//
// # Two halves, both the standard library's own
//
// What an operation takes — its arguments, and the actor it runs as — crosses
// under two options of encoding/json, so neither half is a rule of this
// package's to keep in step with the decoder:
//
//   - ENCODED with every struct field LEFT OUT AT ITS ZERO
//     ([jsonv2.OmitZeroStructFields], which the standard library documents as
//     `omitzero` on every field). A field a later build adds goes on the wire
//     only when a write uses it, so a write that does not is served by an
//     older data node exactly as before, and only one that WOULD lose
//     something is refused. Its zero means what an older build does — the
//     value a build without the field decodes the absence as — which is the
//     rule [define] states for every operation.
//   - DECODED refusing every object member no field declares
//     ([jsonv2.RejectUnknownMembers], the rule json.Decoder's
//     DisallowUnknownFields sets), at every depth: a nested struct, each
//     element of a slice or array of structs, an embedded struct's promoted
//     fields. A map's keys are data, never declarations, and so is the inside
//     of a json.RawMessage — a datastore document, a canonical snapshot —
//     which nothing here decodes.
//
// Each is json.Marshal's and json.Unmarshal's own behaviour in every other
// respect ([json.DefaultOptionsV1]): the same bytes for every value but a
// field at its zero, the same case-insensitive names, the same promotion, the
// same `-`. The decoder reports a member it refuses as a [jsonv2.SemanticError]
// naming where it is ([json.ReportErrorsWithLegacySemantics] off), which a bare
// DisallowUnknownFields does not: its error is an untyped `json: unknown field
// "went_on"`, with no path — so a member under `Ending` read the same as one at
// the top — and a refusal built from it would have been a reading of the
// decoder's words.
//
// # Not a refusal of a field this build retired
//
// The rule refuses what the serving build does not declare, whichever build
// sent it. A field a later build REMOVES is the same case run backwards: a
// writer of the older build still sends it, and the newer data node refuses
// the write rather than drop it. A field is retired by a build that stops
// SETTING it first, so the wire never carries it again before the build that
// deletes it.
//
// # The in-process path takes neither
//
// A data node's own router answers its own seats from its own copy in
// process ([Router.tryLocal]) with the caller's own values: nothing is encoded,
// so nothing can be lost, and nothing here runs.
var (
	inputEncoding = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.OmitZeroStructFields(true))
	inputDecoding = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.RejectUnknownMembers(true),
		json.ReportErrorsWithLegacySemantics(false))
)

// encodeInput is v — an operation's arguments, the actor it runs as, or an
// answer a caller carries into another request ([op.answeredWhole]) — as it crosses
// the wire: json.Marshal's encoding with every struct field at its zero left
// out ([inputEncoding]).
func encodeInput(v any) ([]byte, error) {
	return jsonv2.Marshal(v, inputEncoding)
}

// decodeInput decodes raw into into, a pointer, under the wire's rule
// ([inputDecoding]), and answers every member raw holds that into's type does
// not declare, each by its path ([strikeMember]), sorted — none where it read
// raw whole. err is any other failure to decode.
//
// WHERE IT ANSWERS MEMBERS, into holds whatever the decoder had filled before
// it stopped, and is never to be used: the caller refuses the request.
//
// EVERY MEMBER, NOT THE FIRST. The decoder stops at the first member it
// refuses, so each one it names is struck from a copy of the document — from
// every element of each list on its way, where the same member is undeclared
// in each — and the copy decoded again, until it decodes whole. The standard
// library's rule decides every one: nothing here decides which keys a type
// declares, its case-insensitive names, its promoted fields and its `-` tags
// included, so this can never disagree with the decoder about one.
func decodeInput(raw []byte, into any) ([]string, error) {
	decoded := jsonv2.Unmarshal(raw, into, inputDecoding)
	at, refused := unknownMember(decoded)
	if !refused {
		return nil, decoded
	}
	// A COPY TO STRIKE FROM, its numbers kept as their text: a number this
	// build would have read into a narrower type must reach the next
	// decode as the writer wrote it, or the copy fails on something other
	// than a member and the rest go unnamed.
	doc, docErr := looseDoc(raw)
	if docErr != nil {
		return nil, fmt.Errorf("read the document again to name what it holds: %w", docErr)
	}
	target := reflect.TypeOf(into).Elem()
	var unknown []string
	for {
		path, struck := strikeMember(doc, at)
		unknown = append(unknown, path)
		if !struck {
			// NOTHING LEFT TO STRIKE where the decoder pointed: the
			// member is named, and a loop that cannot shorten the
			// document would name it for ever.
			break
		}
		rest, encodeErr := json.Marshal(doc)
		if encodeErr != nil {
			// NEVER, for a document a decode built — objects, lists,
			// numbers kept as their text, strings, booleans and nulls — and
			// said rather than swallowed, should a decoder ever build one
			// that does not encode.
			return nil, fmt.Errorf("encode the document again to name what it holds: %w", encodeErr)
		}
		// WHOLE, or failing for another reason, which the refusal of the
		// members already named outranks: the request is a build's this
		// one cannot read either way.
		if at, refused = unknownMember(jsonv2.Unmarshal(rest, reflect.New(target).Interface(),
			inputDecoding)); !refused {
			break
		}
	}
	// SORTED, because the order they were met in is the copy's — whose
	// members the encoder writes in its own order — and a refusal that
	// named them in one order on one node and another on the next would be
	// two refusals of one request.
	slices.Sort(unknown)
	return unknown, nil
}

// unknownMember is where err says the decoder met an object member its type
// does not declare, or false where err is anything else.
func unknownMember(err error) (jsontext.Pointer, bool) {
	var semantic *jsonv2.SemanticError
	if !errors.As(err, &semantic) || !errors.Is(err, jsonv2.ErrUnknownName) {
		return "", false
	}
	return semantic.JSONPointer, true
}

// strikeMember deletes the member at, from doc as a generic decode holds it,
// and answers its path as a person reads one — `Ending.went_on`, a list's
// elements `Commands[].note` — and whether it struck anything.
//
// FROM EVERY ELEMENT of each list on the way, not the one the decoder pointed
// at: every element is the same type, so a member undeclared in one is in
// all, and naming it once per element would name one key a thousand times and
// decode a thousand-command push a thousand times to do it.
func strikeMember(doc any, at jsontext.Pointer) (path string, struck bool) {
	tokens := slices.Collect(at.Tokens())
	var b strings.Builder
	level := []any{doc}
	for i, token := range tokens {
		last := i == len(tokens)-1
		var next []any
		listed := false
		for _, node := range level {
			switch node := node.(type) {
			case map[string]any:
				if last {
					if _, held := node[token]; held {
						delete(node, token)
						struck = true
					}
					continue
				}
				if child, held := node[token]; held {
					next = append(next, child)
				}
			case []any:
				listed = true
				next = append(next, node...)
			}
		}
		if listed {
			b.WriteString("[]")
		} else {
			if b.Len() > 0 {
				b.WriteByte('.')
			}
			b.WriteString(token)
		}
		level = next
	}
	return b.String(), struck
}

// carries reports whether doc — a request's inputs as a generic decode holds
// them, under the members a refusal names them by (`args`, `actor`) — holds a
// member at path, a path as a refusal spells one ([strikeMember]): members
// joined by a dot, a list's elements by `[]` after the list's member.
//
// A MATCH OF THE WORDS, NOT A DECODE: a member whose own name holds a dot or
// `[]` is read as the words say, so a path naming one may miss it or meet
// another — and either costs only a request asked of a node in an order that
// is not the best for it ([Router.markUnread]), never a request answered
// wrong: a node passed over is asked last, still asked, and one asked first
// that refuses again is moved on from.
func carries(doc any, path string) bool {
	level := []any{doc}
	for _, segment := range strings.Split(path, ".") {
		name, lists := segment, 0
		for strings.HasSuffix(name, "[]") {
			name, lists = strings.TrimSuffix(name, "[]"), lists+1
		}
		var next []any
		for _, node := range level {
			if object, ok := node.(map[string]any); ok {
				if child, held := object[name]; held {
					next = append(next, child)
				}
			}
		}
		for ; lists > 0; lists-- {
			var elements []any
			for _, node := range next {
				if list, ok := node.([]any); ok {
					elements = append(elements, list...)
				}
			}
			next = elements
		}
		if len(next) == 0 {
			return false
		}
		level = next
	}
	return true
}

// looseDoc is raw as a generic decode holds it — objects, lists, numbers kept
// as their text, so none is read into a narrower type than its writer wrote —
// or why it holds no JSON document.
func looseDoc(raw []byte) (any, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ErrUnknownKeys is a request a data node's build cannot read whole — its
// arguments or the actor it runs as hold keys the operation's types on that
// build do not declare — or an answer this node's build cannot read whole, for
// an operation whose answer a caller carries into another request
// ([op.answeredWhole]).
//
// NOTHING OF THE REQUEST RAN — a write or a read alike: a read carrying a
// newer argument is refused the same way, never answered with the argument
// ignored. A data node refuses such a request before it waits for anything or
// calls its backend, and the router asks the next data node, as it does a node
// that ran nothing; only where none serves it is the caller handed this — the
// walk's account beside it ([ErrUnserved], which it wraps, so every caller that
// reads an estate nobody served reads this as one). A caller that keeps its
// write owed keeps it owed on it, as it does when the estate cannot be reached;
// every other caller surfaces it, and its words say what to do: a data node of
// the asking node's build is needed, so finish the upgrade.
//
// AN ANSWER IS THE OTHER DIRECTION: a newer data node's answer holding keys
// this build does not declare, which this node would drop and then carry on
// short. It is refused here, at the first node that gives it, since the
// answer IS the data and another node's would not change what this build can
// read — so it wraps no walk ([ErrUnknownKeys.Unserved] is nil): the request
// RAN on the node that answered, and it is this node's build that cannot read
// what it said.
type ErrUnknownKeys struct {
	// Op is the operation.
	Op string

	// Keys are the keys, each by its path from the request or the reply that
	// held it — `args.Ending.went_on`, `actor.provenance.Zone`,
	// `result.Manifest.files[].integrity`.
	Keys []string

	// Nodes are the nodes whose build refused them: the data nodes that would
	// not run the request, or for an answer the data node that gave it.
	Nodes []string

	// Answer is set where the keys were in an answer this node read, rather
	// than in a request a data node was sent.
	Answer bool

	// Unserved is the walk that found no data node to serve the request,
	// what every node it asked said: nil on one node's own refusal, and on an
	// answer this node refused.
	Unserved *ErrUnserved
}

func (e *ErrUnknownKeys) Error() string {
	keys, nodes := words.And(asSaid(e.Keys)), words.And(asSaid(e.Nodes))
	if e.Answer {
		return fmt.Sprintf("estate: the answer %s gave to %s holds %s, which this node's build does "+
			"not declare — carried on, it would be lost, so it is not read: this node needs the "+
			"build of %s, so finish the upgrade", nodes, e.Op, keys, nodes)
	}
	sentence := fmt.Sprintf("estate: %s holds %s, which the build of %s does not declare, so nothing "+
		"of it ran — a data node of the asking node's build is needed, so finish the upgrade",
		e.Op, keys, nodes)
	if e.Unserved != nil && e.Unserved.Detail != "" {
		sentence += " (" + e.Unserved.Detail + ")"
	}
	return sentence
}

// Unwrap is the walk that found no data node to serve the request, so a caller
// that asks for an estate nobody served finds one here; nil on a refusal
// made by one node, and on an answer refused.
func (e *ErrUnknownKeys) Unwrap() error {
	if e.Unserved == nil {
		return nil
	}
	return e.Unserved
}

// asSaid is every member of names as the sentence writes it: a key's path and a
// node's id are words the reader looks up as they are, never values to quote.
func asSaid(names []string) []words.Said {
	out := make([]words.Said, len(names))
	for i, name := range names {
		out[i] = words.Said(name)
	}
	return out
}

// prefixed is every path in unknown under the member of the envelope that held
// it — `args`, `actor` or `result`.
func prefixed(member string, unknown []string) []string {
	out := make([]string, len(unknown))
	for i, path := range unknown {
		out[i] = member + "." + path
	}
	return out
}
