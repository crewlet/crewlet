/**
 * Composer's New: the same card as the gallery's, on a page of its own,
 * open on the kind New was given (a doc when none).
 */

import { useParam } from "~/app/router.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";
import { ComposerCard } from "./ComposerCard.tsx";

export function NewArtifact() {
  const [kind, setKind] = useParam("kind", "doc");
  return (
    <ComposerCard
      kind={isArtifactKind(kind) ? kind : "doc"}
      onKind={(k) => setKind(k || "doc")}
      standalone
    />
  );
}
