/**
 * Studio's gallery: the shipped one, with a Make row of stage tiles, each
 * kind's maker character presenting its kind on the org chart's dotted
 * canvas, a line under the name saying who makes it.
 */

import { ArtifactKindTile } from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { KIND, MAKE_ORDER } from "../../shared/kinds.ts";
import { withMake } from "../../shared/gallery.tsx";

export { noMatch } from "@original/routes/artifacts/Gallery.tsx";

function StudioMake() {
  return (
    <section className="cfs-make" aria-labelledby="cfs-make-title">
      <div className="cfs-make-head">
        <h2 className="cf-section-title" id="cfs-make-title">
          Make something new
        </h2>
        <p className="cf-section-hint">
          Write a doc, a deck or a sheet yourself, or ask an agent to build the rest.
        </p>
      </div>
      <ul className="cfs-make-row" role="list">
        {MAKE_ORDER.map((k) => (
          <li key={k}>
            <ArtifactKindTile
              appearance="stage"
              kind={k}
              label={KIND[k].tile}
              hint={KIND[k].hint}
              href={href(["artifacts", "new"], { kind: k })}
            />
          </li>
        ))}
      </ul>
    </section>
  );
}

export const ArtifactGallery = withMake(StudioMake);
