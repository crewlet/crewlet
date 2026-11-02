/**
 * What a caveat on a landed write asks of the person who reads it, as the
 * engine names it (`internal/caveat`, ADR-0060).
 *
 * `none` is something to know: shown, and gone by itself. `required` is
 * something somebody has to act on: shown until it is dismissed. The engine
 * states the action per caveat code, so a screen never guesses it from the
 * sentence. HELD AGAINST `caveat.Actions` by
 * `internal/caveat.TestTheDashboardReadsWhatACaveatAsks`: an action copied
 * here and renamed there would render a caveat that needs a person as one that
 * goes by itself.
 */
export const CAVEAT_ACTIONS = ["none", "required"] as const;
