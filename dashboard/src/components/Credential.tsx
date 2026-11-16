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

import { Copyable, TableTwoLineCell } from "@crewlethq/ui";
import { authorParts, credentialOf } from "~/lib/attribution.ts";

/**
 * A credential as a cell or a fact: its kind in words, its id copied. One
 * reading of the engine's spelling (`credentialOf`), shared with the
 * sentences that name a credential, so a cell and a line cannot disagree.
 */
export function Credential({ through }: { through: string }) {
  const credential = credentialOf(through);
  if (!credential) return <span className="truncate">{through}</span>;
  const words = credential.kind.charAt(0).toUpperCase() + credential.kind.slice(1);
  // THE KIND DRAWN, THE ID ON ITS TITLE AND THE CLIPBOARD: the kit's
  // identifier with a copy control, drawing the words in the id's place.
  return (
    <Copyable
      value={credential.id}
      display={words}
      displayWords
      variant="inline"
      ariaLabel={`Copy the ${credential.kind}'s id, ${credential.id}`}
      title={`Copy ${credential.id}`}
    />
  );
}

/**
 * A recorded author as a table cell: the name, and the credential they wrote
 * through on a quieter line under it ([authorParts]), so a narrow column cuts
 * neither into the other.
 */
export function AuthorCell({
  author,
  operatorId,
  name,
}: {
  author: string;
  operatorId: string | undefined;
  /** The reader's name for the author, where the caller has a better one. */
  name?: string | undefined;
}) {
  const parts = authorParts(author, operatorId, name);
  return <TableTwoLineCell value={parts.name} detail={parts.through || undefined} />;
}
