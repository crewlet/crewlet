package cards

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
)

// Notice is a line the engine adds.
func Notice(tone wire.NoticeTone, text string) wire.Card {
	return wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{Tone: tone, Text: text}}
}

// WithheldText stands in for a stored card whose app the person no longer
// holds: the engine's own sentence for an answer no card is built from.
const WithheldText = "This result is hidden because it needs access you no longer have."

// Oversize is the card of a read whose answer was past what one step reads
// (ADR-0061): never drawn as though it held the answer, but named for what it
// read, with the screen that shows all of it and what would narrow it.
func Oversize(title string, open *wire.Link, narrow []string) wire.Card {
	return wire.Card{Type: wire.CardOversize, Oversize: &wire.OversizeCard{Title: title,
		Open: open, Narrow: narrow}}
}

// Error is the error card for a refusal, in the engine's own words: phrase is
// the tool's action phrase, so "You do not have permission to file tasks." is
// never model text, and retryable is a failure that clears by waiting.
func Error(r wire.Refusal, phrase string, retryable bool) wire.Card {
	return wire.Card{Type: wire.CardError, Error: &wire.ErrorCard{Refusal: r, Phrase: phrase,
		Retryable: retryable}}
}

// Retryable reports a status a later attempt of the same request may clear: a
// node that is behind or busy, never a refusal of the request itself.
func Retryable(status int) bool {
	return status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests
}

// Visible is a stored card as the person may see it now: the card itself when
// they hold every app it was built from, and a notice in its place when they
// no longer do.
//
// A STORED CARD IS RE-CHECKED ON EVERY READ, because a transcript outlives the
// grants it was made under: a table of tasks the person read last week is
// tracker data, and somebody whose tracker app was withdrawn since must not
// read it back out of their conversation. The apps are the ones the card was
// recorded with (the tool's own), judged by [iam.Principal.Can] so an
// administrator holds every app and a name this build does not know opens
// nothing.
func Visible(apps []string, card wire.Card, p iam.Principal) (wire.Card, bool) {
	for _, app := range apps {
		if !p.Can(iam.Grant(app)) {
			return Notice(wire.NoticeWarning, WithheldText), false
		}
	}
	return card, true
}
