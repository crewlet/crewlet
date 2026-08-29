package webhooks

import (
	"net/http"
	"strings"
	"time"
)

// forgeEvent names the integration a relayed Cloud event belongs to and the
// event name the transports already understand.
//
// The mapping exists because Forge renames everything: a Cloud page update
// arrives as avi:confluence:updated:page where the Data Center webhook for the
// same thing says page_updated. Translating here is what lets ONE transport
// per integration handle both deployments — the alternative is every consumer
// knowing both vocabularies.
type forgeEvent struct{ source, legacy string }

var forgeEvents = map[string]forgeEvent{
	"avi:jira:created:issue":          {"jira", "jira:issue_created"},
	"avi:jira:updated:issue":          {"jira", "jira:issue_updated"},
	"avi:jira:deleted:issue":          {"jira", "jira:issue_deleted"},
	"avi:jira:commented:issue":        {"jira", "comment_created"},
	"avi:jira:deleted:comment":        {"jira", "comment_deleted"},
	"avi:confluence:created:page":     {"confluence", "page_created"},
	"avi:confluence:updated:page":     {"confluence", "page_updated"},
	"avi:confluence:trashed:page":     {"confluence", "page_trashed"},
	"avi:confluence:deleted:page":     {"confluence", "page_removed"},
	"avi:confluence:created:comment":  {"confluence", "comment_created"},
	"avi:confluence:updated:comment":  {"confluence", "comment_updated"},
	"avi:confluence:created:blogpost": {"confluence", "blog_created"},
	"avi:confluence:updated:blogpost": {"confluence", "blog_updated"},
}

// forgeEnvelopeKeys are the relay's own fields, stripped from a Jira body
// before it reaches a transport that expects a native webhook.
var forgeEnvelopeKeys = map[string]bool{
	"eventType": true, "atlassianId": true, "selfGenerated": true,
	"suppressNotifications": true, "encryptedData": true,
	"permissions": true, "eventCreatedDate": true,
}

func (r *Receiver) forgeWebhook(w http.ResponseWriter, req *http.Request) {
	// The body is read BEFORE the token is verified, and that ordering is
	// not the usual one. Verifying can block on a JWKS fetch while the
	// sender's own delivery deadline runs out, and an aborted sender
	// surfaces as a broken read at the FIRST body read — so the socket is
	// drained while the sender is still there to be answered. Nothing is
	// persisted or published by reading it.
	raw, ok := r.body(w, req)
	if !ok {
		return
	}
	// PARSED BEFORE THE GATE, which the other routes deliberately no
	// longer do: the readiness answer below is keyed on eventType, and
	// that field is in the body. The cost is bounded by MaxBodyBytes and
	// nothing is persisted or published by decoding it.
	body, ok := parseBody(w, raw)
	if !ok {
		return
	}
	event := str(body, "eventType")
	if !r.serving(w, "forge", event) {
		return
	}

	appID := r.secrets().ForgeAppID
	if appID == "" {
		// The same class as a missing HMAC secret: the app id IS the
		// audience this route verifies against, so without it there is
		// nothing to check and the delivery must be held, not discarded.
		noSecret(w, "forge")
		return
	}
	token, found := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !found || token == "" {
		unauthorized(w, "unauthorized")
		return
	}
	if err := r.forge.verify(req.Context(), token, appID); err != nil {
		// The REASON reaches the log, never the caller: it distinguishes
		// an expired token from one addressed at another app, which is
		// what an operator needs and what an attacker would use to probe.
		log.Warn("forge_fit_invalid", "error", err)
		unauthorized(w, "unauthorized")
		return
	}
	v := verified{source: "forge"}

	// The app's own writes come back to it. Acting on them is how an agent
	// answers its own comment, forever.
	if selfGenerated, _ := body["selfGenerated"].(bool); selfGenerated {
		log.Debug("forge_self_generated_skipped", "event", event)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "selfGenerated"})
		return
	}

	mapped, known := forgeEvents[event]
	if !known {
		// Forge delivers everything the app subscribed to, and an app's
		// subscriptions outlive this build's knowledge of them. Ignoring
		// with a 200 is right: a 4xx would make Atlassian retry an event
		// nothing here will ever handle.
		log.Warn("forge_unknown_event", "event", event)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": event})
		return
	}

	atlassianID := str(body, "atlassianId")
	transformed := transformForge(mapped, body, r.now())
	r.accept(w, req, v, delivery{
		source: mapped.source,
		// Named forge:… rather than webhook:… so the feed says how a
		// Cloud event reached the engine. The same page update arrives
		// as webhook:page_updated from Data Center, and an operator
		// debugging a Cloud tenant needs to see which path it took.
		label:   "forge:" + event,
		summary: forgeSummary(mapped.source, mapped.legacy, transformed),
		body:    body,
		routed:  transformed,
		raw:     raw,
		// A RELAYED EVENT CARRIES NO DELIVERY HEADER. forgeID is the
		// Atlassian ACCOUNT behind the event — the actor, not the
		// delivery — so it cannot identify one, and the relay's own
		// retries resend the same bytes. See [bodyKey].
		key:     bodyKey(raw),
		forgeID: atlassianID,
		headers: safeHeaders(req.Header),
	}, statusOK)
}

// transformForge rewrites a relayed payload into the shape the native
// transports parse.
//
// Confluence puts the event's subject under "content"; Jira states it at the
// top level beside the relay's own envelope fields. Both are normalized here,
// once, rather than in each transport — a transport that had to know it was
// reading a relay would need the whole mapping above as well.
func transformForge(mapped forgeEvent, raw map[string]any, now time.Time) map[string]any {
	body := map[string]any{}
	atlassianID := str(raw, "atlassianId")

	switch mapped.source {
	case "confluence":
		content := object(raw, "content")
		if str(content, "type") == "comment" {
			body["comment"] = content
			container := object(content, "container")
			if len(container) > 0 {
				body["page"] = container
			}
			// The space can be stated on either the comment or its
			// container, and routing reads it off the page. A comment
			// whose space stayed on the comment would route nowhere.
			space := content["space"]
			if space == nil {
				space = container["space"]
			}
			if page, ok := body["page"].(map[string]any); ok && space != nil {
				if _, present := page["space"]; !present {
					page["space"] = space
				}
			}
		} else {
			body["page"] = content
		}
		body["event"] = mapped.legacy
	case "jira":
		for key, value := range raw {
			if !forgeEnvelopeKeys[key] {
				body[key] = value
			}
		}
		body["webhookEvent"] = mapped.legacy
	}

	// The actor. Forge states it ONCE, at the top level, and strips it from
	// the payload — so a transport reading only the body would attribute
	// every Cloud event to nobody.
	if atlassianID != "" {
		if _, present := body["userAccountId"]; !present {
			body["userAccountId"] = atlassianID
		}
		if mapped.source == "jira" {
			setAccountID(body, "user", atlassianID)
			if comment := object(body, "comment"); len(comment) > 0 {
				setAccountID(comment, "author", atlassianID)
			}
		}
	}

	if _, present := body["timestamp"]; !present {
		body["timestamp"] = forgeTimestamp(str(raw, "eventCreatedDate"), now)
	}
	return body
}

// setAccountID fills in an actor object's accountId without overwriting one
// the payload already carries.
func setAccountID(m map[string]any, key, accountID string) {
	actor, ok := m[key].(map[string]any)
	if !ok {
		m[key] = map[string]any{"accountId": accountID}
		return
	}
	if _, present := actor["accountId"]; !present {
		actor["accountId"] = accountID
	}
}

// forgeTimestamp is the event's own time in epoch milliseconds, which is what
// the native webhooks carry and what the transports read.
//
// Falling back to NOW rather than leaving it absent: every consumer of this
// field treats a missing one as the zero time, which orders the event before
// everything else that ever happened.
func forgeTimestamp(created string, now time.Time) int64 {
	if created != "" {
		if at, err := time.Parse(time.RFC3339, created); err == nil {
			return at.UnixMilli()
		}
	}
	return now.UnixMilli()
}
