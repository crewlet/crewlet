/**
 * The View tab: an artifact drawn as its kind is drawn. A page runs in a
 * sandboxed frame on the artifact origin; a document is rendered here, by the
 * dashboard's own element-constructing Markdown renderer, since it runs no
 * author code; a sheet is drawn natively from its schema.
 */

import type { PresenceMember } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactVersionSummary } from "~/protocol/index.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { FrameView } from "./FrameView.tsx";
import { MarkdownView } from "./MarkdownView.tsx";
import { SheetView } from "./SheetView.tsx";
import type { PresenceReport } from "./presence.ts";

export function ViewTab({
  detail,
  version,
  versions,
  store,
  room,
  presence,
  drawn,
  bridgeStores,
}: {
  detail: ArtifactDetail;
  version: number;
  versions: ArtifactVersionSummary[];
  store: ArtifactDataStore | null;
  room: ArtifactRoom;
  presence: PresenceReport;
  drawn: PresenceMember[];
  bridgeStores: Set<ArtifactDataStore>;
}) {
  switch (detail.kind) {
    case "html":
      return (
        <FrameView
          detail={detail}
          version={version}
          versions={versions}
          room={room}
          presence={presence}
          bridgeStores={bridgeStores}
        />
      );
    case "markdown":
      return <MarkdownView detail={detail} version={version} />;
    case "sheet":
      return store ? (
        <SheetView detail={detail} store={store} room={room} presence={presence} drawn={drawn} />
      ) : null;
  }
}
