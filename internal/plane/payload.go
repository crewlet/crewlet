package plane

import (
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/crewlet/crewlet/internal/notify"
)

// Reading a Plane payload.
//
// # Every extraction is defensive, and that is not paranoia
//
// The serializer shapes are not contractual across fork rebases: a foreign
// key arrives as a UUID string on one build and as an expanded object on the
// next, the actor is sometimes a UUID and sometimes a serialised user, and
// the serializers disagree about `project` versus `project_id`. A parser
// that assumed one shape would stop routing anything the day a fork rebased,
// and would do it silently — an absent field reads exactly like an event
// nobody was named in.

// mentionPattern captures the mentioned user's UUID out of the editor's
// mention markup.
//
// `entity_name` in that markup is the node-TYPE discriminator — the literal
// string "user_mention" — and never a display name, which is why a label has
// to be resolved through the party registry rather than read off the tag.
var mentionPattern = regexp.MustCompile(`<mention-component[^>]*entity_identifier="([^"]+)"`)

var (
	mentionComponent = regexp.MustCompile(`<mention-component[^>]*>(?:</mention-component>)?`)
	anyTag           = regexp.MustCompile(`<[^>]+>`)
	whitespace       = regexp.MustCompile(`\s+`)
)

// MentionIDs are the users named in a comment, lowercased, deduped, in the
// order they appear.
//
// The order is kept because the first reason for a person wins downstream,
// and a comment that names somebody twice should not produce two copies.
func MentionIDs(markup string) []string {
	if markup == "" {
		return nil
	}
	var (
		out  []string
		seen = map[string]bool{}
	)
	for _, m := range mentionPattern.FindAllStringSubmatch(markup, -1) {
		id := strings.ToLower(m[1])
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// Flatten turns editor markup into the text a model reads.
//
// A MENTION BECOMES A NAME, resolved through the party registry — the markup
// carries only a UUID, and a body reading "@8f2c1e…, can you look at this"
// tells a seat nothing about who is asking or whether it is them. An
// unresolvable UUID renders as itself rather than vanishing: a mention that
// disappeared would change what the sentence means.
//
// Everything else collapses to whitespace. Deliberately lossy: the consumer
// is a model reading prose, not a browser rendering a document.
func Flatten(markup string, parties notify.Parties) string {
	if markup == "" {
		return ""
	}
	text := mentionComponent.ReplaceAllStringFunc(markup, func(tag string) string {
		m := mentionPattern.FindStringSubmatch(tag)
		if len(m) < 2 {
			return ""
		}
		id := strings.ToLower(m[1])
		if id == "" {
			return ""
		}
		if parties != nil {
			if party, ok := parties.ByExternalID(Backend, id); ok {
				if label := cmpFirst(party.Name, party.Handle); label != "" {
					return "@" + label
				}
			}
		}
		return "@" + id
	})
	text = anyTag.ReplaceAllString(text, " ")
	text = strings.TrimSpace(whitespace.ReplaceAllString(text, " "))
	return html.UnescapeString(text)
}

func cmpFirst(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// base assembles the notification every recipient's copy is made from.
func (p *Parser) base(body, data map[string]any, identifier string, parties notify.Parties) (notify.Inbound, bool) {
	event := str(body, "event")
	action := str(body, "action")
	eventType := event
	if action != "" {
		eventType = event + "." + action
	}
	activity := object(body, "activity")
	workspace := str(body, "workspace_slug")

	projectID := str(data, "project")
	if event == "page" {
		projectID = pageProjectID(data)
	}
	meta := map[string]string{
		// The one actor key every integration stamps, so the
		// self-action guard exists here rather than being a thing
		// somebody remembered for one vendor.
		notify.ActorField: str(activity, "actor_id"),
		"actor_name":      actorName(activity),
		"event_type":      eventType,
		"project":         identifier,
		"project_id":      projectID,
		"workspace":       workspace,
		"timestamp":       firstOf(str(data, "updated_at"), str(data, "created_at")),
	}

	var body_, link string
	name := str(data, "name")

	switch event {
	case "page":
		pageID := refID(data["id"])
		meta["page_id"] = pageID
		if p.url != "" && workspace != "" && projectID != "" && pageID != "" {
			link = fmt.Sprintf("%s/%s/projects/%s/pages/%s",
				p.url, workspace, projectID, pageID)
		}
	default:
		// On a comment or an intake row, `data.id` is the COMMENT's own
		// id and the work item lives under `data.issue`. There is no
		// fallback to `data.id`: a comment id in the issue slot produces
		// a link that 404s and a subscriber lookup that cannot succeed,
		// which is strictly worse than an absent pointer.
		issueID := refID(data["id"])
		if event != "issue" {
			issueID = refID(data["issue"])
		}
		if issueID != "" {
			meta["issue_id"] = issueID
			if p.url != "" && workspace != "" && projectID != "" {
				link = fmt.Sprintf("%s/%s/projects/%s/issues/%s",
					p.url, workspace, projectID, issueID)
			}
		}
		switch {
		case event == "issue_comment":
			meta["comment_id"] = refID(data["id"])
			markup := str(data, "comment_html")
			body_ = Flatten(markup, parties)
			if mentions := MentionIDs(markup); len(mentions) > 0 {
				meta["mention_ids"] = strings.Join(mentions, ",")
			}
		case event == "issue" && action == "created":
			body_ = Flatten(str(data, "description_html"), parties)
		}
		if event == "issue" || event == "issue_comment" {
			// "ENG-42", for the subject line and the prompt. DISPLAY
			// ONLY: the conversation key stays the work item's UUID,
			// because the sequence number rides only the issue
			// payload and the identifier needs a warm cache — keying
			// coalescing on it would split one work item into two
			// partitions the moment either was missing.
			if seq := str(data, "sequence_id"); seq != "" && identifier != "" {
				meta["work_item_key"] = identifier + "-" + seq
			}
		}
	}
	if link != "" {
		meta["url"] = link
	}

	return notify.Inbound{
		Source:    Backend,
		EventType: eventType,
		Sender:    firstOf(actorName(activity), str(activity, "actor_id")),
		Subject:   subject(name, meta["work_item_key"], identifier, workspace, eventType),
		Body:      body_,
		Metadata:  meta,
	}, true
}

// subject is what a seat sees before it reads anything.
//
// The work item's key leads where there is one — "[ENG-42] Fix the login
// redirect" is the line somebody can act on without opening anything.
func subject(name, workItemKey, identifier, workspace, eventType string) string {
	switch {
	case name != "" && workItemKey != "":
		return "[" + workItemKey + "] " + name
	case name != "":
		return "[" + firstOf(identifier, workspace) + "] " + name
	default:
		return "Plane " + eventType
	}
}

// actorName is the person's own name, however this build serialises the
// actor: a bare UUID carries none, an expanded user carries several.
func actorName(activity map[string]any) string {
	actor, ok := activity["actor"].(map[string]any)
	if !ok {
		return ""
	}
	return firstOf(
		strings.TrimSpace(str(actor, "display_name")),
		strings.TrimSpace(strings.TrimSpace(str(actor, "first_name")+" "+str(actor, "last_name"))),
		str(actor, "email"),
	)
}

// refID coerces a foreign key to a lowercased UUID, whether it arrived as a
// string or as an expanded object.
func refID(v any) string {
	if m, ok := v.(map[string]any); ok {
		v = m["id"]
	}
	if v == nil {
		return ""
	}
	return strings.ToLower(fmt.Sprint(v))
}

// ids reads a list of references — assignees, subscribers — in payload order.
func ids(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if id := refID(item); id != "" {
			out = append(out, id)
		}
	}
	return out
}

// pageProjectID finds a page's project, which the serializers spell several
// ways and sometimes nest.
func pageProjectID(data map[string]any) string {
	for _, key := range []string{"project", "project_id"} {
		if id := refID(data[key]); id != "" {
			return id
		}
	}
	// Some builds carry the page's projects as a list, because a page can
	// in principle belong to several. The first is the one that owns it.
	if list := ids(data["projects"]); len(list) > 0 {
		return list[0]
	}
	return ""
}

// str reads a payload field as a string, whatever shape it arrived in.
func str(m map[string]any, key string) string {
	switch v := m[key].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		// Including a float, which is how a sequence number decodes.
		// Go's default formatting renders a whole one as "42" rather
		// than "42.000000", so ENG-42 comes out right with no special
		// case — the language this was ported from needed one.
		return fmt.Sprint(v)
	}
}

func object(m map[string]any, key string) map[string]any {
	if v, ok := m[key].(map[string]any); ok {
		return v
	}
	// An empty map rather than nil, so every reader can index it without
	// asking first.
	return map[string]any{}
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
