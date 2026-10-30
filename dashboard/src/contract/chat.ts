/**
 * The bounds the chat screens refuse on early, as the engine bounds them
 * (`internal/chatlet`), and the one thing the screen shares with the engine
 * rather than refusing on: how often a typing flag is re-asserted against the
 * engine's lapse. How a read cursor packs a position is not here: it packs
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
 *    `presence.TypingLapse` (`internal/presence`'s
 *    `TestTheDashboardReassertsTypingInsideTheEnginesExpiry`);
 *  - the pending re-ask, EQUAL to `chatlet.DecisionWait`
 *    (`TestTheDashboardAsksAPendingPostAgainAfterTheApplierHadItsWait`);
 *  - the search page, EQUAL to `search.MaxChatSearchLimit`
 *    (`internal/search`'s `TestTheDashboardAsksForTheChatSearchPageTheEngineServes`).
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
 * THREE SECONDS, AT MOST HALF the engine's `presence.TypingLapse` (six): the
 * engine puts a typing line nobody re-asserted back to viewing, which is what
 * clears it under a lid closed mid-word, so a tab re-asserting at the lapse
 * would blink off and on under somebody who never stopped typing. At half of
 * it, a re-assert that came late is invisible and a second one is not.
 *
 * It is also a bound on what the frame costs: each is a `presence` frame, held
 * to the socket's allowance with every other, and one per keystroke would
 * spend it on characters.
 *
 * Held POSITIVE AND AT MOST HALF of `presence.TypingLapse` — an inequality, not
 * an equality, since any value inside it works — by `internal/presence`'s
 * `TestTheDashboardReassertsTypingInsideTheEnginesExpiry`.
 */
export const TYPING_THROTTLE_MS = 3_000;

/**
 * How soon a message answered `pending` is asked again, under its own
 * operation key, in ms (`routes/chatlet/outbox.ts`).
 *
 * TWO SECONDS, `chatlet.DecisionWait` — itself `statelog.ReadBudget`, the wait a
 * read is promised the answering node's applier reaches what it waits for, and
 * the wait the engine's own change feed gives that applier before it derives a
 * wake without its verdict. `pending` is a node that had not applied the post
 * yet, and a message is decided where it is applied, so this is the soonest
 * asking again can be answered with the verdict rather than with `pending`
 * again; the operation ledger answers the same key with that verdict and
 * writes nothing twice. Each answer still pending doubles the wait, up to
 * `RETRY_AFTER_MAX_MS`.
 *
 * Held EQUAL to `chatlet.DecisionWait` by `internal/chatlet`'s
 * `TestTheDashboardAsksAPendingPostAgainAfterTheApplierHadItsWait`.
 */
export const PENDING_REASK_MS = 2_000;

/**
 * How many hits one chat search asks for — the search screen's and the room
 * finder's page.
 *
 * FORTY, `search.MaxChatSearchLimit`: the most the engine answers one search
 * with, whatever it is asked for, because one answer is one tool result for a
 * seat and forty is what still fits its ceiling. The screens asked for fifty
 * (the transcript's page, `routes/chatlet/page.ts`), which the engine clamped
 * without a word — so a full page was forty hits on a screen that took it for
 * fewer than it asked, and read as "everything that matched". Asking for the
 * engine's own page is what lets a screen tell a full page — there may be more,
 * narrow the search — from the whole answer.
 *
 * Held EQUAL to `search.MaxChatSearchLimit` by `internal/search`'s
 * `TestTheDashboardAsksForTheChatSearchPageTheEngineServes`.
 */
export const CHAT_SEARCH_PAGE = 40;
