/**
 * The categories the engine assigns, as a CLOSED set.
 *
 * Mirrors `events.CategoryNames()`, which returns EIGHT, and
 * `internal/events`' category gate holds this list against it in both
 * directions. It held ten once: `communication` and `knowledge` are
 * categories no event is registered under, so two of the event log's chips
 * could never match a row and the reader was invited to filter a log down to
 * nothing and conclude the engine was quiet. A chip for a category with
 * nothing in it is still useful — it says the category exists and is quiet —
 * but only where the category exists.
 */
export const CATEGORIES = [
  "a2a",
  "decision",
  "learning",
  "lifecycle",
  "notification",
  "system",
  "task",
  "webhook",
] as const;
