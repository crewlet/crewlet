/**
 * The eighteen wake reasons, as the dashboard phrases them.
 *
 * The KEYS are the engine's: `tracker.Reasons`, the one reason of eighteen the
 * applier records per change and per recipient, and
 * `internal/api/queries`' wake-reason gate holds them here in both
 * directions — a reason the engine writes with no phrase renders as its own
 * snake_case value on the one screen a person reads first, and a phrase for a
 * reason nothing writes is what a rename leaves behind. `lib/reasons.ts` is
 * how a screen reads them.
 *
 * TWO VOICES, because there are two readers and the same reason is a
 * different sentence to each.
 *
 * An INBOX is the reader's own: "assigned to you" is the point of the row. A
 * change's ROUTING is about other people — who this woke and why — and the
 * same word there reads as a claim about the reader, who is usually not on the
 * list at all. The Woke tab said "assigned to you" beside a colleague's name.
 *
 * One table with two columns rather than two tables, so a reason added in one
 * voice cannot be missing in the other.
 */
export const PHRASES: Readonly<Record<string, { short: string; about: string; why: string }>> = {
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
