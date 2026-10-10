/**
 * The one thing a guarded section says to a reader whose key does not reach
 * it.
 *
 * EVERY GUARDED SECTION IS AN ADMIN'S (ADR-0031): what the machine processed
 * and how it is run — nodes, keys, secrets, integrations, the configuration.
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, once the engine has SAID this
 * viewer does not reach it (`viewer.admin` false, and the viewer answered —
 * see `lib/viewer.ts`). Every guarded screen used to mount anyway, ask its
 * guarded questions, be refused, and draw the refusal as data: Nodes read
 * "0 nodes" in its header, four stat tiles of 0, "No nodes are reporting" and
 * a banner calling the refusal "the last reading that succeeded", while the
 * Settings column beside it said 1 node. A refused read is UNKNOWN, and a
 * screen that draws unknown as zero is the failure this engine's whole
 * three-valued doctrine exists to prevent. So nothing is drawn but what is
 * true: this section is for admins, and here is what changes that.
 *
 * TWO READERS, TWO REMEDIES. With no key at all the remedy is a key, and the
 * token dialog is offered. With a MEMBER's key the key works — it reaches the
 * company's work, pages and chart — and another paste of it changes nothing,
 * so the remedy is a person: the first admin the engine named.
 *
 * A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT REFUSED. `loading` covers a
 * viewer read that failed as well as one in flight, and treating either as
 * "not an admin" would lock an admin out of every guarded section until the
 * next poll. The frame waits out the FIRST read (`asking`) with a skeleton,
 * since the section's own reads would only be refused; after a read that
 * FAILED the screen mounts, and its own reads say what they find.
 */

import { Button, EmptyState, InlineCode } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { requestToken } from "~/protocol/index.ts";
import { adminToAsk, useViewer } from "~/lib/viewer.ts";

export function AdminRequired({ what }: { what: string }) {
  const viewer = useViewer();
  if (!viewer.anonymous) {
    return (
      <EmptyState
        icon={<KeyGlyph />}
        title={`${what} is for admins`}
        description={`Your key is a member's: it reaches the company's work, pages and chart. Only an admin can open this — ask ${adminToAsk(viewer.admins)}.`}
      />
    );
  }
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title={`${what} is for admins`}
      description={
        <>
          It needs an admin&rsquo;s API key, reads included. Set a key matching one of your{" "}
          <InlineCode>api.auth.tokens</InlineCode> entries with <InlineCode>role: admin</InlineCode>
          .
        </>
      }
      action={
        <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={requestToken}>
          Set token
        </Button>
      }
    />
  );
}
