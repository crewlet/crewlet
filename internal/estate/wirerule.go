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

// The wire's rule for what an operation takes, its arguments and its actor
// (package doc, "A request is read whole, or runs nowhere"): a key on the wire
// is a value the serving build must read, and a build that cannot read one runs
// nothing. Both halves are encoding/json's own options over json.Marshal's and
// json.Unmarshal's defaults ([json.DefaultOptionsV1]), so no rule here can
// fall out of step with the decoder. It binds a field a later build REMOVES
// alike: the newer node refuses an older writer's, so a build stops SETTING a
// field before a later one deletes it. In process ([Router.tryLocal]) nothing
// is encoded and none of this runs.
var (
	// inputEncoding leaves every struct field at its zero off the wire, so a
	// field a later build adds is sent only by a write that uses it; its zero
	// must mean what an older build decodes the absence as ([define]).
	inputEncoding = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.OmitZeroStructFields(true))
	// inputDecoding refuses every member no field declares, at every depth (a
	// map's keys and a json.RawMessage's inside are data), as a
	// [jsonv2.SemanticError] naming its path, which DisallowUnknownFields lacks.
	//
	// A type that decodes itself is held to it only through UnmarshalJSONFrom
	// calling [jsonv2.UnmarshalDecode] with no options of its own: an
	// UnmarshalJSON's json.Unmarshal restarts from the defaults and drops what it
	// does not declare (a MarshalJSON likewise sends every zero), so
	// wirerule_test.go's decodesItself holds each such type to saying why it
	// drops nothing. An actor has no encoder: a write whose actor may write
	// nothing is refused before it is asked anywhere ([actorWrites]).
	inputDecoding = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsonv2.RejectUnknownMembers(true),
		json.ReportErrorsWithLegacySemantics(false))
)

// encodeInput is v — an operation's arguments, its actor, or an answer a caller
// carries into another request ([op.answeredWhole]) — under [inputEncoding].
func encodeInput(v any) ([]byte, error) {
	return jsonv2.Marshal(v, inputEncoding)
}

// decodeInput decodes raw into into, a pointer, under [inputDecoding], and
// answers every member raw holds that into's type does not declare, by path
// ([strikeMember]), sorted; err is any other failure. Where it answers members,
// into is partly filled and never to be used: the caller refuses the request.
//
// EVERY MEMBER, NOT THE FIRST: the decoder stops at its first refusal, so each
// named member is struck from a copy and the copy decoded again until whole.
// The decoder alone decides what a type declares (case-insensitive names,
// promoted fields, `-` tags), so this never disagrees with it.
func decodeInput(raw []byte, into any) ([]string, error) {
	decoded := jsonv2.Unmarshal(raw, into, inputDecoding)
	at, refused := unknownMember(decoded)
	if !refused {
		return nil, decoded
	}
	// Numbers kept as their text: one read into a narrower type would fail
	// the next decode on something other than a member, leaving the rest
	// unnamed.
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
			// Nothing to strike: a loop that cannot shorten the
			// document would name the member for ever.
			break
		}
		rest, encodeErr := json.Marshal(doc)
		if encodeErr != nil {
			// Never, for a document a decode built; said rather than
			// swallowed should one ever not encode.
			return nil, fmt.Errorf("encode the document again to name what it holds: %w", encodeErr)
		}
		// Whole, or failing for another reason, which the members already
		// named outrank: this build cannot read the request either way.
		if at, refused = unknownMember(jsonv2.Unmarshal(rest, reflect.New(target).Interface(),
			inputDecoding)); !refused {
			break
		}
	}
	// Sorted: the order met is the encoder's, and two nodes naming them in
	// different orders would be two refusals of one request.
	slices.Sort(unknown)
	return unknown, nil
}

// unknownMember is where err says the decoder met an object member its type
// does not declare, or false where err is anything else. A code write's
// carrier's kind or app this build does not know counts as one: only a newer
// build sends one, so its type's decoder refuses it with jsonv2.ErrUnknownName
// at its place (internal/gitlet's unknownToThisBuild decides), and the walk
// asks a node that knows it rather than calling a valid request malformed.
// [strikeMember] strikes args.written_for.kind as a member;
// args.written_for.apps[] names a list whose element it cannot strike. In
// process a request's Validate meets the same value first and refuses it alike
// (gitlet.IsUnknownToThisBuild), which the walk answers as the wire does
// ([unreadHere]); a carrier a stored row holds counts, since another build may
// have written it.
func unknownMember(err error) (jsontext.Pointer, bool) {
	var semantic *jsonv2.SemanticError
	if !errors.As(err, &semantic) || !errors.Is(err, jsonv2.ErrUnknownName) {
		return "", false
	}
	return semantic.JSONPointer, true
}

// strikeMember deletes the member at from doc, a generic decode, and answers
// its path as a person reads one (`Ending.went_on`, `Commands[].note`) and
// whether it struck anything. It strikes from every element of each list on
// the way, not only the one pointed at: they share a type, and striking per
// element would name one key, and decode a thousand-command push, a thousand
// times.
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

// carries reports whether doc, a request's inputs as a generic decode holds
// them (under `args`, `actor`), holds a member at path as [strikeMember]
// spells one. It matches the words, not a decode: a member whose name holds a
// dot or `[]` may be missed or confused, which costs only a node asked in a
// worse order ([Router.markUnread]), never a wrong answer — a node passed over
// is still asked last, and one that refuses again is moved on from.
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

// looseDoc is raw as a generic decode holds it, numbers kept as their text so
// none is read narrower than its writer wrote, or why it holds no JSON.
func looseDoc(raw []byte) (any, error) {
	var doc any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ErrUnknownKeys is a request a data node's build cannot read whole (its
// arguments or actor hold keys that build does not declare), or an answer this
// node's build cannot read whole, of an operation whose answer a caller
// carries into another request ([op.answeredWhole]).
//
// Nothing of a refused request ran, read or write: the data node refuses before
// waiting or calling its backend, and the router asks the next. Only where none
// serves it is the caller handed this, wrapping the walk's [ErrUnserved]; a
// caller that keeps its write owed keeps it owed, as when the estate cannot be
// reached. A refused ANSWER wraps no walk (Unserved is nil): the request ran,
// the answer is the data, and another node's would read no better.
type ErrUnknownKeys struct {
	// Op is the operation.
	Op string

	// Keys are the keys, each by its path from the envelope that held it:
	// `args.Ending.went_on`, `actor.provenance.Zone`,
	// `result.Manifest.files[].integrity`, or `args.written_for.kind` and
	// `args.written_for.apps[]` for a carrier's unknown kind or app.
	Keys []string

	// Nodes are the nodes whose build refused them: the data nodes that would
	// not run the request, or for an answer the data node that gave it.
	Nodes []string

	// Answer is set where the keys were in an answer this node read, rather
	// than in a request a data node was sent.
	Answer bool

	// Unserved is the walk that found no data node to serve the request: nil
	// on one node's own refusal, and on an answer this node refused.
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

// Unwrap is [ErrUnknownKeys.Unserved], so a caller asking for an estate nobody
// served finds one here.
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
