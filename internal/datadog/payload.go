package datadog

import (
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events/types"
)

// Alert is one monitor notification, as the engine's own payload template
// asks Datadog to send it.
//
// EVERY FIELD IS OPTIONAL, by decision. The template lives in the webhook
// definition at Datadog, where an operator can edit it, so a missing field is a
// configuration mistake, and dropping a firing monitor over one is the worst
// response: the alert is real whether or not its priority came through.
//
// The template that fills this is [WebhookPayload], which the reconcile writes
// and the docs publish, so the shape asked for and decoded cannot drift.
type Alert struct {
	// ID is Datadog's notification id, stable across its own retries. The
	// webhook route claims on it, so it is the dedupe key rather than
	// anything this package reads.
	ID string

	// Title is the monitor's rendered title, which carries the state and
	// the monitor's name: "[Triggered] API latency" when it fires and
	// "[Recovered] API latency" when it clears. NOT an identity — see
	// MonitorID.
	Title string

	// MonitorID is the monitor itself, which is what makes a trigger and
	// its recovery ONE conversation.
	//
	// Datadog's $ALERT_ID, not $ID: the latter identifies the NOTIFICATION,
	// so every firing would be its own thread. Nor the title, which carries
	// the state and would split a trigger from its recovery.
	//
	// Empty for an alert from a webhook definition that does not carry it (a
	// hand-written one, or this engine's own after its payload was edited),
	// which is why [Prompt.PartitionKey] still falls back to the title.
	MonitorID string

	// Body is the monitor message with Datadog's own notification targets
	// left in it.
	Body string

	// Transition is the state change: "Triggered", "Recovered",
	// "Re-Triggered", "No Data", "Warn". Datadog's own capitalisation,
	// kept as sent.
	Transition string

	// Priority is the monitor's priority, "1" through "5", or empty when
	// the monitor sets none. Datadog's P1 is the most urgent.
	Priority string

	// Tags are the monitor's tags, already split. This is what routing
	// runs on: see the package doc for why it is tags rather than
	// mentions.
	Tags []string

	// Link is the URL of the alert in Datadog, so a prompt can send a seat
	// somewhere it can actually look.
	Link string

	// Scope is what the monitor was grouped by for this alert, for example
	// "host:web-3,env:prod". It is the difference between "the API is
	// down" and "the API is down in one region".
	Scope string

	// EventType is Datadog's own event classification, or empty.
	EventType string
}

// Triggered reports whether this transition is a monitor going off rather
// than coming back.
//
// The distinction is load-bearing for the prompt rather than for the routing:
// a recovery reaches the same seat as the alert it recovers from, because the
// seat that was woken to investigate is the one that has to be told to stop.
func (a Alert) Triggered() bool {
	switch strings.ToLower(strings.TrimSpace(a.Transition)) {
	case "recovered", "ok":
		return false
	default:
		// UNKNOWN COUNTS AS TRIGGERED. Datadog has added transition
		// names before ("Re-Triggered", "No Data", "Warn"), and the two
		// ways of being wrong are not symmetric: treating a new alert
		// state as a recovery tells a seat to stand down from a page
		// nobody has looked at.
		return true
	}
}

// decode reads one delivery.
//
// Tolerant on the way in for the reason [Alert] states, and it never fails:
// a body that carried nothing this recognises still produced a delivery, and
// the caller decides what an alert naming nothing is worth. The one thing
// that would be worth an error, a body that is not an object, cannot reach
// here because the webhook route has already parsed it.
func decode(w types.RawWebhook) Alert {
	return Alert{
		ID:         str(w.Body, "id"),
		Title:      str(w.Body, "title"),
		MonitorID:  str(w.Body, "monitor_id"),
		Body:       str(w.Body, "body"),
		Transition: str(w.Body, "alert_transition"),
		Priority:   strings.TrimPrefix(str(w.Body, "priority"), "P"),
		Tags:       splitTags(str(w.Body, "tags")),
		Link:       str(w.Body, "link"),
		Scope:      str(w.Body, "scope"),
		EventType:  str(w.Body, "event_type"),
	}
}

// str reads a string field, tolerating the number Datadog sends when a
// template writes a variable unquoted.
func str(body map[string]any, key string) string {
	switch v := body[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		// A template written as "priority": $PRIORITY rather than
		// "$PRIORITY" arrives as a JSON number. Both are things an
		// operator writes, and refusing one of them would drop a firing
		// monitor over a pair of quotes.
		//
		// 'f' with a precision of -1 is the shortest representation that
		// round-trips, so a priority of 1 renders as "1" rather than as
		// "1e+00" or "1.000000".
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// splitTags parses Datadog's `$TAGS` expansion.
//
// Datadog renders a monitor's tags as a comma-separated list, and a tag may
// legitimately contain a colon (`env:prod`) but never a comma. Whitespace
// around each entry is stripped because the expansion carries it after a
// template that put the list on its own line.
//
// Case is FOLDED to lower here, because Datadog itself lowercases tag keys
// and values on ingestion, so a monitor tagged `Crewlet:CEO` in the UI
// arrives as `crewlet:ceo` and a comparison that respected case would match
// neither what the operator typed nor what the seat is called.
func splitTags(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if tag := strings.ToLower(strings.TrimSpace(part)); tag != "" {
			out = append(out, tag)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// TagValues returns the values of every tag with this key.
//
// A SLICE rather than one value, because a monitor watching two teams' shared
// service carries the key twice and waking only the first of them is how the
// other team finds out from a customer.
func TagValues(tags []string, key string) []string {
	prefix := strings.ToLower(key) + ":"
	var out []string
	for _, tag := range tags {
		value, found := strings.CutPrefix(tag, prefix)
		if !found || value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

// WebhookPayload is the template the engine asks Datadog to post.
//
// It lives beside [decode], its only reader: two files would drift into a field
// arriving empty for every alert.
//
// Every value is quoted: an unquoted $PRIORITY expanding to nothing yields
// `"priority": ,`, not JSON, which Datadog posts anyway. [str] still accepts a
// number, for an operator who unquoted one.
//
// $TAGS is the routing input. $ALERT_ID is the MONITOR and $ID the
// notification: the second is the dedupe key at the webhook edge, the first
// makes a trigger and its recovery one conversation.
const WebhookPayload = `{
  "id": "$ID",
  "monitor_id": "$ALERT_ID",
  "title": "$EVENT_TITLE",
  "body": "$EVENT_MSG",
  "alert_transition": "$ALERT_TRANSITION",
  "priority": "$PRIORITY",
  "tags": "$TAGS",
  "link": "$LINK",
  "scope": "$ALERT_SCOPE",
  "event_type": "$EVENT_TYPE"
}`
