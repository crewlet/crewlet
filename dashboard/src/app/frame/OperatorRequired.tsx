/**
 * The one thing a guarded section says to a reader without an operator
 * credential.
 *
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, once the engine has SAID this
 * viewer cannot read it (`viewer.operator` false, and the viewer answered —
 * see `lib/viewer.ts`). Every guarded screen used to mount anyway, ask its
 * guarded questions, be refused, and draw the refusal as data: Nodes read
 * "0 nodes" in its header, four stat tiles of 0, "No nodes are reporting" and
 * a banner calling the refusal "the last reading that succeeded", while the
 * Settings column beside it said 1 node. A refused read is UNKNOWN, and a
 * screen that draws unknown as zero is the failure this engine's whole
 * three-valued doctrine exists to prevent. So nothing is drawn but what is
 * true: this section needs a credential, and here is where to set one.
 *
 * A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT REFUSED. `loading` covers a
 * viewer read that failed as well as one in flight, and treating either as
 * "no credential" would lock an operator out of every guarded section until
 * the next poll. The frame waits out the FIRST read (`asking`) with a
 * skeleton, since the section's own reads would only be refused; after a read
 * that FAILED the screen mounts, and its own reads say what they find.
 */

import { Button, EmptyState, InlineCode } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { apiToken, requestToken } from "~/protocol/index.ts";

export function OperatorRequired({ what }: { what: string }) {
  // A TOKEN THE ENGINE DID NOT ACCEPT is a different fault from no token, and
  // the reader fixing it looks for a different thing: a wrong credential, not
  // a missing one. The builder's own refusal words it the same way.
  const stored = apiToken() !== "";
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title={
        stored ? `This browser's token cannot open ${what}` : `${what} needs an operator credential`
      }
      description={
        <>
          {stored
            ? "The engine did not accept it as an operator's. Set a token matching one of your "
            : "It is guarded, reads included. Set an API token matching one of your "}
          <InlineCode>api.auth.tokens</InlineCode> entries.
        </>
      }
      action={
        <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          {stored ? "Change the token" : "Set token"}
        </Button>
      }
    />
  );
}
