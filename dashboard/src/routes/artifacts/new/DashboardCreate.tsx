/**
 * A new dashboard: its own create page, in the shared frame (`CreateShell`).
 * The body is the dashboard kind's to draw; the writes it needs are in `create.ts`.
 */

import { ArtifactKindGlyph, EmptyState } from "@crewlethq/ui";
import { CreateShell } from "./CreateShell.tsx";

export function DashboardCreate() {
  return (
    <CreateShell kind="dashboard">
      <div className="artifact-create-pending">
        <EmptyState
          icon={<ArtifactKindGlyph kind="dashboard" size="lg" />}
          title="This page is on its way"
          description="Starting a dashboard here is not ready yet. Go back to the gallery to open one."
        />
      </div>
    </CreateShell>
  );
}
