package notify

// The self-action rule: a seat is never woken by its own action.
//
// Without it a seat assigned to its own issue receives a webhook for every
// comment IT posts, comments again, and loops — observed in the Python
// engine as 28 identical comments on one ticket before anyone stopped it.
//
// # One rule, three faces
//
// It shows up at three points, and the Python engine implemented it three
// separate times, in three layers, from three different fields:
//
//   - A chat backend ECHOES the bot's own message straight back. Caught at
//     the transport, by app id.
//   - A webhook names an actor who happens to be the recipient. Caught at
//     the inbound service.
//   - A parser fanning out to watchers includes the actor among them.
//     Caught at parse time, by comparing usernames.
//
// Three implementations meant three chances to disagree, and they did: the
// service read only `actor_account_id`, which one vendor stamps, so the
// guard existed for that vendor and silently did not exist for the rest.
// Here there is ONE actor field every parser stamps, and one comparison —
// applied by whichever layer holds the identifiers.

// ActorField is the metadata key carrying the external id of whoever caused
// an event.
//
// ONE key across every vendor, because the guard reads it and every parser
// writes it. A per-vendor key is not a naming preference — it is a guard
// that protects the vendors somebody remembered and quietly protects none of
// the others, which is exactly what it was.
const ActorField = "actor_external_id"

// SelfAction reports whether an event describes an action by the party it
// would wake.
//
// The actor is RESOLVED rather than string-compared against the recipient's
// id in the transport namespace. An agent acting on a chat backend posts
// under its BOT id while the same seat is addressed by member id, so a
// direct comparison misses precisely the case that loops — the seat's own
// message coming back. Resolution consults both namespaces, so both spellings
// of one seat compare equal.
//
// A nil registry reports false: a node with no company cannot tell whose
// action this was, and refusing to deliver on a guess would drop real work.
// The loop this prevents needs a seat that is already answering, which is a
// node that does have a company.
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
// THE EXCEPTION IS REAL AND NARROW: an event reporting the OUTCOME of the
// actor's own action is exactly what that actor needs to hear. A pipeline
// that failed names the person who pushed as its actor, and they are the one
// who has to fix it; suppressing it means the one person who can act never
// learns. A comment the actor wrote is the opposite — they already know.
//
// The distinction is "did something happen BECAUSE of me that I do not yet
// know about", and only the vendor can name which of its event types are
// which, so it is asked through the prompt registry. A source with no
// registered prompt gets the safe answer, which is no: an unrecognised event
// type that loops is worse than one that goes unheard, because the loop
// takes the company down with it.
func WakesActor(prompts Prompts, source, eventType string) bool {
	if eventType == "" {
		return false
	}
	return prompts.For(source).WakesActor(eventType)
}

// Deliverable reports whether a notification should wake a recipient, and
// why not when it should not.
//
// The reason is returned rather than logged here because the caller records
// it on the skip — an operator looking at a seat that did not answer needs
// to see "self-action" rather than an absence.
func Deliverable(prompts Prompts, r *Registry, n Inbound, recipient Party) (bool, string) {
	if recipient.Human {
		// A human seat is addressable but never woken: a person reads
		// the surface the event arrived on. Delivering would mean
		// spawning a turn for somebody who is not an agent.
		return false, "human seat"
	}
	actor := ActorOf(n.Metadata)
	if SelfAction(r, n.Source, actor, recipient) && !WakesActor(prompts, n.Source, n.EventType) {
		return false, "self-action: the recipient caused this event"
	}
	return true, ""
}

// SuppressSelf filters an actor out of a fan-out target list.
//
// The parse-time face of the same rule, applied where the identifiers are
// still the vendor's own usernames and nothing has been resolved yet. It
// takes the same [WakesActor] exception, so a failed pipeline still reaches
// the person who pushed it while their own comment does not come back to
// them.
//
// Order is preserved: a target list is in priority order, and the first
// reason per target wins downstream.
func SuppressSelf(targets []string, actor string, wakesActor bool) []string {
	if actor == "" || wakesActor {
		return targets
	}
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t != actor {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		// nil rather than an empty slice, so "nobody to tell" is one
		// value everywhere instead of two that compare differently.
		return nil
	}
	return out
}
