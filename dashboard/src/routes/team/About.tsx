/**
 * Team › About: what the company is for — its mission, its vision and the
 * policies every agent works under — for everybody who works here.
 *
 * The same drawing the console's Company › Charter shows
 * (`components/Charter.tsx`), without the way to edit it: the charter is the
 * company document's, and the document is changed in the console. An admin
 * crosses there through the frame's doors (`g .`), which keep their place.
 */

import { Charter } from "~/components/Charter.tsx";
import { PreviousRevisionNote } from "~/components/PreviousRevisionNote.tsx";

export function About() {
  return (
    <>
      <PreviousRevisionNote />
      <Charter />
    </>
  );
}
