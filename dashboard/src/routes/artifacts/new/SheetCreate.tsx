/**
 * A new sheet: its own create page, in the shared frame (`CreateShell`).
 * The body is the sheet kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function SheetCreate() {
  return (
    <CreateShell kind="sheet">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="sheet" size="lg" />}
          title="This page is on its way"
          description="Starting a sheet here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
