package cards

import (
	"encoding/json"
	"net/http"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
)

// Notice is a line the engine adds.
func Notice(tone wire.NoticeTone, text string) wire.Card {
	return wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{Tone: tone, Text: text}}
}

// The engine's own sentences for an answer no card is built from.
const (
	// TooLargeText is an answer past what one step reads, which is never
	// carded as though it were whole.
	TooLargeText = "This answer is larger than one step reads, so it is not shown. " +
		"Narrow the question, or open it in the app."
	// WithheldText stands in for a stored card whose app the person no
	// longer holds.
	WithheldText = "This result is hidden because it needs access you no longer have."
)

// Refusal reads the API's refusal envelope from an answer that was not a
// success: its code, its sentence and the grants that would admit the person.
// An answer that carries no envelope (a proxy's page, an empty body) is named
// by its status alone, in words a person reads.
func Refusal(status int, body []byte) wire.Refusal {
	var envelope struct {
		Error   string   `json:"error"`
		Message string   `json:"message"`
		Grants  []string `json:"grants"`
	}
	if json.Unmarshal(body, &envelope) == nil && envelope.Error != "" {
		return wire.Refusal{Code: envelope.Error, Message: envelope.Message,
			Grants: envelope.Grants}
	}
	return wire.Refusal{Code: "unavailable", Message: "The engine answered " +
		http.StatusText(status) + " and said nothing more."}
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
