/**
 * The eighteen wake reasons, as English, written once.
 *
 * The applier records, per change and per recipient, the ONE reason of eighteen
 * under which that person heard about it — `internal/tracker/recipients.go`,
 * in the precedence order that decided it. Nothing has ever drawn it, and it
 * is the fact no commercial tracker records: Linear, Jira and ClickUp can all
 * tell you that you were notified, and none of them can tell you why.
 *
 * ONE TABLE, because two would disagree. `asked` on an inbox row and `asked`
 * on an item's routing tab must mean the same thing in the same words, and
 * eighteen raw snake_case values rendered directly read like a log file rather
 * than like a sentence about a person.
 *
 * A reason this build does not know renders as ITSELF rather than vanishing,
 * for the reason the event registry gives: a rolling upgrade puts values a
 * newer node writes in front of an older one's screen.
 */

/*
 * THERE IS NO COPY OF THE PRIMARY SPLIT HERE, deliberately.
 *
 * Which reasons lead an inbox is a property of the PERSON — their own record
 * may override the shipped eight — so the only correct answer is the one the
 * engine states per read, on `primary_reasons`. A constant here would be a
 * second answer to a question the wire already answers, and it would be wrong
 * for anybody who set a preference. It was written that way once, out of the
 * WRONG eight: the engine had two lists of eight both called "primary", one
 * being the reasons a wake obliges a seat to answer. That one is now
 * `Reason.Addressed`.
 */

/*
 * TWO VOICES, because there are two readers and the same reason is a different
 * sentence to each.
 *
 * An INBOX is the reader's own: "assigned to you" is the point of the row. A
 * change's ROUTING is about other people — who this woke and why — and the
 * same word there reads as a claim about the reader, who is usually not on the
 * list at all. The Woke tab said "assigned to you" beside a colleague's name.
 *
 * One table with two columns rather than two tables, so a reason added in one
 * voice cannot be missing in the other.
 */
const PHRASES: Record<string, { short: string; about: string; why: string }> = {
  mention: { short: "mentioned you", about: "mentioned", why: "somebody named you in a comment" },
  prioritised: {
    short: "prioritised for you",
    about: "prioritised for them",
    why: "somebody put this at the top of your list",
  },
  blocking: {
    short: "blocking yours",
    about: "blocks theirs",
    why: "this task blocks one of yours",
  },
  asked: { short: "asked you", about: "asked", why: "a question is waiting on your answer" },
  answered: {
    short: "answered you",
    about: "answered",
    why: "a question you asked has been answered",
  },
  assignee: { short: "assigned to you", about: "assignee", why: "you own this task" },
  unassigned: {
    short: "unassigned from you",
    about: "unassigned",
    why: "you no longer own this task",
  },
  reporter: { short: "you reported it", about: "reporter", why: "you filed this task" },
  thread: { short: "in your thread", about: "in the thread", why: "you are in this conversation" },
  unblocked: { short: "unblocked", about: "unblocked", why: "what this was waiting on is done" },
  routed_to: { short: "routed to you", about: "routed to them", why: "work routes to you now" },
  parent_assignee: {
    short: "under your task",
    about: "owns the parent",
    why: "you own this task's parent",
  },
  checklist: {
    short: "your checklist item",
    about: "has a checklist item",
    why: "you claimed an item on this task",
  },
  collaborator: {
    short: "you collaborate on it",
    about: "collaborator",
    why: "you were brought onto this task",
  },
  watcher: { short: "you watch it", about: "watching", why: "you follow this task" },
  unwatched: {
    short: "you stopped watching",
    about: "stopped watching",
    why: "you were removed as a watcher",
  },
  purged: { short: "purged", about: "purged", why: "this was permanently removed" },
  lead_fallback: {
    short: "routed to you as the lead",
    about: "the unit's lead",
    why: "nobody better was found, and you lead the unit that owns it",
  },
};

/**
 * Two or three words for a chip beside a row.
 *
 * AN ABSENT REASON IS SAID OUT LOUD rather than rendered as an empty chip. The
 * applier writes one on every notice, so a row carrying none is a row that
 * lost it somewhere — and an empty chip is indistinguishable from a chip that
 * was never drawn, which turns the one fact this surface exists to show into
 * a blank nobody reports.
 */
export function reasonPhrase(reason: string): string {
  if (reason === "") return "no reason recorded";
  return PHRASES[reason]?.short ?? reason.replace(/_/g, " ");
}

/**
 * The same reason in the THIRD person, for a surface about somebody else.
 *
 * A change's routing lists colleagues, and [reasonPhrase]'s "assigned to you"
 * beside a colleague's name is a sentence about the wrong person. Falls back
 * to the raw value for the same reason the other does: a newer node's reason
 * must render as itself rather than vanish.
 */
export function reasonAbout(reason: string): string {
  if (reason === "") return "no reason recorded";
  return PHRASES[reason]?.about ?? reason.replace(/_/g, " ");
}

/** A sentence for the detail pane: why this reached this person. */
export function reasonWhy(reason: string): string {
  if (reason === "") {
    return "this notice carries no reason, and the engine writes one on every notice";
  }
  return PHRASES[reason]?.why ?? `the engine recorded the reason “${reason}”`;
}
