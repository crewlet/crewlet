/**
 * The credential a write was made through, in words, with its id behind a
 * copy control.
 *
 * `throughOf` answers the engine's own spelling (`pat:01a1dc39-…`,
 * `session:01a1dc41-…`), which a trail drew as a monospace id that wrapped
 * across two lines on a phone and read as the engine's source everywhere
 * else. A reader asks what KIND of credential it was; the id is for the one
 * who goes looking for that token or session, so it is one press away and
 * whole on the clipboard rather than cut on the screen.
 */

import { Copyable } from "@crewlethq/ui";

/** The credential prefixes the engine writes in `operator_id` (`iam.ActorFor`). */
const KINDS: Readonly<Record<string, string>> = {
  pat: "Personal access token",
  session: "Signed-in session",
};

/** What a credential is called, and the id behind it where it has one. */
export function credentialOf(through: string): { words: string; id?: string } {
  const colon = through.indexOf(":");
  const kind = colon > 0 ? KINDS[through.slice(0, colon)] : undefined;
  if (!kind) return { words: through };
  return { words: kind, id: through.slice(colon + 1) };
}

/** A credential as a cell or a fact: its kind in words, its id copied. */
export function Credential({ through }: { through: string }) {
  const { words, id } = credentialOf(through);
  if (!id) return <span className="truncate">{words}</span>;
  // THE KIND DRAWN, THE ID ON ITS TITLE AND THE CLIPBOARD: the kit's
  // identifier with a copy control, drawing the words in the id's place.
  return (
    <Copyable
      value={id}
      display={words}
      variant="inline"
      ariaLabel={`Copy the ${words.toLowerCase()}'s id, ${id}`}
      title={`Copy ${id}`}
    />
  );
}
