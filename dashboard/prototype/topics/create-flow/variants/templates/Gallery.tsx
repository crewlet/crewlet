/**
 * Templates' gallery: the shipped one, opening on a shelf of starting points
 * drawn live, narrowed by kind (`start=`), each landing in its editor already
 * filled.
 */

import { ArtifactKindTile, SegmentedControl } from "@crewlethq/ui";
import { href, useParam } from "~/app/router.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import { KIND, MAKE_ORDER } from "../../shared/kinds.ts";
import { withMake } from "../../shared/gallery.tsx";
import { StartPreview, featured, startsOf, useExamples } from "./starts.tsx";

export { noMatch } from "@original/routes/artifacts/Gallery.tsx";

function Shelf() {
  const [start, setStart] = useParam("start", "");
  const kind = isArtifactKind(start) ? start : "";
  const examples = useExamples();
  const starts = kind ? startsOf(kind) : featured();
  return (
    <section className="cft-shelf" aria-labelledby="cft-title">
      <div className="cft-head">
        <h2 className="cf-section-title" id="cft-title">
          Start from
        </h2>
        <SegmentedControl
          label="Kind"
          semantics="radio"
          size="sm"
          value={kind || "all"}
          onValueChange={(v: string) => setStart(v === "all" ? "" : v)}
          options={[
            { value: "all", label: "Popular" },
            ...MAKE_ORDER.map((k) => ({ value: k, label: KIND[k].tile })),
          ]}
        />
      </div>
      <ul className="cft-row" role="list">
        {starts.map((s) => (
          <li key={`${s.kind}-${s.id}`}>
            <ArtifactKindTile
              appearance="preview"
              kind={s.kind}
              label={s.title}
              hint={kind ? s.detail : KIND[s.kind].tile}
              href={href(["artifacts", "new"], s.query)}
            >
              <StartPreview start={s} examples={examples} />
            </ArtifactKindTile>
          </li>
        ))}
      </ul>
    </section>
  );
}

export const ArtifactGallery = withMake(Shelf);
