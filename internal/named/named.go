// Package named says what a sentence the engine writes NAMES: the work item,
// change, commit, page or turn a person reading the sentence can open.
//
// # The sentence carries its references beside it, never inside it
//
// A feed line ("SRE was handed its next ready work item ENG-287") is a plain
// string to every reader that only reads it: a model's context, the CLI, a
// breadcrumb, a tab title, a search. A [Line] is that string together with
// the [Ref]s it names, each one the object's kind and id and the words the
// sentence spells it with. A reader that draws references (the dashboard)
// finds each ref's words in the text; every other reader keeps the text.
//
// Markup inside the string was the alternative, and it is wrong three ways:
// every plain reader would have to strip it, words somebody else wrote (a
// change's title, a message's subject) could forge a reference, and a summary
// cut to its bound could split one in half.
//
// # Built once, so the two cannot drift
//
// A ref's words are written into the text by the same call that records the
// ref ([Line.Name]), so they are a run of the sentence by construction. A
// reader locates them left to right, each search starting after the last
// match; a ref whose words were cut off simply goes unmarked. No offsets are
// carried, because a Go byte offset and a browser's UTF-16 index differ and a
// cut summary would move them.
//
// # An id is the address the object's own page reads
//
// Each [Kind] states its id's grammar. A reader resolves a ref (title, and
// whether the reader may open it) through the same authority the object's
// page uses, so a ref grants nothing: it only says what the words are.
package named

import (
	"slices"
	"strconv"
)

// Kind is what a [Ref] names.
type Kind string

// The kinds a sentence names, each with its id's grammar.
const (
	// WorkItem is a Worklet task by its id or its key (ENG-287).
	WorkItem Kind = "work_item"
	// Change is a Gitlet change by its reference, `<repository>!<number>`.
	Change Kind = "code_change"
	// Commit is a Gitlet commit, `<repository>@<object id>`.
	Commit Kind = "commit"
	// Repository is a Gitlet repository by its name.
	Repository Kind = "repository"
	// Page is a Doclet page by its id.
	Page Kind = "page"
	// Artifact is an artifact by its id.
	Artifact Kind = "artifact"
	// Turn is one turn by its id.
	Turn Kind = "turn"
	// Schedule is a schedule, `<scope type>/<scope id>/<name>`.
	Schedule Kind = "schedule"
	// Channel is an agent-to-agent channel by its id.
	Channel Kind = "a2a_channel"
	// Seat is a seat by its handle.
	Seat Kind = "seat"
	// Room is a Chatlet room by its id.
	Room Kind = "chat_room"
)

// Kinds is every kind, in a stable order.
var Kinds = []Kind{WorkItem, Change, Commit, Repository, Page, Artifact, Turn,
	Schedule, Channel, Seat, Room}

// Valid reports a kind this build names.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Ref is one object a sentence names.
type Ref struct {
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
	// Text is the words the sentence spells the object with, which are a
	// run of its text: a key, `web!42`, a commit's first twelve characters,
	// a page's title.
	Text string `json:"text"`
}

// Valid reports a ref a reader can locate and resolve.
func (r Ref) Valid() bool { return r.Kind.Valid() && r.ID != "" && r.Text != "" }

// Line is a sentence and what it names, in the order it names them.
type Line struct {
	Text string
	Refs []Ref
}

// Words starts a line with words that name nothing.
func Words(s string) Line { return Line{Text: s} }

// Words appends words that name nothing.
func (l Line) Words(s string) Line {
	l.Text += s
	return l
}

// Name appends a reference's words and records the reference. A ref that is
// not [Ref.Valid] is written as its words alone, so a payload missing an id
// still says what it said.
func (l Line) Name(r Ref) Line {
	l.Text += r.Text
	if r.Valid() {
		// CLIPPED, so two lines grown from one prefix never share a
		// backing array and overwrite each other's refs.
		l.Refs = append(slices.Clip(l.Refs), r)
	}
	return l
}

// Then appends another line, its references after this one's.
func (l Line) Then(o Line) Line {
	l.Text += o.Text
	if len(o.Refs) > 0 {
		l.Refs = append(slices.Clip(l.Refs), o.Refs...)
	}
	return l
}

// Item names a Worklet task: by its id where the sentence knows it, which a
// move or a rename keeps, else by its key; spelled as its key.
func Item(id, key string) Ref {
	switch {
	case key == "":
		return Ref{Kind: WorkItem, ID: id, Text: id}
	case id == "":
		return Ref{Kind: WorkItem, ID: key, Text: key}
	}
	return Ref{Kind: WorkItem, ID: id, Text: key}
}

// ChangeRef names a Gitlet change, spelled as its reference (`web!42`).
func ChangeRef(repository string, number int64) Ref {
	if repository == "" || number <= 0 {
		return Ref{}
	}
	ref := repository + "!" + strconv.FormatInt(number, 10)
	return Ref{Kind: Change, ID: ref, Text: ref}
}

// ShortCommit is how many characters of an object id a sentence spells.
const ShortCommit = 12

// CommitOf names a Gitlet commit, spelled as the first [ShortCommit]
// characters of its id.
func CommitOf(repository, oid string) Ref {
	if repository == "" || oid == "" {
		return Ref{Text: oid}
	}
	return Ref{Kind: Commit, ID: repository + "@" + oid, Text: oid[:min(len(oid), ShortCommit)]}
}

// Repo names a Gitlet repository, spelled as its name.
func Repo(name string) Ref { return Ref{Kind: Repository, ID: name, Text: name} }

// PageRef names a Doclet page by its id, spelled as text (its title, or its id).
func PageRef(id, text string) Ref { return Ref{Kind: Page, ID: id, Text: text} }

// ArtifactRef names an artifact by its id, spelled as text (its title).
func ArtifactRef(id, text string) Ref { return Ref{Kind: Artifact, ID: id, Text: text} }

// TurnRef names a turn, spelled as its id.
func TurnRef(id string) Ref { return Ref{Kind: Turn, ID: id, Text: id} }

// ScheduleRef names a schedule by its scope and name, spelled as text.
func ScheduleRef(scopeType, scopeID, name, text string) Ref {
	if scopeType == "" || scopeID == "" || name == "" {
		return Ref{Text: text}
	}
	return Ref{Kind: Schedule, ID: scopeType + "/" + scopeID + "/" + name, Text: text}
}

// ChannelRef names an agent-to-agent channel, spelled as its id.
func ChannelRef(id string) Ref { return Ref{Kind: Channel, ID: id, Text: id} }

// SeatRef names a seat by its handle, spelled as text.
func SeatRef(handle, text string) Ref { return Ref{Kind: Seat, ID: handle, Text: text} }

// RoomRef names a Chatlet room by its id, spelled as text.
func RoomRef(id, text string) Ref { return Ref{Kind: Room, ID: id, Text: text} }
