/**
 * The dictation socket's words, held to `internal/api/dictation` and
 * `internal/speech` by `internal/api/dictation`'s
 * `TestTheDashboardReadsTheDictationSocket`.
 */

/** Why this engine cannot dictate (`speech.Reason`), on the viewer's `dictation`. */
export type DictationReason = "disabled" | "unsupported_platform" | "model_unavailable" | "failed";

/** How a dictation ended, as `done`'s `reason`. */
export type DictationDoneReason = "finish" | "cancel" | "length" | "idle" | "preparing";
