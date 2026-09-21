/**
 * What a room may be called.
 *
 * A CHANNEL NAME IS AN ADDRESS, not a label. The create arbitrates on it at
 * the broker — two people typing `#launch` contend and exactly one wins — and
 * the same string becomes a claim key, half of a scope path and a broker
 * subject token. That is why the grammar is so narrow, and why there is no
 * escaping alphabet: a character that is legal in one of those three and not
 * the others is precisely the bug escaping would hide.
 *
 * THE RULE IS MIRRORED FROM `internal/chat`, deliberately, and the two halves
 * are not equals. `NormalizeName` lower-cases and trims; `namePattern` is
 * `^[a-z0-9][a-z0-9-]{0,63}$` over the normalised form; `MaxChannelName` is 64.
 * What this copy buys is the round trip: somebody typing `Product Launch!`
 * learns what is wrong while the caret is still in the field, rather than
 * pressing Create and being told by a server that already refused. What it
 * must never buy is a second opinion — so the form only ever REFUSES what the
 * engine would refuse, and where the two disagree the engine's own sentence is
 * what the screen renders (see `start.ts`). A client that admitted something
 * the engine refuses costs one round trip; a client that refused something the
 * engine admits would make a legal name unreachable from this dashboard with
 * nothing anywhere to say so.
 *
 * THE NAME IS IMMUTABLE once the create has claimed it: the value layer has no
 * rename record at all, so this grammar is checked at the one moment it can be
 * — which is the moment a person is typing it.
 */

/**
 * The longest a name may be — `chat.MaxChannelName`.
 *
 * SIXTY-FOUR, and it is bytes in the engine and characters here without the
 * two ever disagreeing: the pattern admits ASCII alone, so any string that
 * gets as far as the length check is one byte per character. A name that is
 * not ASCII is refused for what it CONTAINS, which is the sentence a person
 * can act on, rather than for how many bytes it took.
 */
export const MAX_CHANNEL_NAME = 64;

/** `chat.namePattern`, over the NORMALISED form. */
const NAME = /^[a-z0-9][a-z0-9-]{0,63}$/;

/**
 * The canonical form of what somebody typed — `chat.NormalizeName`.
 *
 * LOWER-CASED AND TRIMMED, because a name is an address: `#Launch` and
 * `#launch ` mean one room, and a company holding both is one where every
 * `#mention` is a coin flip. There is no interior whitespace folding, unlike a
 * page title, because a legal name has no interior whitespace at all.
 */
export function normalizeName(typed: string): string {
  return typed.trim().toLowerCase();
}

/** Whether this is a legal name in its normalised form. */
export function validName(name: string): boolean {
  return NAME.test(name);
}

/**
 * Why what somebody has typed is not a name yet, or "" when it is one.
 *
 * ONE SENTENCE NAMING THE CHARACTER, because "invalid name" tells somebody
 * that they are wrong without telling them what right looks like — and the
 * first illegal character is the one thing the field can point at.
 *
 * AN EMPTY FIELD IS NOT A REFUSAL. Nothing is wrong with a form nobody has
 * filled in yet, and a dialog that opens shouting at a person is one that has
 * refused them before they typed. The Create button is what is disabled; see
 * [startable].
 */
export function nameRefusal(typed: string): string {
  const name = normalizeName(typed);
  if (name === "") return "";
  const bad = [...name].find((char) => !/[a-z0-9-]/.test(char));
  if (bad !== undefined) {
    const what =
      bad === " "
        ? "a space is not part of an address — a hyphen is how a name is spaced"
        : `“${bad}” is not part of an address`;
    return `${what}. A name is lower-case letters, digits and hyphens: it is claimed at the broker and becomes part of a subject, so it holds nothing else.`;
  }
  if (name.startsWith("-")) {
    return "a name starts with a letter or a digit. A leading hyphen reads as punctuation everywhere the name is rendered, and the engine refuses it.";
  }
  if (name.length > MAX_CHANNEL_NAME) {
    return `that is ${name.length} characters and the most a name may be is ${MAX_CHANNEL_NAME}. It is an address people type and a token on a subject, so it is bounded rather than cut to fit.`;
  }
  // UNREACHABLE THROUGH THE THREE CHECKS ABOVE, and stated rather than left to
  // fall through as "this is fine": the pattern is the authority, so anything
  // it refuses that the sentences above did not explain is still a refusal.
  return validName(name)
    ? ""
    : `“${name}” is not a channel name: lower-case letters, digits and hyphens, starting with a letter or a digit, at most ${MAX_CHANNEL_NAME} characters.`;
}

/** Whether this is enough to press Create with: a name, and no refusal. */
export function startable(typed: string): boolean {
  return normalizeName(typed) !== "" && nameRefusal(typed) === "";
}
