package ledger

import (
	"fmt"
	"maps"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/plainjson"
)

// PieceKind is what an over-budget payload in a ledger render is.
type PieceKind string

const (
	// PieceArgument is one tool-call argument value: a message body, a
	// document, a diff.
	PieceArgument PieceKind = "argument"
	// PieceError is the text a failed tool call returned.
	PieceError PieceKind = "error"
	// PieceProduced is a prior round's produced text.
	PieceProduced PieceKind = "produced"
	// PieceAnswer is the text a successful write returned
	// ([FormatOptions.Answers]).
	PieceAnswer PieceKind = "answer"
)

// Piece is one payload a render would show past its budget.
//
// # A ledger never cuts a payload; it asks for it to be fitted
//
// A cut payload is not a shorter account (a body cut short is its greeting, an
// error its preamble without the missing scope), so a render names pieces over
// budget and the caller has the auxiliary model (internal/compact) rewrite them into
// [Fitted]. An unfitted piece renders whole: a fallback cut would reinstate the
// defect. Limit is BYTES; whether a payload needs fitting is judged in runes,
// so a multi-byte identifier is never paraphrased.
type Piece struct {
	Kind  PieceKind
	Text  string
	Limit int
}

// Fitted is what the caller made of each piece: a rewrite, marked as one, or
// an honest account of a payload that could not be rewritten. Absent pieces
// render whole.
type Fitted map[Piece]string

// text is what a render shows for one payload.
func (f Fitted) text(p Piece) string {
	if fitted, ok := f[p]; ok {
		return fitted
	}
	return p.Text
}

// over reports whether text exceeds a rune budget. A budget of 0 or less is
// unbounded, which is the verbatim contract Review's evidence log depends on.
func over(text string, limit int) bool {
	return limit > 0 && utf8.RuneCountInString(text) > limit
}

// valuePiece is the piece an argument value would need, or false when the
// value renders as itself.
//
// A NON-STRING VALUE is judged by its JSON, and only becomes a piece when that
// JSON is over the budget — so a number or a short object keeps its native
// type, and the rendered arguments still read as what was sent. The JSON is the
// line's own ([marshal]'s, through [plainjson.Marshal], unescaped): a piece is
// what the rewrite is handed and what the line then shows, and an object
// holding markup was weighed and sent as `\u003c` escapes — six bytes for each
// `<`, so a value the line would carry whole was judged over its budget and
// handed to a model to decode.
func valuePiece(value any, limit int) (Piece, bool) {
	if s, ok := value.(string); ok {
		if !over(s, limit) {
			return Piece{}, false
		}
		return Piece{Kind: PieceArgument, Text: s, Limit: limit}, true
	}
	encoded, err := plainjson.Marshal(value, "")
	text := string(encoded)
	if err != nil {
		// Unmarshalable (a channel, a func, a NaN) — there is nothing to
		// preserve the type of, so it is judged by its Go rendering.
		text = goString(value)
	}
	if !over(text, limit) {
		return Piece{}, false
	}
	return Piece{Kind: PieceArgument, Text: text, Limit: limit}, true
}

// ShowsWhole reports whether a call's line rendered under opts shows the
// argument value as itself, not a fitted rewrite or an omission by size and
// digest (ledgerfit), neither of which is the value.
//
// It is [valuePiece]'s answer, never a second rule: where the caller fits every
// piece [CallPieces] names (as ledgerfit.Fit does), a value is shown whole
// exactly where [renderArgs] carries it as itself, judged in runes and, for a
// non-string, by the line's own JSON. An unfitted piece renders whole
// ([Fitted]) while this answers false, the safe side for a caller reading false
// as "may equal anything". A stricter rule would take a shown value for
// anything; a looser one would compare a rewrite as the value.
func ShowsWhole(value any, opts FormatOptions) bool {
	_, piece := valuePiece(value, opts.ValueLimit)
	return !piece
}

// CallPieces is every payload [FormatCalls] would need fitted for these calls
// under these options, each once. Of a [FormatCallGroups] render it is the
// pieces of the groups' concatenation (slices.Concat), which shows exactly the
// calls the groups show, since one read cap runs across the groups in order as
// it runs down one list. Named group by group, a read the cap leaves out of a
// later group would still be fitted, its rewrite spent and never shown.
func CallPieces(calls []Call, opts FormatOptions) []Piece {
	var out []Piece
	seen := map[Piece]bool{}
	add := func(p Piece) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, call := range visible(calls, opts) {
		for _, key := range slices.Sorted(maps.Keys(call.Args)) {
			if p, ok := valuePiece(call.Args[key], opts.ValueLimit); ok {
				add(p)
			}
		}
		if call.Failed && over(call.Result, opts.ValueLimit) {
			add(Piece{Kind: PieceError, Text: call.Result, Limit: opts.ValueLimit})
		}
		if answers(call, opts) && over(call.Result, opts.ValueLimit) {
			add(Piece{Kind: PieceAnswer, Text: call.Result, Limit: opts.ValueLimit})
		}
	}
	return out
}

// IterationPieces is every payload [RenderIterations] would need fitted.
func IterationPieces(records []Iteration, skip []string) []Piece {
	var out []Piece
	seen := map[Piece]bool{}
	for _, rec := range records {
		pieces := CallPieces(rec.Calls, Format(skip, rec.Reads))
		if over(rec.Text, RenderedArtifactLimit) {
			pieces = append(pieces, Piece{Kind: PieceProduced, Text: rec.Text, Limit: RenderedArtifactLimit})
		}
		for _, p := range pieces {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// renderArgs serialises one call's arguments, each value as itself or as its
// fitted text.
//
// Output key order is json.Marshal's — which for a map is sorted.
// Deterministic rather than dependent on the order the model happened to emit
// its arguments in, so two identical calls render identically and a diff of
// two ledger blocks is readable.
func renderArgs(args map[string]any, opts FormatOptions) string {
	if len(args) == 0 {
		return ""
	}
	shown := make(map[string]any, len(args))
	for k, v := range args {
		if p, ok := valuePiece(v, opts.ValueLimit); ok {
			shown[k] = opts.Fitted.text(p)
			continue
		}
		shown[k] = v
	}
	return marshal(shown)
}

// marshal renders a map as compact JSON, degrading to a Go rendering rather
// than to an error: a ledger line is evidence, and evidence that vanished
// because one argument held an unmarshalable value is the worst outcome
// available.
//
// WITHOUT HTML ESCAPING, through [plainjson.Marshal], whose package doc says
// why that is not `json.Marshal`. A ledger line has two audiences and the
// escaping is wrong for both: a model re-reads it every round, where `&` costs
// six bytes and `<` reads as noise; and a person reads it on the dashboard, in
// the prompt the round was sent. Neither is an HTML document.
func marshal(v map[string]any) string {
	text, err := plainjson.Marshal(v, "")
	if err != nil {
		return goString(v)
	}
	return string(text)
}

// goString renders a value that JSON refuses. Only reached for arguments a
// tool surface should never have produced (a NaN, a cyclic structure); it
// exists so such a value costs a scruffy line rather than the whole record.
func goString(v any) string { return fmt.Sprintf("%v", v) }
