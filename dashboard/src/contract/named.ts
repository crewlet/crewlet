/**
 * What an engine line names (`internal/named`): the kinds of object a feed
 * line, a turn's trigger or a wake's subject names beside its words, each by
 * an id its own page reads.
 *
 * Held to the engine by `internal/api/queries`'
 * `TestTheDashboardNamesWhatTheEngineNames`, in the engine's order.
 */

/** Every kind a line names (`named.Kinds`). */
export const NAMED_REF_KINDS = [
  "work_item",
  "code_change",
  "commit",
  "repository",
  "page",
  "artifact",
  "turn",
  "schedule",
  "a2a_channel",
  "seat",
  "chat_room",
] as const;

/**
 * The most references one `references` ask resolves
 * (`queries.MaxReferences`); a page drawing more asks in several.
 */
export const MAX_REFERENCES = 100;
