/**
 * How the engine PACKS a state log's position into one number, so the
 * dashboard can unpack it for a person to read — and the chat screen can PACK
 * a read cursor in the same number space.
 *
 * A COPY of `statelog.GenerationStride`, held to it, and to packing as
 * `statelog.Position.Packed` packs, by `internal/api`'s
 * `TestTheDashboardUnpacksAPositionWithTheEnginesStride`. Drift is silent in
 * both uses. Unpacking, a wrong stride still compares correctly — the packed
 * values are compared whole — and prints every re-anchored position as the
 * wrong generation and the wrong sequence, which nothing else would notice.
 * Packing, it is worse: a read cursor is ONE INTEGER the coordination record
 * only ever compares with the packed position of a room's newest message
 * (`routes/chatlet/cursor.ts`), so a stride too high writes a cursor above every
 * message and a room reads as read for ever, and one too low leaves a badge
 * that never clears however far somebody reads.
 */

/**
 * How many sequences one generation of a state log spans in a PACKED position:
 * 2^40. `applied_through` and `log_seq` are (generation × 2^40) + seq, so they
 * order correctly with a plain `<` across a re-anchor. Exact as a JS number
 * while the generation is under 2^13, which no log reaches — a generation
 * moves only when an operator re-anchors one.
 */
export const GENERATION_STRIDE = 1_099_511_627_776;
