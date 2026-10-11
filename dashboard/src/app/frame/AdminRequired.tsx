/**
 * The one thing a console address says to a reader whose key does not reach
 * it.
 *
 * EVERY CONSOLE ADDRESS IS AN ADMIN'S (ADR-0031): what the machine processed
 * and how it is run — nodes, keys, secrets, integrations, the configuration —
 * and it is answered as one rather than as Not Found, because the address is
 * right and it is the reader's key that does not reach it.
 * DRAWN BY THE FRAME, IN PLACE OF THE SCREEN, once the engine has SAID this
 * viewer does not reach it (`viewer.admin` false, and the viewer answered —
 * see `lib/viewer.ts`). Every guarded screen used to mount anyway, ask its
 * guarded questions, be refused, and draw the refusal as data: Nodes read
 * "0 nodes" in its header, four stat tiles of 0, "No nodes are reporting" and
 * a banner calling the refusal "the last reading that succeeded", while the
 * navigation beside it counted 1 node. A refused read is UNKNOWN, and a
 * screen that draws unknown as zero is the failure this engine's whole
 * three-valued doctrine exists to prevent. So nothing is drawn but what is
 * true: this section is for admins, and here is what changes that.
 *
 * TWO READERS, TWO REMEDIES — IN A PERSON'S VOICE. The reader is a person
 * who followed a link into the console, so it speaks as the person shell does:
 * no API, no `api.auth`, no role field. With no key at all the remedy is to
 * sign in with an admin's access key, and the sign-in is offered. With a
 * MEMBER's key the key works — it reaches the company's work, pages and chart
 * — and another paste of it changes nothing, so the remedy is a person: the
 * first admin the engine named, asked by name. Both carry the way back to the
 * reader's own day, because a refusal with no way out is a dead end in the
 * middle of somebody's morning.
 *
 * A VIEWER NOBODY HAS ANSWERED FOR YET IS NOT REFUSED. `loading` covers a
 * viewer read that failed as well as one in flight, and treating either as
 * "not an admin" would lock an admin out of the whole console until the
 * next poll. The frame waits out the FIRST read (`asking`) with a skeleton,
 * since the section's own reads would only be refused; after a read that
 * FAILED the screen mounts, and its own reads say what they find.
 */

import { Button, ButtonLink, EmptyState } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { requestToken } from "~/protocol/index.ts";
import { adminToAsk, useViewer } from "~/lib/viewer.ts";

export function AdminRequired({ what }: { what: string }) {
  const viewer = useViewer();
  const back = (
    <ButtonLink variant="secondary" href={href(["today"])}>
      Go to Today
    </ButtonLink>
  );
  if (!viewer.anonymous) {
    return (
      <EmptyState
        icon={<KeyGlyph />}
        title={`${what} is for admins`}
        description={`It is part of the admin console. If you need something changed here, ask ${adminToAsk(viewer.admins)}.`}
        action={back}
      />
    );
  }
  return (
    <EmptyState
      icon={<KeyGlyph />}
      title={`${what} is for admins`}
      description="It is part of the admin console. Sign in with an admin's access key to open it."
      action={
        <span className="row gap-2 wrap">
          <Button variant="primary" leadingIcon={<KeyGlyph />} onClick={requestToken}>
            Sign in
          </Button>
          {back}
        </span>
      }
    />
  );
}
