/**
 * The `viewer` answers a suite mounts a screen as.
 *
 * What a reader may see is decided by what the viewer answer says they hold
 * (`lib/access.ts`), so a suite that means to read a screen whole reads it as
 * an ADMIN — who holds every app — and one about a lock names the access it
 * means. Answered by the socket, never assumed: an unanswered viewer is a
 * reader the frame is still asking about.
 */

/** An admin, bound to the seat `jane`: every app is included — and listed, as
 *  the engine reports an admin's grants, expanded. */
export const ADMIN_VIEWER = {
  login: "founder",
  owner: "jane",
  handle: "jane",
  name: "Jane Founder",
  kind: "human",
  grants: ["admin", "worklet", "doclet", "chatlet"],
  acts: [],
};

/**
 * What the frame itself asks of the socket on every screen, answered as the
 * engine answers an admin with nothing waiting: who is reading, and the inbox
 * and queue counts a reader holding Worklet is asked for. A suite's own
 * answers come first.
 */
export const FRAME_ANSWERS: Readonly<Record<string, unknown>> = {
  viewer: ADMIN_VIEWER,
  work_inbox: { notices: [] },
  work_items: { items: [] },
};
