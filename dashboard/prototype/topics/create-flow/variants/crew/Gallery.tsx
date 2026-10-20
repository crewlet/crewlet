/**
 * Crew's gallery: the shipped one, opening on the crew, six characters on
 * the dotted ground, each opening its kind's panel (`make=`, so Back closes
 * it and a link opens on it).
 */

import { ArtifactKindTile, DottedGround } from "@crewlethq/ui";
import { useParam } from "~/app/router.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import { KIND, MAKE_ORDER } from "../../shared/kinds.ts";
import { withMake } from "../../shared/gallery.tsx";
import { CrewPanel } from "./CrewPanel.tsx";

export { noMatch } from "@original/routes/artifacts/Gallery.tsx";

function CrewMake() {
  const [make, setMake] = useParam("make", "");
  const kind = isArtifactKind(make) ? make : "";
  return (
    <>
      <section aria-labelledby="cfr-title">
        <DottedGround shape="inset" className="cfr-band">
          <div className="cfr-intro">
            <h2 className="cf-section-title" id="cfr-title">
              Make something new
            </h2>
            <p className="cf-section-hint">
              You write docs, decks and sheets. Designs, dashboards and pages are built by one of
              your agents.
            </p>
          </div>
          <ul className="cfr-crew" role="list">
            {MAKE_ORDER.map((k) => (
              <li key={k}>
                <ArtifactKindTile
                  appearance="figure"
                  kind={k}
                  label={KIND[k].tile}
                  hint={KIND[k].hint}
                  selected={k === kind}
                  onSelect={() => setMake(k)}
                />
              </li>
            ))}
          </ul>
        </DottedGround>
      </section>
      <CrewPanel kind={kind} onClose={() => setMake("")} />
    </>
  );
}

export const ArtifactGallery = withMake(CrewMake);
