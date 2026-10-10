/**
 * A turn told as a story a member may read (`internal/api/queries`, story.go):
 * the closed sets a story names.
 *
 * Each is a COPY of the engine's, because this is a separate build that cannot
 * import a Go identifier — held to it by
 * `internal/api/queries.TestTheDashboardNamesEveryStoryValue`. A value that
 * lands in Go without landing here is a story card drawn with a bare word, or
 * with nothing, for ever and silently; what each value is CALLED is behaviour,
 * and lives in `lib/stories.ts`.
 */

/** How a session ended, or that it has not — `queries.StoryOutcomes`. */
export const STORY_OUTCOMES = [
  "approved",
  "sent_back",
  "failed",
  "waiting_on_you",
  "running",
] as const;

/**
 * What set a session going — `queries.StoryOrigins` — which is also why it may
 * be told: `private` is a session told only because it is charged to a work
 * item, and so told without what set it going.
 */
export const STORY_ORIGINS = ["task", "page", "chat", "schedule", "colleague", "private"] as const;

/** What a session did beyond answering — `queries.StoryActionKinds`. */
export const STORY_ACTIONS = [
  "read_page",
  "coding_started",
  "coding_asked",
  "coding_answered",
  "coding_finished",
] as const;
