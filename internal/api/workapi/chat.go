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

// THE COMPANY'S OWN CHAT, ON THIS SURFACE: the three verbs no tool makes.
//
// Every ordinary chat gesture — a post, a reaction, a room made or its
// settings changed, a read cursor — is a catalogue tool, reached through the
// act route and the operator's MCP and the one dispatch behind both (ADR-0024),
// so there is no route here for any of them. What IS here is what no seat is
// given and so no tool serves: erasing messages, pruning a room's past and
// setting a room's own horizon. Each destroys what people said, now or on a
// delay, with nothing to undo it, so each is decided as the purges are —
// ADMIN'S, A PERSON'S OR A CREDENTIAL'S AND A RECENT PROOF'S
// ([authz.ActionChatErase], [authz.ActionChatPrune],
// [authz.ActionChatRetention]) — decided on the route with no row read, and
// audited through the same `operator_acted` record a tool's call leaves
// ([operator.AuditGesture]).
//
// # Said in nobody's voice, and read from nowhere
//
// None of the three is said in the room, so unlike every chat verb a tool
// serves, none is SEATED: the store records whoever made it under their own
// name, an administrator's pipeline token included. And none READS the room it
// destroys from: an administrator is in no private room or direct conversation
// they were not invited to, and a credential pasted into one is exactly what
// they need to take out of it without being shown everything else said there.
// So the confirmation a destruction takes is not something read off the room
// — the purges' is the item's key or the page's title, read from the row — but
// what the request itself destroys, repeated: an erase's COUNT of messages,
// and a prune's CUTOFF. A room the store does not hold is the store's answer.
//
// # Bound to what they destroy
//
// The operation id is [keyedOp]'s over the request's key and what it ASKS, in
// a canonical form: the erase's set of messages trimmed, deduplicated and
// sorted ([chatlet.EraseSet]), so one erase named in two orders is one operation;
// the prune's cutoff in UTC nanoseconds, so one instant written in two zones is
// one; the horizon as the number of days or null. The same key sent with other
// messages, a later cutoff or another horizon is another operation and lands
// as asked — never answered `applied` from the first request's record while
// nothing it names was destroyed, which is the one answer a compliance erase
// may not get.

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
			map[string]string{"detail": "state why in the body's `reason` — the " +
				"messages are destroyed on every data node, and the erase's reason " +
				"is the only account of them that survives"})
		return
	}
	confirm := strings.TrimSpace(r.URL.Query().Get("confirm"))
	if confirm == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidQuery,
			map[string]string{"detail": "repeat how many messages this erases in " +
				"?confirm= — an erase destroys them on every data node, and there is " +
				"nothing that undoes it"})
		return
	}
	set := chatlet.EraseSet(ids)
	if confirm != strconv.Itoa(len(set)) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %q and the "+
				"request names %d messages — nothing was destroyed", confirm, len(set))})
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
	cutoff, ok := queryInstant(w, r, "cutoff", "name the instant everything "+
		"before it is pruned in ?cutoff=, RFC 3339 — a prune destroys it on every "+
		"data node, and there is nothing that undoes it")
	if !ok {
		return
	}
	confirm, ok := queryInstant(w, r, "confirm", "repeat the cutoff in ?confirm= "+
		"— a prune destroys everything the room said before it, and there is "+
		"nothing that undoes it")
	if !ok {
		return
	}
	if !confirm.Equal(cutoff) {
		httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
			map[string]string{"detail": fmt.Sprintf("?confirm= says %s and "+
				"?cutoff= says %s — nothing was destroyed",
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
// absent one would read as the company's horizon, and a request that said
// nothing would then destroy the past of a room an administrator had set to
// keep for ever.
// A shorter horizon is a prune on a delay — the duty takes whatever it no
// longer covers on its next pass — which is why this is decided as a prune is.
func (s *served) putChatRetention(w http.ResponseWriter, r *http.Request) {
	args, ok := readArgs(w, r)
	if !ok || !only(w, args, "retention_days") {
		return
	}
	held, stated := args["retention_days"]
	if !stated {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "state the room's horizon in " +
				"`retention_days` — a number of days, 0 to keep it for ever, or " +
				"null for the company's own"})
		return
	}
	days, ok := retentionDays(w, held)
	if !ok {
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
			map[string]string{"detail": "name the messages to erase in the body's " +
				"`message_ids`, a list of their ids"})
		return nil, false
	}
	ids := make([]string, 0, len(list))
	for _, entry := range list {
		id, ok := entry.(string)
		if !ok {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": fmt.Sprintf("`message_ids` holds %v, "+
					"and every entry is a message's id as text", entry)})
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
			map[string]string{"detail": fmt.Sprintf("?%s= is %q, which is not an "+
				"RFC 3339 instant (2026-06-01T00:00:00Z)", name, raw)})
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
		map[string]string{"detail": fmt.Sprintf("`retention_days` is %v, and a "+
			"room's horizon is a whole number of days, 0 to keep it for ever, or "+
			"null for the company's own", held)})
	return nil, false
}
