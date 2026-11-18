package workapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events/types"
)

// The company's own chat on this surface: the three verbs no tool makes.
// Every other chat gesture is a catalogue tool behind the act route and MCP
// (ADR-0024). Erase, prune and a room's horizon destroy what people said with
// nothing to undo, so each is decided as the purges are (admin's, a person's
// or a credential's, and a recent proof: [authz.ActionChatErase],
// [authz.ActionChatPrune], [authz.ActionChatRetention]) with no row read, and
// audited as `operator_acted` ([operator.AuditGesture]). None is SEATED (the
// store records the actor under their own name) and none READS the room: an
// administrator is in no private room they were not invited to, so the
// confirmation repeats what the request destroys (an erase's COUNT, a prune's
// CUTOFF), not something read off the room.

// The operation id is [keyedOp]'s over the key and what the request ASKS, in
// canonical form (the erase set trimmed, deduplicated, sorted by
// [chatlet.EraseSet]; the cutoff in UTC nanoseconds; the horizon as days or
// null), so the same key with other messages, cutoff or horizon is another
// operation, never answered `applied` while nothing it names was destroyed.

// ChatStore is chat's write side for the three verbs no tool makes, through
// the estate router.
type ChatStore interface {
	Erase(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		messageIDs []string, reason string) (chatlet.Written, error)
	Prune(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		cutoff time.Time) (chatlet.Written, error)
	SetRetention(ctx context.Context, actor chatlet.Actor, channelID string,
		days *int) (chatlet.Written, error)
}

// servesChat reports whether these halves hold chat's destructions.
func (h Halves) servesChat() bool { return h.ChatStore != nil }

// onChat mounts a route of chat's half.
func (s *Service) onChat(h servedHandler) http.HandlerFunc {
	return s.on(Halves.servesChat, h)
}

// chatRoutes are chat's three destructions, each decided on the route with no
// row read.
func (s *Service) chatRoutes(mount mounter) {
	route := func(pattern string, p authz.Policy, h servedHandler) {
		mount(pattern, p, s.onChat(h))
	}
	route("POST /chatlet/channels/{channel_id}/erase",
		authz.Policy{Action: authz.ActionChatErase}.Destroying(), (*served).postChatErase)
	route("POST /chatlet/channels/{channel_id}/prune",
		authz.Policy{Action: authz.ActionChatPrune}.Destroying(), (*served).postChatPrune)
	route("PUT /chatlet/channels/{channel_id}/retention",
		authz.Policy{Action: authz.ActionChatRetention}, (*served).putChatRetention)
}

// postChatErase destroys named messages of one room for good.
//
// # The body names them, and the query repeats how many
//
// `{"message_ids": [...], "reason": "..."}` — the REASON IN THE BODY, unlike
// the purges' query parameter, because it is the account a compliance erase
// leaves on a record every node keeps, a sentence or two, and a URL carries
// what it holds into every proxy's access log. The reason is required: the
// messages are gone, and the erase record's reason is the only account of
// them that survives. `?confirm=` is the number of messages the request
// names, once duplicates are folded, checked before anything is written.
func (s *served) postChatErase(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "message_ids", "reason") {
		return
	}
	ids, ok := idList(w, args["message_ids"])
	if !ok {
		return
	}
	reason, _ := args["reason"].(string)
	reason = strings.TrimSpace(reason)
	if reason == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "Give a `reason` in the body. Erased " +
				"messages are gone for good, and the reason is all that's kept " +
				"about them."})
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	if confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "Repeat how many messages you're erasing " +
				"in ?confirm=. An erase can't be undone."})
		return
	}
	set := chatlet.EraseSet(ids)
	if confirm != strconv.Itoa(len(set)) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q, but the "+
				"request names %d messages. Nothing was erased.", confirm, len(set))})
		return
	}
	channelID := r.PathValue("channel_id")
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.chatActor(w, r)
	if !ok {
		return
	}
	opID, ok := keyedOp(w, r, key, "chat-erase", channelID, eraseArgs(set, reason))
	if !ok {
		return
	}
	written, err := s.ChatStore.Erase(r.Context(), actor, opID, channelID, set, reason)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionChatErase), key, written.Outcome.Outcome,
		written.Outcome.Position, err)
	if err != nil {
		log.Warn("api_chat_erase_failed", "channel", channelID, "actor", actor.Name(),
			"error", err)
		failErr(w, r, err, key)
		return
	}
	// LOGGED AT INFO WITH THE REASON, for the purge's reason: what it was
	// about no longer exists to be inspected.
	log.Info("chat_erased", "channel", channelID, "messages", len(set),
		"actor", actor.Name(), "reason", reason, "outcome", written.Outcome.Outcome)
	answer(w, key, written.Outcome.Outcome, written.Outcome.Unvouched, map[string]any{
		"channel_id": channelID, "messages": len(set),
		"outcome":  string(written.Outcome.Outcome),
		"position": positionOf(written.Outcome.Position),
	})
}

// eraseArgs is what an erase asks, as its operation is bound to it: the
// canonical set and the reason.
func eraseArgs(set []string, reason string) map[string]any {
	return map[string]any{"message_ids": set, "reason": reason}
}

// postChatPrune destroys everything one room said before an instant.
//
// `?cutoff=` is that instant, RFC 3339, and `?confirm=` repeats it: the same
// instant, in any zone. A prune takes no body and no reason, as the retention
// duty's own does not — it enforces a horizon somebody already decided, where
// an erase is a judgement about particular messages.
func (s *served) postChatPrune(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args) {
		return
	}
	cutoff, ok := queryInstant(w, r, "cutoff", "Give the cutoff in ?cutoff= "+
		"as an RFC 3339 time. Everything the room said before it is deleted, and "+
		"a prune can't be undone.")
	if !ok {
		return
	}
	confirm, ok := queryInstant(w, r, "confirm", "Repeat the cutoff in "+
		"?confirm=. A prune can't be undone.")
	if !ok {
		return
	}
	if !confirm.Equal(cutoff) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %s, but "+
				"?cutoff= says %s. Nothing was deleted.",
				confirm.Format(time.RFC3339Nano), cutoff.Format(time.RFC3339Nano))})
		return
	}
	channelID := r.PathValue("channel_id")
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.chatActor(w, r)
	if !ok {
		return
	}
	opID, ok := keyedOp(w, r, key, "chat-prune", channelID, pruneArgs(cutoff))
	if !ok {
		return
	}
	written, err := s.ChatStore.Prune(r.Context(), actor, opID, channelID, cutoff)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionChatPrune), key, written.Outcome.Outcome,
		written.Outcome.Position, err)
	if err != nil {
		log.Warn("api_chat_prune_failed", "channel", channelID, "actor", actor.Name(),
			"error", err)
		failErr(w, r, err, key)
		return
	}
	log.Info("chat_pruned", "channel", channelID,
		"cutoff", cutoff.Format(time.RFC3339Nano), "actor", actor.Name(),
		"outcome", written.Outcome.Outcome)
	answer(w, key, written.Outcome.Outcome, written.Outcome.Unvouched, map[string]any{
		"channel_id": channelID, "cutoff": cutoff.Format(time.RFC3339Nano),
		"outcome":  string(written.Outcome.Outcome),
		"position": positionOf(written.Outcome.Position),
	})
}

// pruneArgs is what a prune asks, as its operation is bound to it: the cutoff
// in UTC nanoseconds, so one instant written in two zones is one operation.
func pruneArgs(cutoff time.Time) map[string]any {
	return map[string]any{"cutoff": cutoff.UTC().UnixNano()}
}

// putChatRetention sets one room's own horizon, or takes it back.
//
// `{"retention_days": n}` keeps the room's messages n days, `0` for ever, and
// `{"retention_days": null}` returns the room to the company's horizon
// (`chat.chatlet.message_retention_days`). REQUIRED, as a value or a null: an
// absent one would read as the company's horizon and could destroy the past of
// a room set to keep for ever.
// A shorter horizon is a prune on a delay, so it is decided and CONFIRMED as
// one: `?confirm=` repeats the body's horizon (days, `0`, or `company` for
// null, per [chatlet.HorizonConfirmation]) before anything is written. Every
// horizon is confirmed, a longer one too: whether it shortens the room's
// depends on what the room had, which this route does not read.
func (s *served) putChatRetention(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "retention_days") {
		return
	}
	held, stated := args["retention_days"]
	if !stated {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "Say how long the room keeps messages in " +
				"`retention_days`: a number of days, 0 for forever, or null for the " +
				"company's setting."})
		return
	}
	days, ok := retentionDays(w, held)
	if !ok {
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	if confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "Repeat it in ?confirm=: the number of " +
				"days, 0 for forever, or `company` for the company's setting. A " +
				"shorter time deletes older messages, and that can't be undone."})
		return
	}
	if want := chatlet.HorizonConfirmation(days); confirm != want {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q, but the "+
				"body asks for %q. Nothing was changed.", confirm, want)})
		return
	}
	channelID := r.PathValue("channel_id")
	key, ok := operationKey(w, r)
	if !ok {
		return
	}
	actor, ok := s.chatActor(w, r)
	if !ok {
		return
	}
	// THE STORE BINDS THE KEY TO WHAT IS ASKED ([chatlet.Actor.OpKey]): the
	// horizon is in the operation, so the same key with another one is
	// another write.
	actor.OpKey = key
	written, err := s.ChatStore.SetRetention(r.Context(), actor, channelID, days)
	operator.AuditGesture(r.Context(), s.audit, types.TransportWork,
		string(authz.ActionChatRetention), key, written.Outcome.Outcome,
		written.Outcome.Position, err)
	if err != nil {
		log.Warn("api_chat_retention_failed", "channel", channelID,
			"actor", actor.Name(), "error", err)
		failErr(w, r, err, key)
		return
	}
	log.Info("chat_retention_set", "channel", channelID, "retention_days", held,
		"actor", actor.Name(), "outcome", written.Outcome.Outcome)
	var answered any
	if days != nil {
		answered = *days
	}
	answer(w, key, written.Outcome.Outcome, written.Outcome.Unvouched, map[string]any{
		"channel_id": channelID, "retention_days": answered,
		"outcome":  string(written.Outcome.Outcome),
		"position": positionOf(written.Outcome.Position),
	})
}

// chatActor is the request's chat actor: [builtin.PrincipalChatActor], with no
// key of its own — an erase and a prune take their whole operation id from
// [keyedOp], and a retention's key is set where it is written.
func (s *Service) chatActor(w http.ResponseWriter, r *http.Request) (chatlet.Actor, bool) {
	actor, err := builtin.PrincipalChatActor(r.Context(), nil)
	if err != nil {
		// UNREACHABLE behind the router, for [Service.actor]'s reason.
		operator.RefuseDecision(w, r, "", authz.Decision{Err: err})
		return chatlet.Actor{}, false
	}
	return actor, true
}

// idList reads an erase's message ids: a JSON list of strings. Anything else is
// the caller's to change, refused naming the field — an id that is not text
// would be erased as somebody's guess of what it meant.
func idList(w http.ResponseWriter, held any) ([]string, bool) {
	list, ok := held.([]any)
	if !ok {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "List the messages to erase by id in " +
				"the body's `message_ids`."})
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, entry := range list {
		id, ok := entry.(string)
		if !ok {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": fmt.Sprintf("`message_ids` holds %v. "+
					"Each entry is a message id, as text.", entry)})
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, true
}

// queryInstant reads one RFC 3339 instant from the query, refusing a missing
// one as `invalid_query` with need and a malformed one naming the parameter.
func queryInstant(w http.ResponseWriter, r *http.Request, name, need string) (
	time.Time, bool) {

	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": need})
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": fmt.Sprintf("?%s= is %q, which isn't an "+
				"RFC 3339 time like 2026-06-01T00:00:00Z.", name, raw)})
		return time.Time{}, false
	}
	return at.UTC(), true
}

// retentionDays reads a room's horizon off the body: a whole number of days, or
// null for the company's own. Its RANGE is the store's to judge, which refuses
// one it does not keep naming the field; what is refused here is a value that
// is no number of days at all — a fraction, which the decoder would truncate
// into another horizon, or text.
func retentionDays(w http.ResponseWriter, held any) (*int, bool) {
	if held == nil {
		return nil, true
	}
	number, ok := held.(json.Number)
	if ok {
		if n, err := strconv.ParseInt(string(number), 10, 64); err == nil &&
			n >= math.MinInt32 && n <= math.MaxInt32 {
			days := int(n)
			return &days, true
		}
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
		map[string]string{"detail": fmt.Sprintf("`retention_days` is %v. Use "+
			"a whole number of days, 0 for forever, or null for the company's "+
			"setting.", held)})
	return nil, false
}
