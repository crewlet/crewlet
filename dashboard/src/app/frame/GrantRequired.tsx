/**
 * The one thing a section says to a reader who holds none of its access.
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
 * true: this section needs access this reader does not hold, and here is
 * the one thing that changes that from this browser.
 *
 * ACCESS, NOT A CREDENTIAL. This used to say "needs an operator credential"
 * and offer to set a token, which was the whole of authority while a Tier A
 * token was the only credential. A person signs in now, and what a signed-in
 * reader lacks is ACCESS, Admin or an app's (`lib/access.ts`): "Search needs
 * Doclet access. An admin can give it to you." Giving it is somebody else's
 * change, an admin's, and the sentence names the access by the word People &
 * access uses for it, so the person can ask for it by name.
 *
 * WHO THE READER IS COMES WITH THE REFUSAL, from the one reading it was
 * decided on (`reader`) — never from the viewer beside it. Decided on the
 * session's grants in the moment before the socket's viewer answers, the
 * refusal read the viewer's empty login and told a person signed in that
 * nobody was, on every cold load of a section closed to them.
 *
 * THE COMMON NEXT STEP COMES FIRST. Most readers refused a section should go
 * back to their work, so the primary action is Go to Home and Switch account
 * is the secondary one. The title names only the access ("You need Doclet
 * access"): the page's own name is already in the header, and "Doclet needs
 * Doclet access" said it twice. A panel (`compact`) draws the line
 * alone, with no button, so a small card stays small.
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
 * company document to a reader without Admin, and said nothing true
 * in its place until it drew this. `also` is the one sentence such a panel
 * adds — another way to the same answer, as leading the seat's unit is.
 */

import type { ReactNode } from "react";
import { Button, ButtonLink, EmptyState, InlineCode } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { goSignIn } from "~/lib/session.ts";
import { accessNames, type Grant } from "~/lib/access.ts";

export function GrantRequired({
  grants,
  reader,
  size,
  also,
}: {
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
  // A LINK, since Home is a page: ⌘-click opens it in a tab.
  const actions = reader ? (
    <span className="row gap-2">
      <ButtonLink variant="primary" href={href([])}>
        Go to Home
      </ButtonLink>
      <Button variant="secondary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
        Switch account
      </Button>
    </span>
  ) : (
    <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
      Sign in
    </Button>
  );
  return (
    <EmptyState
      {...(size ? { size } : {})}
      icon={<KeyGlyph />}
      title={`You need ${accessNames(grants)} access`}
      description={
        reader ? (
          <>
            Signed in as <InlineCode>{reader}</InlineCode>. Ask an admin for access.
            {also ? <> {also}</> : null}
          </>
        ) : (
          <>
            You’re not signed in.
            {also ? <> {also}</> : null}
          </>
        )
      }
      {...(size === "compact" ? {} : { action: actions })}
    />
  );
}
