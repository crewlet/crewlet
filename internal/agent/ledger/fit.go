package ledger

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
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
)

// Piece is one payload a render would show past its budget.
//
// # A ledger never cuts a payload; it asks for it to be fitted
//
// This package used to trim each of these itself — an argument to its first
// two hundred runes, a failed call's error document likewise, a round's
// output to its last four thousand — and the trimmed text was what the next
// round acted on. A message body cut at two hundred runes is not a shorter
// account of the message; it is its greeting. An error document cut there is
// its HTTP preamble without the line naming the missing scope.
//
// So a render now NAMES the pieces that exceed their budget, the caller has
// each one REWRITTEN to fit by the seat's auxiliary model
// ([github.com/crewlet/crewlet/internal/compact]), and hands the rewrites back
// as [Fitted]. The package still imports nothing from crewlet, because the
// rewriting is the caller's — this only says what needs it.
//
// A PIECE THE CALLER DID NOT FIT RENDERS WHOLE. That is the default because
// it is the only one that cannot lie: a missing rewrite costs prompt weight,
// where a fallback cut would reinstate exactly the defect above. A caller
// that cannot have a payload rewritten and must not carry it whole says so in
// the text it hands back — see ledgerfit — rather than this package guessing.
//
// Limit is BYTES, the unit a rewrite is held to; a text within it is within
// the same number of runes. Whether a payload NEEDS fitting is judged in
// runes, so an identifier in a script with multi-byte characters — a channel
// name, a page title — is never handed to a model to be paraphrased.
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
// type, and the rendered arguments still read as what was sent.
func valuePiece(value any, limit int) (Piece, bool) {
	if s, ok := value.(string); ok {
		if !over(s, limit) {
			return Piece{}, false
		}
		return Piece{Kind: PieceArgument, Text: s, Limit: limit}, true
	}
	dumped, err := json.Marshal(value)
	text := string(dumped)
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

// CallPieces is every payload [FormatCalls] would need fitted for these calls
// under these options, each once.
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
// WITHOUT HTML ESCAPING, which `json.Marshal` does by default. A ledger line
// has two audiences and the escaping is wrong for both: a model re-reads it
// every round, where `&` costs six bytes and `<` reads as noise;
// and a person reads it on the seat screen, where the line is rendered as the
// conversation ledger's `tool_calls`. Neither is an HTML document.
//
// This package imports nothing from crewlet, so the rule is spelled here
// rather than shared with `tools.RecordArgs`, which states the same one for
// the records a transcript renders. That is the boundary working, not a copy
// nobody noticed: an encoder is two lines and the import would drag the whole
// tool layer behind a type the turn context, the prompt builder and the API
// all hold.
func marshal(v map[string]any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return goString(v)
	}
	// Encode writes a trailing newline; a ledger line is one line.
	return strings.TrimSuffix(buf.String(), "\n")
}

// goString renders a value that JSON refuses. Only reached for arguments a
// tool surface should never have produced (a NaN, a cyclic structure); it
// exists so such a value costs a scruffy line rather than the whole record.
func goString(v any) string { return fmt.Sprintf("%v", v) }
