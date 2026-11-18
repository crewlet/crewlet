package notify

// The self-action rule: a seat is never woken by its own action.
//
// Without it a seat woken by its own comments loops. One rule, ONE actor field
// every parser stamps, one comparison, applied by whichever layer holds the
// identifiers:
//
//   - A chat backend ECHOES the bot's own message: caught at the transport.
//   - A webhook names the recipient as actor: caught at the inbound service.
//   - A first-party parser's watcher fan-out includes the actor: caught at
//     parse time, where author and candidates share one namespace (Worklet's
//     router and Doclet's drop the author, under their prompt's exception). A
//     vendor parser only stamps the actor: the registry resolves its accounts.

// ActorField is the metadata key carrying the external id of whoever caused
// an event.
//
// ONE key across every third-party app, because the guard reads it and every
// parser writes it. A per-integration key is not a naming preference. It is a
// guard that protects the third-party apps somebody remembered and quietly
// protects none of the others, which is exactly what it was.
const ActorField = "actor_external_id"

// SelfAction reports whether an event describes an action by the party it
// would wake.
//
// The actor is RESOLVED, not string-compared with the recipient's transport
// id: an agent on a chat backend posts under its BOT id while the seat is
// addressed by member id, so a direct comparison misses exactly the looping
// case. Resolution consults both namespaces.
//
// A nil registry reports false: a node with no company cannot tell whose
// action this was, and refusing on a guess would drop real work. The loop needs
// an answering seat, which means a node that has a company.
func SelfAction(r *Registry, transport, actorID string, recipient Party) bool {
	if r == nil || actorID == "" || recipient.Handle == "" {
		return false
	}
	actor, ok := r.ByExternalID(transport, actorID)
	return ok && actor.Handle == recipient.Handle
}

// ActorOf reads the actor's external id off a notification's metadata.
func ActorOf(metadata map[string]string) string { return metadata[ActorField] }

// WakesActor reports whether an event should reach the party who caused it
// anyway, overriding the self-action rule.
//
// THE EXCEPTION IS NARROW: the OUTCOME of the actor's own action is what that
// actor needs to hear (a failed pipeline names the pusher, the one who can fix
// it); a comment they wrote they already know. Only the app can say which
// notifications are which, so it is asked through the prompt registry with the
// whole notification, since a source may answer from what its parser stamped on
// this recipient's copy ([Prompt.WakesActor]). A source with no registered
// prompt gets no: an unrecognised event that loops is worse than one unheard,
// because the loop takes the company down with it.
func WakesActor(prompts Prompts, n Inbound) bool {
	return prompts.For(n.Source).WakesActor(n)
}

// Deliverable reports whether a notification should wake a recipient, and
// why not when it should not.
//
// The reason is returned rather than logged here because the caller records
// it on the skip — an operator looking at a seat that did not answer needs
// to see "self-action" rather than an absence.
func Deliverable(prompts Prompts, r *Registry, n Inbound, recipient Party) (bool, SkipReason) {
	if recipient.Human {
		// A human seat is addressable but never woken: a person reads
		// the surface the event arrived on. Delivering would mean
		// spawning a turn for somebody who is not an agent.
		return false, ReasonHumanSeat
	}
	actor := ActorOf(n.Metadata)
	if SelfAction(r, n.Source, actor, recipient) && !WakesActor(prompts, n) {
		return false, ReasonSelfAction
	}
	return true, ""
}
