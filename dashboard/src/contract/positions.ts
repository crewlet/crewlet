/**
 * How the engine PACKS a state log's position into one number, so the
 * dashboard can unpack it for a person to read.
 *
 * A COPY of `statelog.GenerationStride`, held to it by `internal/api`'s
 * `TestTheDashboardUnpacksAPositionWithTheEnginesStride`. A stride that drifted
 * would still compare correctly — the packed values are compared whole — and
 * would print every re-anchored position as the wrong generation and the wrong
 * sequence, which nothing else would notice.
 */

/**
 * How many sequences one generation of a state log spans in a PACKED position:
 * 2^40. `applied_through` and `log_seq` are (generation × 2^40) + seq, so they
 * order correctly with a plain `<` across a re-anchor. Exact as a JS number
 * while the generation is under 2^13, which no log reaches — a generation
 * moves only when an operator re-anchors one.
 */
export const GENERATION_STRIDE = 1_099_511_627_776;
