package webhooks

import (
	"strconv"
	"strings"
)

// The one-line summaries the activity feed shows beside each delivery.
//
// They read the payload defensively through the accessors below, because a
// webhook body is the one input here with no schema: a provider adds a field,
// a self-hosted fork renames one, and a summary builder that indexed blindly
// would turn a cosmetic difference into a 500 on a verified delivery.

// preview trims a title to n runes and marks that it was cut.
//
// Runes, not bytes: a byte slice through UTF-8 leaves a broken code point,
// which renders as a replacement character in the feed and, worse, is invalid
// JSON's problem to explain rather than the trimming's.
func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// join assembles a summary from the parts that are present.
//
// The empty filter is what keeps an absent field from becoming a gap in the
// line: a webhook body routinely omits half of what a summary would like, and
// joining blindly produces "GitHub  opened PR #12" with a doubled space, or a
// line that starts or ends with one.
func join(parts ...string) string {
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

func quoted(s string, n int) string {
	if s == "" {
		return ""
	}
	return `"` + preview(s, n) + `"`
}

func slackSummary(handle string, body map[string]any) string {
	event := object(body, "event")
	kind := str(event, "type")
	if kind == "" {
		kind = str(body, "type")
	}
	user := str(event, "user")
	channel := str(event, "channel")
	text := str(event, "text")

	lead := "Slack → " + handle
	switch kind {
	case "message":
		who := user
		if who == "" {
			who = "someone"
		}
		what := who + " sent a message"
		if text != "" {
			what = who + ` said ` + quoted(text, 80)
		}
		where := ""
		if channel != "" {
			where = "in #" + channel
		}
		return join(lead, what, where)
	case "app_mention":
		if user != "" {
			return join(lead, "mentioned by "+user)
		}
		return join(lead, "app mentioned")
	case "reaction_added":
		if reaction := str(event, "reaction"); reaction != "" {
			return join(lead, ":"+reaction+": by "+user)
		}
		return join(lead, "reaction added")
	case "":
		return lead
	default:
		return join(lead, strings.ReplaceAll(kind, "_", " "))
	}
}

func jiraSummary(body map[string]any) string {
	issue := object(body, "issue")
	action := strings.ReplaceAll(
		strings.TrimPrefix(str(body, "webhookEvent"), "jira:"), "_", " ")
	return join("Jira",
		str(object(body, "user"), "displayName"),
		action,
		str(issue, "key"),
		quoted(str(object(issue, "fields"), "summary"), 60))
}

func githubSummary(event string, body map[string]any) string {
	action := str(body, "action")
	sender := str(object(body, "sender"), "login")
	repo := str(object(body, "repository"), "full_name")

	var what string
	switch event {
	case "push":
		branch := strings.TrimPrefix(str(body, "ref"), "refs/heads/")
		what = "pushed " + strconv.Itoa(len(list(body, "commits"))) + " commit(s) to " + branch
	case "pull_request":
		pr := object(body, "pull_request")
		what = join(action+" PR #"+num(pr, "number"), quoted(str(pr, "title"), 60))
	case "issues":
		issue := object(body, "issue")
		what = join(action+" issue #"+num(issue, "number"), quoted(str(issue, "title"), 60))
	default:
		what = join(event, action)
	}
	where := ""
	if repo != "" {
		where = "on " + repo
	}
	return join("GitHub", sender, what, where)
}

func gitlabSummary(event string, body map[string]any) string {
	attrs := object(body, "object_attributes")
	kind := str(body, "object_kind")
	if kind == "" {
		kind = event
	}
	if kind == "" {
		kind = "event"
	}
	action := str(attrs, "action")

	var what string
	switch kind {
	case "merge_request":
		what = join(orElse(action, "updated")+" MR !"+num(attrs, "iid"),
			quoted(str(attrs, "title"), 60))
	case "issue":
		what = join(orElse(action, "updated")+" issue #"+num(attrs, "iid"),
			quoted(str(attrs, "title"), 60))
	case "note":
		what = "commented on " + orElse(str(attrs, "noteable_type"), "item")
	case "pipeline":
		what = join("pipeline", str(attrs, "status"))
	default:
		what = join(kind, action)
	}
	where := ""
	if path := str(object(body, "project"), "path_with_namespace"); path != "" {
		where = "on " + path
	}
	return join("GitLab", str(object(body, "user"), "username"), what, where)
}

func confluenceSummary(body map[string]any) string {
	event := firstOf(body, "event", "webhookEvent", "eventType")
	page := object(body, "page")
	if len(page) == 0 {
		page = object(body, "content")
	}
	space := object(page, "space")
	if len(space) == 0 {
		space = object(body, "space")
	}
	user := object(body, "user")
	who := str(user, "displayName")
	if who == "" {
		who = str(user, "name")
	}
	where := ""
	if key := str(space, "key"); key != "" {
		where = "[" + key + "]"
	}
	return join("Confluence", who, strings.ReplaceAll(event, "_", " "),
		where, quoted(str(page, "title"), 60))
}

func forgeSummary(source, legacy string, body map[string]any) string {
	switch source {
	case "confluence":
		page := object(body, "page")
		title := str(page, "title")
		where := ""
		if key := str(object(page, "space"), "key"); key != "" {
			where = "[" + key + "]"
		}
		what := strings.ReplaceAll(legacy, "_", " ")
		if len(object(body, "comment")) > 0 {
			if title == "" {
				return join("Forge", where, what)
			}
			return join("Forge", where, what, "on "+quoted(title, 50))
		}
		return join("Forge", where, what, quoted(title, 50))
	case "jira":
		issue := object(body, "issue")
		what := strings.ReplaceAll(strings.TrimPrefix(legacy, "jira:"), "_", " ")
		return join("Forge", what, str(issue, "key"),
			quoted(str(object(issue, "fields"), "summary"), 50))
	default:
		return join("Forge", legacy)
	}
}

func orElse(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// datadogSummary glosses a monitor alert for the event feed.
func datadogSummary(body map[string]any) string {
	parts := []string{"Datadog"}

	if transition := str(body, "alert_transition"); transition != "" {
		parts = append(parts, transition)
	}
	if title := str(body, "title"); title != "" {
		parts = append(parts, title)
	}
	if priority := str(body, "priority"); priority != "" {
		parts = append(parts, "P"+priority)
	}

	return strings.Join(parts, " · ")
}
