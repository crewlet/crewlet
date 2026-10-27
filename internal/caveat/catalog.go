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

// LaneChangedNotPlaced is a card dropped on a board that moved to its new lane
// and was not placed where it was dropped: item names it as a model reads it,
// and why is the model's sentence about the placement that did not land.
func LaneChangedNotPlaced(item, why string) Caveat {
	return Caveat{code: CodeLaneChangedNotPlaced,
		agent: fmt.Sprintf("%s moved lanes, and was not placed where it was "+
			"dropped: %s", item, why),
		person: "The card moved lanes, but not to the position it was dropped at.",
	}
}

// THE PARTIAL OUTCOMES: a call that landed part of what it was asked. Each
// constructor takes the model's sentence as the site wrote it (the facts and
// the next call only the site holds), and words the person's from the counts
// a screen can show.

// DependenciesNotRecorded is item saved and its dependency change not: agent is
// the model's sentence about it.
func DependenciesNotRecorded(item, agent string) Caveat {
	return Caveat{code: CodeDependenciesNotRecorded, agent: agent,
		person: fmt.Sprintf("%s was saved, but what it waits on was not "+
			"recorded. Set its dependencies again.", item)}
}

// MoveUnfinished is a move whose item landed and of whose total items under it
// only followed came along; waiting says the rest wait on an item in the trash.
func MoveUnfinished(followed, total int, waiting bool, agent string) Caveat {
	if waiting {
		return Caveat{code: CodeMoveWaiting, agent: agent,
			person: fmt.Sprintf("The item moved, but only %d of the %d items "+
				"under it came along. The rest wait for an item in the trash "+
				"under it. Restore it or delete it, and the move finishes.",
				followed, total)}
	}
	return Caveat{code: CodeMoveUnfinished, agent: agent,
		person: fmt.Sprintf("The item moved, but only %d of the %d items under "+
			"it came along so far. The rest follow on their own.", followed, total)}
}

// SubtreeUnfinished is a removal (restored false) or a restore whose item
// landed and of whose total items only followed came along.
//
// THE REMEDY IS ONE THE PERSON CAN REACH, by verb: a restored item leaves the
// trash, so there is nothing of it to restore again, and the rest are restored
// from the trash, one by one; a removed item's rest are still where they were,
// and are removed the way it was, or brought back together by restoring it.
func SubtreeUnfinished(restored bool, followed, total int, agent string) Caveat {
	if restored {
		return Caveat{code: CodeSubtreeUnfinished, agent: agent,
			person: fmt.Sprintf("The item was restored, but only %d of the %d "+
				"items that go with it came back. The rest are still in the "+
				"trash; restore them there.", followed, total)}
	}
	return Caveat{code: CodeSubtreeUnfinished, agent: agent,
		person: fmt.Sprintf("The item is in the trash, but only %d of the %d items "+
			"that go with it came along, and the rest are where they were. Remove "+
			"them the same way, or restore this one from the trash to bring them "+
			"back together.", followed, total)}
}

// DatastorePending is an artifact datastore write on the log that this node
// has not applied yet. Its verdict is not known: the batch can still be
// refused, so the person is told it was sent, not that it was saved, and
// where to see what became of it.
func DatastorePending(agent string) Caveat {
	return Caveat{code: CodeDatastorePending, agent: agent,
		person: "The change was sent, but whether it was applied is not known " +
			"yet. Open the artifact to see whether it was."}
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
