/**
 * The one thing a section says to a reader who holds none of its grants.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, once the engine has SAID this
 * reader holds none of what the section needs (`useHeldReading` — the
 * viewer's answer, or the session's where the viewer has not answered — see
 * `lib/viewer.ts`). Every guarded screen used to mount anyway,
 * ask its guarded questions, be refused, and draw the refusal as data: Nodes
 * read "0 nodes" in its header, four stat tiles of 0, "No nodes are reporting"
 * and a banner calling the refusal "the last reading that succeeded", while
 * the Settings column beside it said 1 node. A refused read is UNKNOWN, and a
 * screen that draws unknown as zero is the failure this engine's whole
 * three-valued doctrine exists to prevent. So nothing is drawn but what is
 * true: this section needs a grant, this reader does not hold it, and here is
 * the one thing that changes that from this browser.
 *
 * A GRANT, NOT A CREDENTIAL. This used to say "needs an operator credential"
 * and offer to set a token, which was the whole of authority while a Tier A
 * token was the only credential. A person signs in now, and what a signed-in
 * reader lacks is a grant: the way to one from here is to sign in as somebody
 * who holds it. Granting it is somebody else's change — whoever holds
 * `people:manage` — and the sentence names the grant so the person can ask
 * for it by name.
 *
 * WHO THE READER IS COMES WITH THE REFUSAL, from the one reading it was
 * decided on (`reader`) — never from the viewer beside it. Decided on the
 * session's grants in the moment before the socket's viewer answers, the
 * refusal read the viewer's empty login and told a person signed in that
 * nobody was, on every cold load of a section closed to them.
 *
 * THE BUTTON SAYS WHAT IT DOES, and the grant is said once, above it. It
 * repeated the grants — "Sign in as somebody holding people:manage or
 * audit:read" — which a button cannot wrap, so on a phone it ran past both
 * edges of the screen; the words are the product's own everywhere else
 * (the palette, the org builder).
 *
 * A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT REFUSED. `loading` covers a
 * viewer read that failed as well as one in flight, and treating either as
 * "holds nothing" would lock a reader out of every guarded section until the
 * next poll. The frame waits out the FIRST read (`asking`) with a skeleton,
 * since the section's own reads would only be refused; after a read that
 * FAILED the screen mounts, and its own reads say what they find.
 *
 * AND A PANEL THAT NEVER ASKS says the same thing in the same words, at the
 * panel's size (`compact`): a seat's Settings tab sends no read of the
 * company document to a reader without `config:read`, and said nothing true
 * in its place until it drew this. `also` is the one sentence such a panel
 * adds — another way to the same answer, as leading the seat's unit is.
 */

import type { ReactNode } from "react";
import { Button, EmptyState, InlineCode } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { goSignIn } from "~/lib/session.ts";
import { grantWords, type Grant } from "../nav.ts";

export function GrantRequired({
  what,
  grants,
  reader,
  size,
  also,
}: {
  what: string;
  grants: readonly Grant[];
  /**
   * The login the reader is signed in as, or "" for nobody — from the reading
   * the refusal was decided on (`useHeldReading`), never a second one.
   */
  reader: string;
  /** `compact` inside a panel, rather than in place of a screen. */
  size?: "compact";
  /** One more sentence after the remedy: another way to the same answer. */
  also?: ReactNode;
}) {
  const needs = grantWords(grants);
  return (
    <EmptyState
      {...(size ? { size } : {})}
      icon={<KeyGlyph />}
      title={`${what} needs ${needs}`}
      description={
        reader ? (
          <>
            You are signed in as <InlineCode>{reader}</InlineCode>, which holds{" "}
            {grants.length > 1 ? "none of them" : "no such grant"}. Somebody holding{" "}
            <InlineCode>people:manage</InlineCode> can grant it to you.
            {also ? <> {also}</> : null}
          </>
        ) : (
          <>
            Nobody is signed in in this browser.
            {also ? <> {also}</> : null}
          </>
        )
      }
      action={
        <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
          {reader ? "Sign in as somebody else" : "Sign in"}
        </Button>
      }
    />
  );
}
