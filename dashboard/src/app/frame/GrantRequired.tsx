/**
 * The one thing a section says to a reader who holds none of its grants.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, once the engine has SAID this
 * viewer holds none of what the section needs (`viewer.grants`, and the viewer
 * answered — see `lib/viewer.ts`). Every guarded screen used to mount anyway,
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
 * A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT REFUSED. `loading` covers a
 * viewer read that failed as well as one in flight, and treating either as
 * "holds nothing" would lock a reader out of every guarded section until the
 * next poll. The frame waits out the FIRST read (`asking`) with a skeleton,
 * since the section's own reads would only be refused; after a read that
 * FAILED the screen mounts, and its own reads say what they find.
 */

import { Button, EmptyState, InlineCode } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { goSignIn } from "~/lib/session.ts";
import { useViewer } from "~/lib/viewer.ts";
import { grantWords, type Grant } from "../nav.ts";

export function GrantRequired({ what, grants }: { what: string; grants: readonly Grant[] }) {
  const viewer = useViewer();
  const needs = grantWords(grants);
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title={`${what} needs ${needs}`}
      description={
        viewer.login ? (
          <>
            You are signed in as <InlineCode>{viewer.login}</InlineCode>, which holds{" "}
            {grants.length > 1 ? "none of them" : "no such grant"}. Somebody holding{" "}
            <InlineCode>people:manage</InlineCode> can grant it to you.
          </>
        ) : (
          "Nobody is signed in in this browser."
        )
      }
      action={
        <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={goSignIn}>
          {`Sign in as somebody holding ${needs}`}
        </Button>
      }
    />
  );
}
