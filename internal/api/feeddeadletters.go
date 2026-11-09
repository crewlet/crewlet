package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE WAKE FEEDS' DEAD LETTERS, over HTTP for the retention gestures' reason:
// the entries are kept in the broker's dead-letter stream, which on the
// default topology is the engine's own embedded broker binding no socket, so
// `crewlet feeds dead-letters` and `discard` are clients of these routes.
//
//   - GET /feeds/dead-letters answers every wake feed's dead letters, by
//     feed, each entry by its id and what the feed recorded of its record —
//     never its frame. Every node serving the API answers alike, since the
//     entries are the broker's; a feed whose listing failed is named with
//     its error, and an entry this build cannot read with why, still listed
//     by its id.
//   - POST /feeds/dead-letters/{id}/discard?confirm={id} drops one entry,
//     whichever feed's subject holds it, through the engine's ledger — so no
//     node's alarm reading ever counts it as a wake the retention lost — and
//     answers the entry it dropped. An id no feed's subject holds, a
//     subscription's poison under another `dlq.` subject among them, is
//     `404 no_dead_letter` and removes nothing.
//
// Both are admin's, mounted on the deployment surface ([App.mountDeployment]),
// and the discard asks the recent proof every write there asks.

// FeedDeadLetters is the wake feeds' dead letters — see
// [engine.FeedDeadLetters].
//
// EXPORTED, as [NodeGate] is and for its reason: the caller converts a typed
// nil to a genuine one before handing it over, since a nil
// *engine.FeedDeadLetters inside a non-nil interface would pass [New]'s check
// and panic on the first read.
type FeedDeadLetters interface {
	// List is every wake feed's dead letters, a feed whose listing failed
	// named with its error.
	List(ctx context.Context) engine.FeedDeadLetterListing

	// Discard drops one entry, answering [engine.ErrNoDeadLetter] for an id
	// no feed holds and a [*engine.DeadLettersUnread] where a feed that
	// could not be listed might.
	Discard(ctx context.Context, req engine.FeedDeadLetterDiscard) (engine.FeedDeadLetter, error)
}

// mountFeedDeadLetters registers the listing and the discard, through the
// mount [App.mountDeployment] hands it — which is what decides the grant: the
// listing is a read of the deployment, and a discard gives up a wake for good.
func (a *App) mountFeedDeadLetters(mount func(string, authz.Effect, http.HandlerFunc)) {
	mount("GET /feeds/dead-letters", "", a.serveFeedDeadLetters)
	// A DISCARDED ENTRY IS A WAKE NO NODE WILL EVER DERIVE: nothing takes
	// it back, so it destroys rather than rewrites.
	mount("POST /feeds/dead-letters/{id}/discard", authz.EffectDestroys, a.serveDeadLetterDiscard)
}

// serveFeedDeadLetters answers GET /feeds/dead-letters: every wake feed's dead
// letters as the engine lists them, always 200 — a feed that could not be
// listed is named in the answer with its error, which is the honest reading of
// a listing that reached some feeds and not others.
func (a *App) serveFeedDeadLetters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.deadLetters.List(r.Context()))
}

// serveDeadLetterDiscard answers POST /feeds/dead-letters/{id}/discard.
func (a *App) serveDeadLetterDiscard(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	// THE CONFIRMATION ECHOES THE ID, the shape every destructive gesture on
	// this surface takes: what a discard gives up, no node ever derives
	// again.
	if raw == "" || r.URL.Query().Get("confirm") != raw {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeConfirmRequired,
			httpjson.Detail{
				"detail": "the id was not repeated: a discarded dead letter's wake is lost " +
					"for good, and no node will ever derive it",
				"hint": "repeat the id in ?confirm=",
			})
		return
	}
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeDeadLetterInvalid,
			httpjson.Detail{
				"detail": raw + " is not a dead letter's id, which is the broker's sequence " +
					"for the entry, a whole number above zero",
				"hint": "name the entry by the id GET /feeds/dead-letters lists it under",
			})
		return
	}
	caller, ok := auth.Caller(w, r)
	if !ok {
		return
	}
	// WHO PRESSED, as every record on this surface names a caller: the author
	// and the credential they pressed it through, on the engine's line.
	actor := iam.ActorFor(caller)
	entry, err := a.deadLetters.Discard(r.Context(), engine.FeedDeadLetterDiscard{ID: id, By: actor})
	var unread *engine.DeadLettersUnread
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, entry)
	case errors.Is(err, engine.ErrNoDeadLetter):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNoDeadLetter, httpjson.Detail{
			"detail": err.Error(),
			"hint": "GET /feeds/dead-letters lists every entry a wake feed holds, by id; " +
				"a subscription's poison, kept beside them in the dead-letter stream, is no feed's to " +
				"discard",
		})
	case errors.As(err, &unread):
		// NEVER A 404: the entry may be on the feed that did not answer.
		log.Warn("api_dead_letter_discard_unread", "by", actor.Name,
			"operator", actor.OperatorID, "id", id, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, discardRetry(err), httpjson.Detail{
			"detail": err.Error(),
			"hint":   "ask again once every wake feed's dead letters can be listed",
		})
	case errors.Is(err, queue.ErrUnavailable):
		log.Warn("api_dead_letter_discard_unanswered", "by", actor.Name,
			"operator", actor.OperatorID, "id", id, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, discardRetry(err), httpjson.Detail{
			"detail": err.Error(),
			"hint": "list the dead letters before asking again: the broker may have " +
				"dropped the entry although its answer was lost",
		})
	default:
		log.Warn("api_dead_letter_discard_failed", "by", actor.Name,
			"operator", actor.OperatorID, "id", id, "error", err)
		httpjson.FailWithFields(w, http.StatusInternalServerError,
			httpjson.CodeDeadLetterDiscardFailed, httpjson.Detail{
				"detail": err.Error(),
				"hint": "list the dead letters before asking again: the entry may be gone " +
					"although its answer was lost",
			})
	}
}

// discardRetry is the Retry-After a discard the broker did not answer carries:
// an election's length where a failure is one waiting may clear
// ([queue.ErrUnavailable]), the broker's own word for a condition — and none
// otherwise, which tells a client waiting will not help.
func discardRetry(err error) int {
	if errors.Is(err, queue.ErrUnavailable) {
		return httpjson.RetrySeconds(statelog.ElectionRetryHint)
	}
	return 0
}
