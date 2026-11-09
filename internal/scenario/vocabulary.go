package scenario

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Handles is every handle a fixture may use: one per generic role, and owner.
var Handles = []string{"owner", "pm", "designer", "app", "backend", "qa", "sre"}

// Statuses is every item status a timeline may name.
var Statuses = []string{"todo", "in_progress", "in_review", "done", "cancelled"}

// PhraseClass is the kind of words a comment carries.
type PhraseClass string

// The classes of words a comment may carry.
const (
	SayAck         PhraseClass = "ack"
	SayStatusNote  PhraseClass = "status_note"
	SayRequest     PhraseClass = "request"
	SayQuestion    PhraseClass = "question"
	SayAnswer      PhraseClass = "answer"
	SaySubstantive PhraseClass = "substantive"
)

// PhraseClasses is every [PhraseClass].
var PhraseClasses = []PhraseClass{SayAck, SayStatusNote, SayRequest, SayQuestion, SayAnswer, SaySubstantive}

// Valid reports whether c is a declared class.
func (c PhraseClass) Valid() bool { return slices.Contains(PhraseClasses, c) }

var phrases = map[PhraseClass][]string{
	SayAck:         {"Noted.", "Understood, nothing further.", "Thanks, no reply needed."},
	SayStatusNote:  {"Still waiting.", "No change since the last check."},
	SayRequest:     {"I need a decision on this."},
	SayQuestion:    {"Which option should we take?"},
	SayAnswer:      {"Take the first option."},
	SaySubstantive: {"Pushed the change.", "Review done.", "Ready for review."},
}

// Phrase renders the n-th words of a class, cycling through its list.
func Phrase(c PhraseClass, n int) string {
	list := phrases[c]
	return list[n%len(list)]
}

var (
	itemKey   = regexp.MustCompile(`^W-[1-9][0-9]*$`)
	changeKey = regexp.MustCompile(`^change-[1-9][0-9]*$`)
	numbered  = regexp.MustCompile(`_[0-9]+$`)
)

// Violation is one string a fixture may not carry, at its JSON path.
type Violation struct {
	File, Path, Value string
}

func (v Violation) String() string { return fmt.Sprintf("%s %s: %q", v.File, v.Path, v.Value) }

// Vocabulary decides whether a fixture may hold value at path; key says the
// value is an object key rather than a string value.
type Vocabulary func(path []string, value string, key bool) bool

// timelineKeys is every object key a timeline file has.
var timelineKeys = []string{"name", "pattern", "seats", "steps", "handle", "kind", "at",
	"actor", "verb", "item", "say", "mentions", "ask", "answers", "assignee", "status",
	"watcher", "expect", "woken"}

// TimelineVocabulary admits a timeline's own words and nothing else. A name is
// a pattern, optionally numbered, so a file name carries no words either.
func TimelineVocabulary(at []string, value string, key bool) bool {
	if key {
		return slices.Contains(timelineKeys, value)
	}
	if len(at) == 1 && at[0] == "name" {
		return Pattern(numbered.ReplaceAllString(value, "")).Valid()
	}
	return slices.Contains(Handles, value) || itemKey.MatchString(value) ||
		changeKey.MatchString(value) || slices.Contains(Statuses, value) ||
		PhraseClass(value).Valid() || Verb(value).Valid() ||
		SeatKind(value).Valid() || Pattern(value).Valid()
}

// CheckTree walks every .json file under fsys and reports each string value
// the vocabulary refuses. An unreadable or undecodable file is an error.
func CheckTree(fsys fs.FS, vocab Vocabulary) ([]Violation, error) {
	var out []Violation
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(name) != ".json" {
			return err
		}
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("scenario: decode %s: %w", name, err)
		}
		walk(doc, nil, func(at []string, value string, key bool) {
			if !vocab(at, value, key) {
				out = append(out, Violation{File: name, Path: "/" + strings.Join(at, "/"), Value: value})
			}
		})
		return nil
	})
	return out, err
}

func walk(v any, at []string, visit func([]string, string, bool)) {
	switch v := v.(type) {
	case map[string]any:
		for k, child := range v {
			visit(at, k, true)
			walk(child, append(slices.Clip(at), k), visit)
		}
	case []any:
		for i, child := range v {
			walk(child, append(slices.Clip(at), strconv.Itoa(i)), visit)
		}
	case string:
		visit(at, v, false)
	}
}
