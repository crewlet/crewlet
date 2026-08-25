package jira

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// Reading a Jira payload.
//
// # One reader for two deployments and a relay
//
// The same event arrives in three shapes. Data Center posts its native
// webhook; Cloud posts through the Forge app, which renames the event and
// strips the actor into the envelope (the API edge puts both back); and the
// two deployments name people differently — accountId on Cloud, name on Data
// Center. Every extraction here reads whichever is present, because a reader
// that knew one shape would come back empty on the other and "nobody was
// named" is a legitimate answer that nothing would question.

// adfBlocks are the node types that end a line when flattened.
//
// Without them a bulleted list becomes one run-on sentence, which is the
// difference between a model reading acceptance criteria as four items and
// reading them as one.
var adfBlocks = map[string]bool{
	"paragraph": true, "heading": true, "blockquote": true,
	"bulletList": true, "orderedList": true, "listItem": true,
	"codeBlock": true, "rule": true, "panel": true, "doc": true,
}

// Flatten turns an Atlassian Document Format tree into the text a model
// reads.
//
// Jira Cloud sends comment bodies and issue descriptions as ADF — a nested
// {type, content, text, attrs} tree — while Data Center sends wiki-markup
// strings. Both arrive here and both come out as prose; a caller never
// branches on which it got.
//
// A MENTION KEEPS ITS NAME where the node carries one, because the sentence
// stops meaning anything without it: "can @ look at this" is not the message
// somebody sent. A node carrying only an account id renders in Jira's own
// [~accountid:…] form rather than vanishing, for the same reason.
//
// Deliberately lossy about everything else: the consumer is a model reading
// prose, not a browser rendering a document.
func Flatten(node any) string {
	return strings.TrimSpace(flatten(node))
}

func flatten(node any) string {
	switch v := node.(type) {
	case nil:
		return ""
	case string:
		return v
	case map[string]any:
		return flattenNode(v)
	default:
		return fmt.Sprint(v)
	}
}

func flattenNode(node map[string]any) string {
	attrs, _ := node["attrs"].(map[string]any)
	switch str(node, "type") {
	case "text":
		return str(node, "text")
	case "mention":
		if name := firstOf(str(attrs, "text"), str(attrs, "displayName")); name != "" {
			if strings.HasPrefix(name, "@") {
				return name
			}
			return "@" + name
		}
		if id := str(attrs, "id"); id != "" {
			return "[~accountid:" + id + "]"
		}
		return ""
	case "emoji":
		return firstOf(str(attrs, "shortName"), str(attrs, "text"))
	case "hardBreak":
		return "\n"
	}
	content, ok := node["content"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, child := range content {
		b.WriteString(flatten(child))
	}
	if adfBlocks[str(node, "type")] {
		b.WriteString("\n")
	}
	return b.String()
}

// MentionIDs are the accounts named in an ADF tree, in document order,
// deduped.
//
// ROUTING DEPENDS ON THIS, not just rendering: Jira's watcher list does NOT
// auto-include a mentioned user, so a colleague who was @-mentioned and is
// not watching the issue would otherwise never hear about it — the one
// routing miss a person notices immediately, because they can see their own
// name in the comment.
func MentionIDs(node any) []string {
	var (
		out  []string
		seen = map[string]bool{}
	)
	var walk func(any)
	walk = func(n any) {
		m, ok := n.(map[string]any)
		if !ok {
			return
		}
		if str(m, "type") == "mention" {
			attrs, _ := m["attrs"].(map[string]any)
			if id := str(attrs, "id"); id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		content, ok := m["content"].([]any)
		if !ok {
			return
		}
		for _, child := range content {
			walk(child)
		}
	}
	walk(node)
	return out
}

// diffHeavyFields are the ones whose before and after are paragraphs of
// prose.
//
// Rendered inline they are a wall of text on every edit — and Jira re-emits
// the whole description on changes that did not touch it, so the wall
// usually says nothing. They collapse to "was updated", which is the entire
// signal they carry.
var diffHeavyFields = map[string]bool{"description": true, "environment": true}

// accountFields carry an account id rather than a display string on the
// `from` / `to` side, so a missing `fromString` there resolves through the
// party registry instead of rendering a UUID.
var accountFields = map[string]bool{
	"assignee": true, "reporter": true, "creator": true, "watchers": true,
}

// changesOf renders a changelog as one line per field, resolved.
//
// # Why this is rendered here rather than carried as JSON
//
// The metadata map is stringly-typed, so a structured changelog can only
// cross it as an encoded blob that the prompt decodes again — a second
// serialisation format, in the one place where a decode failure is silent
// (an unparseable blob renders as no changes at all, which reads exactly
// like an event that changed nothing). The resolution also needs the party
// registry, and the parser is the frame holding it with the most context
// about the event. So the changelog crosses as the lines it will be shown
// as, and the prompt's job is where to put them.
func changesOf(body map[string]any, parties notify.Parties) string {
	changelog, ok := body["changelog"].(map[string]any)
	if !ok {
		return ""
	}
	items, ok := changelog["items"].([]any)
	if !ok {
		return ""
	}
	var lines []string
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		field := firstOf(str(item, "field"), str(item, "fieldId"))
		if field == "" {
			field = "unknown"
		}
		if diffHeavyFields[field] {
			lines = append(lines, "- **"+field+"**: was updated")
			continue
		}
		// The *String variants are the display forms, and Jira omits
		// them on transitions where one side is null. Falling through to
		// the raw id keeps the line useful — an account id says more
		// than "(none) → (none)" — and a line with nothing on either
		// side is dropped, because that change carried no signal at all.
		from, fromDisplay := str(item, "from"), str(item, "fromString")
		to, toDisplay := str(item, "to"), str(item, "toString")
		if fromDisplay == "" && from == "" && toDisplay == "" && to == "" {
			continue
		}
		lines = append(lines, "- **"+field+"**: "+
			side(field, from, fromDisplay, parties)+" → "+
			side(field, to, toDisplay, parties))
	}
	return strings.Join(lines, "\n")
}

// side renders one end of a change.
func side(field, raw, display string, parties notify.Parties) string {
	if display != "" {
		return display
	}
	if raw == "" {
		return "(none)"
	}
	if accountFields[field] && parties != nil {
		if party, ok := parties.ByExternalID(Backend, raw); ok {
			if label := party.Label(); label != "" {
				return label
			}
		}
	}
	return raw
}

// actorOf is who caused the event, in the shapes the three sources use.
//
// A comment webhook carries no top-level `user` — the author is on the
// comment — and the Forge relay puts the account id on both. Reading only
// the top level attributes every Cloud comment to nobody, which then defeats
// the self-action guard: the seat that wrote the comment gets woken by it.
func actorOf(body map[string]any) map[string]any {
	if u, ok := body["user"].(map[string]any); ok && len(u) > 0 {
		return u
	}
	if comment, ok := body["comment"].(map[string]any); ok {
		if author, ok := comment["author"].(map[string]any); ok {
			return author
		}
	}
	return map[string]any{}
}

// personID reads a person's routing identity, whichever the deployment used.
func personID(person map[string]any) string {
	return firstOf(str(person, "accountId"), str(person, "name"))
}

// commentEvents are the event names a comment body arrives under.
//
// `comment_deleted` keeps its body: what was deleted is still the signal —
// a seat asked to act on a comment that is now gone needs to know what it
// said to know whether the ask stands.
var commentEvents = map[string]bool{
	"comment_created": true, "comment_updated": true, "comment_deleted": true,
}

// base assembles the notification every recipient's copy is made from.
//
// The second result is false for a payload that names no issue: every rule
// below it — the conversation key, the recon pointer, the watcher lookup —
// rests on the issue, and a copy without one is a wake with nowhere to look.
func (p *Parser) base(body map[string]any, parties notify.Parties) (notify.Inbound, bool) {
	event := str(body, "webhookEvent")
	issue, _ := body["issue"].(map[string]any)
	fields, _ := issue["fields"].(map[string]any)
	project, _ := fields["project"].(map[string]any)

	issueKey := str(issue, "key")
	if issueKey == "" {
		return notify.Inbound{}, false
	}
	actor := actorOf(body)
	assignee, _ := fields["assignee"].(map[string]any)

	meta := map[string]string{
		// The one actor key every integration stamps, so the
		// self-action guard is a spine rule rather than something each
		// vendor remembered.
		notify.ActorField:     personID(actor),
		"actor_name":          firstOf(str(actor, "displayName"), str(actor, "name")),
		"event_type":          event,
		"issue_key":           issueKey,
		"issue_id":            str(issue, "id"),
		"project":             strings.ToUpper(str(project, "key")),
		"project_name":        str(project, "name"),
		"assignee_account_id": personID(assignee),
		"assignee_email":      str(assignee, "emailAddress"),
		"timestamp":           timestampOf(body),
	}
	if changes := changesOf(body, parties); changes != "" {
		meta["changes"] = changes
	}
	if link := p.link(issueKey); link != "" {
		meta["url"] = link
	}

	var text string
	comment, _ := body["comment"].(map[string]any)
	switch {
	case commentEvents[event] && len(comment) > 0:
		text = Flatten(comment["body"])
		if id := str(comment, "id"); id != "" {
			meta["comment_id"] = id
		}
	default:
		text = Flatten(fields["description"])
	}

	summary := str(fields, "summary")
	subject := "[" + issueKey + "]"
	if summary != "" {
		subject += " " + summary
	}
	return notify.Inbound{
		Source:    Backend,
		EventType: event,
		Sender:    firstOf(meta["actor_name"], meta[notify.ActorField]),
		Subject:   subject,
		Body:      text,
		Metadata:  meta,
	}, true
}

// link is the address a person opens.
//
// Empty when no shareable base is configured, which is the honest answer for
// a Cloud instance named only by its cloud id: the API gateway is not a
// place a browser can go, and a link that looks right and opens nothing is
// worse than none.
func (p *Parser) link(issueKey string) string {
	if p.url == "" {
		return ""
	}
	return p.url + "/browse/" + issueKey
}

// timestampOf renders the event's own time.
//
// Jira sends epoch MILLISECONDS, which is a number no reader of a prompt can
// place. Converted once, here, at the edge — the convention every other
// duration and instant in the engine follows — so nothing downstream carries
// a unit in a string.
func timestampOf(body map[string]any) string {
	switch v := body["timestamp"].(type) {
	case nil:
		return ""
	case string:
		// Some bridges send it already formatted. Passed through rather
		// than re-parsed: a value this build cannot read is still the
		// vendor's own answer to when it happened.
		return v
	case float64:
		return time.UnixMilli(int64(v)).UTC().Format(time.RFC3339)
	case int64:
		return time.UnixMilli(v).UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(v)
	}
}

// str reads a payload field as a string, whatever shape it arrived in.
func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		// An id that decoded as a number. Rendered without the decimal
		// tail JSON's float default would give it, because it is
		// compared against ids that arrived as strings.
		return strconv.FormatInt(int64(v), 10)
	default:
		return fmt.Sprint(v)
	}
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
