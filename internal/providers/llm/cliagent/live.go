package cliagent

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// eventSeparator is what joins the text of two stream events into one answer,
// in [extractStream] and in the live view alike, so the view a watcher was
// shown and the answer the call returns are spelled the same.
const eventSeparator = "\n"

// liveAnswer is one streamed call's view of its answer, as its CLI's stream
// arrives: what is shown, and the reconciliation that makes it end as the
// answer exactly.
//
// THE GUARANTEE: on a call that succeeds, the deltas sent after the last
// [llm.Delta.Retract] concatenate to exactly the [llm.Completion.Content] it
// returns. The view is built to agree with that content as it arrives (the
// envelope's prose through [MessageStream], plain text trimmed as
// [extractStream] trims it), and [liveAnswer.settle] closes whatever gap is
// left: the tail the view had not reached, or a retraction and the whole
// answer where the view and the parser disagree.
type liveAnswer struct {
	req     llm.Request
	profile *cliprofile.Profile
	view    func(string) string
	// joined says an event's text has been read, so the next is preceded
	// by [eventSeparator].
	joined bool
	shown  strings.Builder
}

// newLiveAnswer starts the view of one call. envelope says the prompt carried
// the response contract, so the reply is read as an envelope and only its
// message prose is shown.
func newLiveAnswer(req llm.Request, p *cliprofile.Profile, envelope bool) *liveAnswer {
	l := &liveAnswer{req: req, profile: p}
	if envelope {
		l.view = NewMessageStream().Write
	} else {
		l.view = (&trimmedView{}).Write
	}
	return l
}

// line reads one line of the CLI's stdout.
//
// Fragments come from the profile's delta paths where it has them, which
// concatenate as they are; otherwise from its text paths, event by event,
// through the SAME event filter the answer is read with, so a profile that
// takes its answer from one terminal event delivers it in one delta at the end
// rather than pushing a tool's output through as though the model had said it.
func (l *liveAnswer) line(raw string) {
	doc, ok := decodeObject(strings.TrimSpace(raw))
	if !ok {
		return
	}
	var text string
	if len(l.profile.DeltaPaths) > 0 {
		// Whether this one event matched is not the question here: most
		// events in a stream carry no fragment at all.
		text, _ = deltaOf(*l.profile, doc)
	} else {
		chunk, _ := textOf(*l.profile, doc)
		if chunk != "" && l.joined {
			text = eventSeparator + chunk
		} else {
			text = chunk
		}
		l.joined = l.joined || chunk != ""
	}
	if text == "" {
		return
	}
	l.show(l.view(text))
}

// show sends what the view revealed.
func (l *liveAnswer) show(text string) {
	if text == "" {
		return
	}
	l.shown.WriteString(text)
	l.req.Send(llm.Delta{Content: text})
}

// settle reconciles what was shown with the answer the call returns.
func (l *liveAnswer) settle(content string) {
	tail, restart := llm.Reconcile(l.shown.String(), content)
	if restart {
		l.req.Send(llm.Delta{Retract: true})
	}
	l.req.Send(llm.Delta{Content: tail})
	l.shown.Reset()
	l.shown.WriteString(content)
}

// trimmedView shows plain text as [strings.TrimSpace] will leave it: nothing
// of its leading whitespace, and a run of whitespace held back until the text
// after it arrives, so the end of the answer is never shown with whitespace
// the answer does not have.
type trimmedView struct {
	started bool
	held    strings.Builder
}

// Write reads the next fragment and returns what it reveals.
func (v *trimmedView) Write(fragment string) string {
	var out strings.Builder
	for i := 0; i < len(fragment); {
		r, size := utf8.DecodeRuneInString(fragment[i:])
		piece := fragment[i : i+size]
		i += size
		if unicode.IsSpace(r) {
			if v.started {
				v.held.WriteString(piece)
			}
			continue
		}
		v.started = true
		out.WriteString(v.held.String())
		v.held.Reset()
		out.WriteString(piece)
	}
	return out.String()
}
