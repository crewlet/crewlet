package caveat

import (
	"fmt"
	"strings"
)

// THE CATALOG: one constructor per code, each writing both phrasings from the
// same facts, so the two can never describe different events. The agent's
// words name the tool and argument a model would use next; the person's name
// only what a person sees on a screen: a handle as `@handle`, a project by its
// key, a field by its name.

// UnaddressedComment is a comment from somebody other than the assignee that
// neither asks nor mentions anybody: it wakes assignee, who is entitled to
// absorb it without replying.
func UnaddressedComment(assignee string) Caveat {
	return Caveat{code: CodeUnaddressedComment,
		agent: fmt.Sprintf("%s is woken by this but is not asked to answer it. "+
			"Use ask: %s, or @-mention them, if you are expecting a reply.",
			assignee, assignee),
		person: fmt.Sprintf("@%s will see this comment, but it does not ask "+
			"them anything. Ask them a question or mention them if you expect "+
			"a reply.", assignee),
	}
}

// DefaultAssigneeGone is a create that named nobody in project, whose default
// assignee handle is no longer a seat on the chart: the item was filed to
// nobody and lands in triage.
func DefaultAssigneeGone(project, handle string) Caveat {
	return Caveat{code: CodeDefaultAssigneeGone,
		agent: fmt.Sprintf("%s's default assignee %q is not a seat on the org "+
			"chart any more, so this task was filed to nobody and lands in "+
			"triage. The project lead sets another with write_project "+
			"`default_assignee`.", project, handle),
		person: fmt.Sprintf("%s's default assignee, @%s, is no longer on the "+
			"org chart, so this item is unassigned and waiting in triage. "+
			"Assign it to someone, and ask %s's lead to choose a new default "+
			"assignee.", project, handle, project),
	}
}

// BodyLinksCapped is a body naming found distinct item keys, of which only the
// first linked are linked.
func BodyLinksCapped(found, linked int) Caveat {
	return Caveat{code: CodeBodyLinksCapped,
		agent: fmt.Sprintf("this body names %d distinct task keys and the first "+
			"%d are linked; the rest are left as plain text", found, linked),
		person: fmt.Sprintf("This text names %d items. Only the first %d are "+
			"linked; the rest stay plain text.", found, linked),
	}
}

// FieldDateTruncated is a timestamp typed into a date field (slug, shown as
// name) that holds no time: it was stored as day, the day it falls on in zone.
func FieldDateTruncated(slug, name, typed, day, zone string) Caveat {
	label := strings.TrimSpace(name)
	if label == "" {
		label = slug
	}
	return dateTruncated(fmt.Sprintf("field %q", slug), label, typed, day, zone)
}

// TargetDateTruncated is a timestamp given as a project's target date, which
// holds a day: it was stored as day, the day it falls on in zone.
func TargetDateTruncated(typed, day, zone string) Caveat {
	return dateTruncated("the target date", "The target date", typed, day, zone)
}

func dateTruncated(agentWhat, personWhat, typed, day, zone string) Caveat {
	return Caveat{code: CodeDateTruncated,
		agent: fmt.Sprintf("%s holds a date and not a time, so %s was stored "+
			"as %s, the day it falls on in %s", agentWhat, typed, day, zone),
		person: fmt.Sprintf("%s holds a date without a time, so it was saved "+
			"as %s, the day %s falls on in %s.", personWhat, day, typed, zone),
	}
}

// TagNearDuplicate is a tag slug declared in project within a typo of the near
// tags it already keeps. It was declared anyway.
func TagNearDuplicate(project, slug string, near []string) Caveat {
	others := strings.Join(near, ", ")
	return Caveat{code: CodeTagNearDuplicate,
		agent: fmt.Sprintf("%s is within a typo of %s. It was declared anyway; "+
			"merge them if that was not deliberate", slug, others),
		person: fmt.Sprintf("The new tag %s is spelled almost like %s. It was "+
			"added anyway. If that was a typo, ask %s's lead to merge them.",
			slug, others, project),
	}
}

// FiledEarlierUnread is a create an earlier attempt of the same operation
// already filed as task, whose key this writer had no store to read back.
func FiledEarlierUnread(task string) Caveat {
	return Caveat{code: CodeFiledEarlierUnread,
		agent: fmt.Sprintf("task %s was filed by an earlier copy of this "+
			"operation, and this writer has no store to read its key from", task),
		person: "An earlier attempt of this change already filed the item, and " +
			"this server could not read it back to show it.",
	}
}

// FiledEarlierRemoved is a create an earlier attempt of the same operation
// already filed as task, which is no longer on this node.
func FiledEarlierRemoved(task string) Caveat {
	return Caveat{code: CodeFiledEarlierRemoved,
		agent: fmt.Sprintf("task %s was filed by an earlier copy of this "+
			"operation and is no longer on this node", task),
		person: "An earlier attempt of this change already filed the item, and " +
			"it has since been deleted.",
	}
}

// FileFinding is a finding about a file its caller authored: where names the
// file (and line), and finding is the linter's sentence. The finding is about
// the file's own text, which a model and a person who wrote it read alike, so
// both phrasings are the same words.
func FileFinding(where, finding string) Caveat {
	text := where + ": " + finding
	person := text
	if !strings.HasSuffix(person, ".") {
		person += "."
	}
	return Caveat{code: CodeFileFinding, agent: text, person: person}
}
