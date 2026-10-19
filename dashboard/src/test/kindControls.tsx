/**
 * A kind's view with the page bar its controls are published to, for a suite
 * that presses them: the deck's Edit, Grid and Present, the doc's Edit. The
 * real `ArtifactBar` draws what the view publishes
 * (`lib/artifacts/kindControls.ts`), so a suite presses the controls a reader
 * presses.
 */

import { useState, type ReactNode } from "react";
import { ArtifactBar, EditingStatus } from "@crewlethq/ui";
import { KindControlsSink, type KindControls } from "~/lib/artifacts/kindControls.ts";

export function WithKindBar({ children }: { children: ReactNode }) {
  const [controls, setControls] = useState<KindControls | null>(null);
  return (
    <KindControlsSink.Provider value={setControls}>
      <ArtifactBar
        as="div"
        title="Artifact"
        kind="slides"
        kindLabel="Artifact"
        {...(controls?.modes ? { modes: controls.modes } : {})}
        {...(controls?.editing
          ? {
              status: (
                <EditingStatus dirty={controls.editing.dirty} saving={controls.editing.saving} />
              ),
            }
          : {})}
        kindTools={controls?.tools ?? []}
        menu={controls?.menu ?? []}
      />
      {children}
    </KindControlsSink.Provider>
  );
}
