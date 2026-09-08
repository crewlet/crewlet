package datadog

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/config"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/notify"
)

var log = logging.Get("datadog")

// The metadata keys a Datadog notification carries.
//
// Named constants rather than literals because the prompt reads back what the
// parser wrote, and a key spelled two ways is a field that is always empty on
// exactly one side.
const (
	// RoutedViaField says WHY this seat was woken: "tag" when a monitor
	// named it, "fallback" when nothing did. The prompt asks the two for
	// different things.
	RoutedViaField = "routed_via"

	TransitionField = "transition"
	PriorityField   = "priority"
	LinkField       = "link"
	ScopeField      = "scope"
	MonitorField    = "monitor"
	TagsField       = "tags"
)

// The routing reasons [RoutedViaField] carries.
const (
	RoutedViaTag      = "tag"
	RoutedViaFallback = "fallback"
)

// Ignore is the fallback value that means "wake nobody". See
// [config.DatadogIgnore], which is where it is defined and validated.
const Ignore = config.DatadogIgnore

// DefaultHandleTag is the monitor tag key that names a seat.
//
// "crewlet" rather than something like "owner" or "team", which are the two
// obvious alternatives and are both wrong here: a company that already tags
// monitors by owning team would find every one of them suddenly routing to a
// seat, and a key this specific cannot collide with a convention somebody
// already has.
const DefaultHandleTag = "crewlet"

// ParserOptions configure a [Parser].
type ParserOptions struct {
	// HandleTag is the monitor tag key naming a seat. Empty takes
	// [DefaultHandleTag].
	HandleTag string

	// Fallback is the seat an alert naming nobody wakes.
	//
	// REQUIRED, and config validation refuses an enabled Datadog block
	// without one. See the package doc: an alerting integration whose
	// alerts reach nobody is worse than one that is switched off, because
	// it looks like coverage.
	Fallback string
}

// Parser turns one Datadog delivery into the notifications it implies.
type Parser struct {
	handleTag string
	fallback  string
}

// NewParser builds the parser.
func NewParser(opts ParserOptions) *Parser {
	tag := strings.ToLower(strings.TrimSpace(opts.HandleTag))
	if tag == "" {
		tag = DefaultHandleTag
	}
	return &Parser{handleTag: tag, fallback: strings.TrimSpace(opts.Fallback)}
}

// Source implements [notify.Parser].
func (p *Parser) Source() string { return Backend }

// Parse implements [notify.Parser].
//
// # A recovery reaches the same seats as the alert
//
// Not filtered to triggers, deliberately. The seat woken to investigate an
// alert is the one that has to be told it cleared, and a company whose
// recoveries were dropped would have agents chasing incidents that ended
// hours ago. What differs is the ASK, which is the prompt's job.
//
// # The registry is deliberately not consulted
//
// Every other parser here intersects its targets against the parties the
// engine can route to, so a comment mentioning outsiders does not fan out to
// notifications nobody can deliver. That reasoning does not carry over: an
// outsider on GitHub is an ordinary occurrence, while a Datadog monitor
// tagged with a seat that does not exist is a TYPO, and the operator has to
// see it. The spine already records an undeliverable notification with the
// handle on it, so passing it through is what makes the typo visible;
// dropping it here would make it silent on the one surface where silence is
// the failure that matters.
func (p *Parser) Parse(_ context.Context, w types.RawWebhook, _ *notify.Registry) ([]notify.Routed, error) {
	alert := decode(w)

	handles := TagValues(alert.Tags, p.handleTag)
	via := RoutedViaTag
	if len(handles) == 0 {
		if p.fallback == Ignore {
			// ASKED FOR. A company may want only the monitors it has
			// labelled to wake anybody, and every other alert to pass
			// through Datadog's own on-call instead of an agent's inbox.
			//
			// Logged at debug rather than warn: the drop is the
			// configuration doing what it says, and warning about it every
			// time would make a working setup look faulty.
			log.Debug("datadog_alert_ignored", "title", alert.Title,
				"detail", "no monitor tag named a seat and this company ignores those")
			return nil, nil
		}
		if p.fallback == "" {
			// Reachable only through a config that skipped validation,
			// which today means a test. Logged rather than silent
			// because the alternative is an alert that vanishes.
			log.Warn("datadog_alert_unrouted", "title", alert.Title,
				"detail", "no monitor tag named a seat and this company has no route_to")
			return nil, nil
		}
		handles, via = []string{p.fallback}, RoutedViaFallback
	}

	var (
		out  []notify.Routed
		seen = map[string]bool{}
	)
	for _, handle := range handles {
		handle = strings.TrimSpace(handle)
		if handle == "" || seen[handle] {
			continue
		}
		seen[handle] = true
		out = append(out, notify.Routed{
			Inbound: inbound(alert, via),
			// A HANDLE, not an external id, and this is the one third-party app
			// where that is right: a monitor tag is written by an
			// operator reading the company document, so the word in it
			// is the seat's own handle rather than an account Datadog
			// issued. Sending it as an external id would ask the
			// registry to resolve a Datadog identity that was never
			// minted.
			To: notify.Recipient{Handle: handle},
		})
	}

	return out, nil
}

// inbound assembles one recipient's copy.
//
// Built per recipient rather than once and shared, matching every other
// parser here: the metadata carries the routing reason, and a shared map
// would make every copy claim whichever reason was written last. Datadog
// currently gives every copy the same reason, which is exactly the kind of
// thing that stops being true the moment a second tier is added.
func inbound(alert Alert, via string) notify.Inbound {
	meta := map[string]string{
		RoutedViaField:  via,
		TransitionField: alert.Transition,
		PriorityField:   alert.Priority,
		LinkField:       alert.Link,
		ScopeField:      alert.Scope,
		TagsField:       strings.Join(alert.Tags, ","),
		MonitorField:    alert.Title,
	}
	// NO ActorField. Every other surface stamps who caused the event so
	// the spine can suppress waking them for their own action. A monitor
	// has no author: the nearest thing is whoever last edited it, which is
	// not who this concerns, and stamping it would suppress the alert for
	// exactly the person who owns the monitor.
	for key, value := range meta {
		if value == "" {
			delete(meta, key)
		}
	}
	return notify.Inbound{
		Source:    Backend,
		EventType: eventType(alert),
		Subject:   subject(alert),
		Body:      alert.Body,
		Metadata:  meta,
	}
}

// eventType is the third-party app's own name for what happened, normalised.
//
// Lowercased with spaces collapsed to underscores, so "No Data" becomes
// "no_data" and sits beside "issue_comment" and "pipeline.failed" in the
// event store rather than beside nothing.
func eventType(alert Alert) string {
	transition := strings.ToLower(strings.TrimSpace(alert.Transition))
	if transition == "" {
		return "monitor"
	}
	return "monitor." + strings.ReplaceAll(transition, " ", "_")
}

// subject is the one line an operator sees in the feed.
func subject(alert Alert) string {
	if alert.Title != "" {
		return alert.Title
	}
	// A template somebody edited can leave the title out. The scope is the
	// next most identifying thing on the payload, and a subject naming the
	// host is far better than an empty one.
	if alert.Scope != "" {
		return "Datadog monitor on " + alert.Scope
	}
	return "Datadog monitor"
}
