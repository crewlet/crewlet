// One way to reach anything.
//
// The dashboard had no search of any kind: a thirty-seat org was
// navigated by scrolling the roster, and an event or trace id copied out
// of a log could only be opened by hand-editing the URL. Adding a search
// box per view would have meant one ranking rule per view and four places
// to keep them consistent, so this is a single palette over everything
// the page can already address.
//
// `buildResults` is pure so the ranking is testable without a DOM. The
// shell owns the open/closed state and the keyboard, because the palette
// is chrome — it outlives the view under it.

import { flattenSeats } from "./org.js";

// Rooms are matched by their own labels rather than by route, so a
// reader types what they see in the sidebar.
export const ROOMS = [
  { route: "/", label: "Mission Control", hint: "what is happening now" },
  { route: "/work", label: "Work", hint: "turns and sandbox runs" },
  { route: "/activity", label: "Activity", hint: "the event feed" },
  { route: "/org", label: "Org", hint: "chart, directory, charter" },
  { route: "/schedules", label: "Schedules", hint: "recurring work" },
  { route: "/spend", label: "Spend & Budgets", hint: "tokens and caps" },
  { route: "/integrations", label: "Integrations", hint: "inbound surfaces" },
  { route: "/fleet", label: "Fleet", hint: "nodes and seat ownership" },
  { route: "/config", label: "Configuration", hint: "revisions and entities" },
  { route: "/tools", label: "Tools", hint: "the tool registry" },
];

// A 32-character hex id, with or without dashes: an event id or a trace
// id pasted from a log. Which of the two it is cannot be told apart by
// shape, so the palette offers both rather than guessing.
const ID_RE = /^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$|^[0-9a-f]{16,}$/i;

/**
 * Score a candidate against the typed text.
 *
 * Returns a rank (lower is better) or `null` for no match. A prefix beats
 * a word boundary beats a substring, so typing "eng" puts the Engineer
 * seat above a role that merely contains those letters.
 */
function score(text, term) {
  const haystack = String(text || "").toLowerCase();
  if (!term) return 3;
  const idx = haystack.indexOf(term);
  if (idx < 0) return null;
  if (idx === 0) return 0;
  if (/[\s\-_/]/.test(haystack[idx - 1])) return 1;
  return 2;
}

/**
 * Everything the typed text could mean, grouped and ranked.
 *
 * `state` is the store's state; only `org` and `agents` are read.
 */
export function buildResults(state, text) {
  const term = String(text || "").trim().toLowerCase();
  const groups = [];

  const rooms = ROOMS.map((room) => ({ room, rank: score(room.label, term) }))
    .filter((hit) => hit.rank !== null)
    .sort((a, b) => a.rank - b.rank)
    .map((hit) => ({
      kind: "room",
      id: `room:${hit.room.route}`,
      label: hit.room.label,
      hint: hit.room.hint,
      route: hit.room.route,
    }));
  if (rooms.length) groups.push({ title: "Rooms", items: rooms.slice(0, 6) });

  const seats = flattenSeats(state.org || {})
    .map((seat) => {
      const rank = Math.min(
        score(seat.name, term) ?? 99,
        score(seat.handle, term) ?? 99,
      );
      return { seat, rank };
    })
    .filter((hit) => hit.rank < 99)
    .sort((a, b) => a.rank - b.rank)
    .map((hit) => ({
      kind: "seat",
      id: `seat:${hit.seat.handle}`,
      label: hit.seat.name,
      hint:
        hit.seat.kind === "human"
          ? "human seat"
          : hit.seat.unitPath.join(" › ") || "seat",
      route: `/seats/${encodeURIComponent(hit.seat.handle)}`,
    }));
  if (seats.length) groups.push({ title: "Seats", items: seats.slice(0, 8) });

  if (ID_RE.test(term)) {
    groups.push({
      title: "Open by id",
      items: [
        {
          kind: "event",
          id: `event:${term}`,
          label: `Event ${term}`,
          hint: "open this event",
          route: `/events/${encodeURIComponent(term)}`,
        },
        {
          kind: "trace",
          id: `trace:${term}`,
          label: `Trace ${term}`,
          hint: "open the whole turn",
          route: `/traces/${encodeURIComponent(term)}`,
        },
      ],
    });
  }

  return groups;
}

/** The groups flattened, which is what arrow keys move through. */
export function flatResults(groups) {
  return groups.flatMap((group) => group.items);
}
