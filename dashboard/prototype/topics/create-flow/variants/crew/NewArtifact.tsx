/**
 * Crew's New: there is no page of its own. New opens the gallery with the
 * kind's panel already open (a doc when none was given), so every way in
 * lands on the same panel.
 */

import { useEffect } from "react";
import { useNavigator, useParam } from "~/app/router.tsx";
import { isArtifactKind } from "~/lib/artifacts/words.ts";

export function NewArtifact() {
  const nav = useNavigator();
  const [kind] = useParam("kind", "doc");
  useEffect(() => {
    nav.replace(["artifacts"], { make: isArtifactKind(kind) ? kind : "doc" });
  }, [nav, kind]);
  return null;
}
