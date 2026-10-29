/**
 * The header every answer the engine's API writes carries, and its one value
 * (`httpjson.EngineHeader`, set around everything a socket serves). An answer
 * without it was written by something in front of the engine (a proxy, a
 * gateway, a cache), whatever its body says, and says nothing about what the
 * engine did. Held to the engine's own by `internal/api`'s
 * `TestTheDashboardKnowsTheEnginesOwnAnswers`.
 */
export const ENGINE_HEADER = "X-Crewlet-Engine";

/** [ENGINE_HEADER]'s one value. */
export const ENGINE_HEADER_VALUE = "1";
