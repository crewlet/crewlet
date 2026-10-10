/**
 * Agents › Edit org: the builder, which edits the company's configuration.
 *
 * THE SURFACES ARE PASSED, NOT IMPORTED BY THE BUILDER: every dialog and both
 * views are injected so a suite can drive it with fakes. `builderSurfaces` is
 * the real set.
 *
 * A SECTION OF AGENTS AND A CHUNK OF ITS OWN. The builder is about a fifth of
 * the dashboard's source — more than any whole workspace — and it is the one
 * section a reader opens to write rather than to look, so shipping it inside
 * the Agents chunk would make every glance at the org chart pay for an editor
 * the reader did not open. `app/lazyScreen.ts` names it as the `org` chunk.
 */

import { Builder } from "./builder/Builder.tsx";
import { builderSurfaces } from "./builder/surfaces.ts";

export function OrgEdit() {
  return <Builder surfaces={builderSurfaces} />;
}
