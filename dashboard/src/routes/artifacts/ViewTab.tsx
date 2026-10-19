/**
 * The View tab: an artifact drawn as its kind is drawn, from the dashboard's
 * half of the kind table ([KIND_VIEWS]). A framed kind runs in a sandboxed
 * frame on the artifact origin; a native kind is drawn here from its closed
 * format (a document by the element-constructing Markdown renderer, a sheet
 * from its schema), since it runs no author code.
 */

import type { ReactNode } from "react";
import type { PresenceMember } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactKind, ArtifactVersionSummary } from "~/protocol/index.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { FrameView } from "./FrameView.tsx";
import { DocView } from "./DocView.tsx";
import { SheetView } from "./SheetView.tsx";
import type { PresenceReport } from "./presence.ts";

/** Everything a kind's view may draw from; each kind takes what it needs. */
export interface KindViewProps {
  detail: ArtifactDetail;
  version: number;
  versions: ArtifactVersionSummary[];
  /** The artifact's datastore, null where it keeps none or it is not open. */
  store: ArtifactDataStore | null;
  room: ArtifactRoom;
  presence: PresenceReport;
  drawn: PresenceMember[];
  bridgeStores: Set<ArtifactDataStore>;
}

/** How each kind draws one version: a record over every kind, so a new kind fails the build until it is drawn. */
export const KIND_VIEWS: Readonly<Record<ArtifactKind, (props: KindViewProps) => ReactNode>> = {
  html: ({ detail, version, versions, room, presence, bridgeStores }) => (
    <FrameView
      detail={detail}
      version={version}
      versions={versions}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores}
    />
  ),
  doc: ({ detail, version }) => <DocView detail={detail} version={version} />,
  sheet: ({ detail, store, room, presence, drawn }) =>
    store ? (
      <SheetView detail={detail} store={store} room={room} presence={presence} drawn={drawn} />
    ) : null,
};

export function ViewTab(props: KindViewProps) {
  return KIND_VIEWS[props.detail.kind](props);
}
