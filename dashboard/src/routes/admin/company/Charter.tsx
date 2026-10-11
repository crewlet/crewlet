/**
 * Company › Charter: the company's mission, its vision and the standing
 * policies every executor is given, and the way to change them.
 *
 * # Why the charter is the console's first Company section
 *
 * It is what the company has told every seat, verbatim, in every executor's
 * prompt — the first thing an admin configures and the first place a founder
 * looks for "what did we tell them". The drawing is the one a teammate reads
 * as Team › About (`components/Charter.tsx`); what the console adds is the way
 * to edit it, which is the org builder's, and the note that says when this
 * node is still applying a revision this tab saved.
 */

import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { Charter } from "~/components/Charter.tsx";
import { PreviousRevisionNote } from "~/components/PreviousRevisionNote.tsx";

export function CharterScreen() {
  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["admin", "company", "org"])}>
          Edit in org →
        </a>
      </PageActions>
      <PageNote>
        What the company has told every seat. Policies render into every executor&rsquo;s prompt in
        full.
      </PageNote>
      <PreviousRevisionNote />
      <Charter />
    </>
  );
}
