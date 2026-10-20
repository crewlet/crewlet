/**
 * Composer's gallery: the shipped one, opening on the composer card, whose
 * open kind is in the address (`make=`), so Back closes it and a link opens
 * on it.
 */

import { useParam } from "~/app/router.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import { withMake } from "../../shared/gallery.tsx";
import { ComposerCard } from "./ComposerCard.tsx";

export { noMatch } from "@original/routes/artifacts/Gallery.tsx";

function ComposerMake() {
  const [make, setMake] = useParam("make", "");
  return <ComposerCard kind={isArtifactKind(make) ? make : ""} onKind={setMake} />;
}

export const ArtifactGallery = withMake(ComposerMake);
