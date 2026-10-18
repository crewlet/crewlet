/**
 * The bounds the chat screens refuse on early, as the engine bounds them
 * (`internal/chatlet`), and the one thing the screen shares with the engine
 * rather than refusing on: how often a typing flag is re-asserted against the
 * engine's expiry. How a read cursor packs a position is not here: it packs
 * with `positions.ts`'s stride, the one copy of the engine's.
 *
 * Each screen holds a value at its limit while a person is typing, so the
 * refusal arrives in the field rather than after the press. EVERY VALUE HERE
 * IS HELD BY A GATE IN THE PACKAGE THAT OWNS IT, and each one's doc names its
 * gate and what the gate asserts, because they do not all assert the same
 * thing:
 *
 *  - the five limits, EQUAL to `internal/chatlet`'s own constants
 *    (`TestTheDashboardsChatLimitsAreTheEngines`);
 *  - the name grammar, by BEHAVIOUR against `chatlet.ValidName`
 *    (`TestTheDashboardsChannelNameGrammarIsTheEngines`), since two regular
 *    expressions can be spelled differently and mean the same thing;
 *  - the typing throttle, an INEQUALITY — at most half of
 *    `stream.ChatTypingTTL` (`internal/api/stream`'s
 *    `TestTheDashboardReassertsTypingInsideTheEnginesExpiry`).
 *
 * A limit drifts silently from the side that sees it: a copy too low greys
 * out a gesture the engine would take, with nothing anywhere to say so, and a
 * copy too high moves the refusal from the form to the write path — which for
 * a 32 KiB message is the one case where losing the draft costs something.
 *
 * REFUSED RATHER THAN CUT, every one: cutting somebody's sentence to fit is
 * losing what they wrote, and the engine refuses it by name anyway.
 */

/**
 * The longest a room's name may be — `chatlet.MaxChannelName`, sixty-four, held
 * equal to it by `TestTheDashboardsChatLimitsAreTheEngines`, as are the four
 * limits below.
 *
 * Bytes in the engine and characters here without the two ever disagreeing:
 * the name pattern admits ASCII alone, so any string that gets as far as the
 * length check is one byte per character.
 */
export const MAX_CHANNEL_NAME = 64;

/**
 * What a room may be called, over the NORMALISED form — the engine's
 * `chatlet.namePattern`, held to `chatlet.ValidName` by behaviour rather than by
 * spelling: the gate compiles it and asks both sides the same edge cases,
 * because two dialects can write one rule two ways.
 *
 * A STRING rather than a regular expression, because a module here declares
 * data and nothing else; `routes/chatlet/name.ts` compiles it once. Held by
 * `internal/chatlet`'s `TestTheDashboardsChannelNameGrammarIsTheEngines`.
 */
export const CHANNEL_NAME_PATTERN = "^[a-z0-9][a-z0-9_-]{0,63}$";

/** The most a message may carry, in UTF-8 bytes — `chatlet.MaxBody`, 32 KiB. */
export const MAX_BODY_BYTES = 32768;

/**
 * A room's topic and purpose, in UTF-8 bytes — `chatlet.MaxTopic` and
 * `chatlet.MaxPurpose`. BYTES, NOT CHARACTERS: the engine bounds the record's own
 * field, and an emoji in a topic is four of these and one of those.
 */
export const MAX_TOPIC_BYTES = 256;
export const MAX_PURPOSE_BYTES = 600;

/**
 * The most people one direct conversation may hold — `chatlet.MaxDMParticipants`,
 * eight, THE AUTHOR INCLUDED. Past that a conversation is a room, and a room
 * has a name somebody can join by.
 */
export const MAX_DIRECT_PARTICIPANTS = 8;

/**
 * How often a tab re-asserts that somebody is typing, in ms.
 *
 * THREE SECONDS, AT MOST HALF the engine's `stream.ChatTypingTTL` (six): the
 * engine forgets a typing flag nobody re-asserted, which is what clears the
 * indicator under a lid closed mid-word, so a tab re-asserting at the TTL
 * would blink off and on under somebody who never stopped typing. At half of
 * it, a re-assert that came late is invisible and a second one is not.
 *
 * It is also a rate limit on a FLEET-WIDE probe: a focus frame makes the node
 * re-ask its peers who is in the room, so one frame per keystroke would be a
 * scatter per character.
 *
 * Held POSITIVE AND AT MOST HALF of `stream.ChatTypingTTL` — an inequality,
 * not an equality, since any value inside it works — by `internal/api/stream`'s
 * `TestTheDashboardReassertsTypingInsideTheEnginesExpiry`.
 */
export const TYPING_THROTTLE_MS = 3_000;
