/**
 * A git object's id as the engine's repositories write one, and the width a
 * code search plans on.
 *
 * ITS OWN MODULE, apart from `code.ts`, because the frame reads these — the
 * router knows a commit's page by a whole id, and the palette's Code scope
 * waits for a term a trigram long — while `code.ts` carries Gitlet's words and
 * vocabularies, which would otherwise ride the first page every reader loads.
 */

/**
 * How many hex digits a whole object id has (`gitref.OIDHexLen`): the
 * engine's repositories are object-format sha1. A commit's page is named by a
 * whole id, and git's null id is this many zeros. And the fewest digits of an
 * abbreviated id the engine takes back (`code.MinAbbrevDigits`), git's own
 * default abbreviation, which is what a row prints. Held by
 * `internal/code.TestTheDashboardNamesAnObjectAsTheEnginesRepositoriesDo`.
 */
export const OBJECT_ID_HEX_DIGITS = 40;
/** See [OBJECT_ID_HEX_DIGITS]. */
export const OBJECT_ID_MIN_ABBREV_DIGITS = 7;

/**
 * A trigram's width in bytes: the shortest literal Gitlet's code search plans
 * on, which refuses `bad_params` for a term with none. Held by
 * `internal/codeindex.TestTheDashboardSearchesCodeAtTheTrigramsWidth`.
 */
export const CODE_SEARCH_TRIGRAM_BYTES = 3;
