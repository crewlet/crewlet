/**
 * The artifact's view: an artifact drawn as its kind is drawn, from the dashboard's
 * half of the kind table ([KIND_VIEWS]). A framed kind runs in a sandboxed
 * frame on the artifact origin (a page in one, a design's artboards in one
 * each, on a canvas drawn here); a native kind is drawn here from its closed
 * format (a doc by the element-constructing Markdown renderer, a deck by
 * the slide renderer, a dashboard's widgets by the design system's figures
 * over datasets read as the reader, a sheet from its schema), since it runs no
 * author code.
 */

import type { ReactNode } from "react";
import type { PresenceMember } from "@crewlethq/ui";
import type { ArtifactDetail, ArtifactKind, ArtifactVersionSummary } from "~/protocol/index.ts";
import type { ArtifactRoom } from "~/lib/artifacts/room.ts";
import type { ArtifactDataStore } from "~/lib/artifacts/datastore.ts";
import { DashboardView } from "./DashboardView.tsx";
import { FrameView } from "./FrameView.tsx";
import { DesignView } from "./DesignView.tsx";
import { DocView } from "./DocView.tsx";
import { SheetView } from "./SheetView.tsx";
import { SlidesView } from "./SlidesView.tsx";
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
  /**
   * A version this reader published from the view landed: show it. Absent
   * for a secondary drawing (a side of a compare), which is read only and
   * says nothing to the room.
   */
  onPublished?: ((version: number) => void) | undefined;
  /** Whether a framed version says its data is today's; false where a comparison says it once for both sides. */
  dataNote?: boolean;
}

/** How each kind draws one version: a record over every kind, so a new kind fails the build until it is drawn. */
export const KIND_VIEWS: Readonly<Record<ArtifactKind, (props: KindViewProps) => ReactNode>> = {
  html: ({ detail, version, versions, room, presence, bridgeStores, dataNote }) => (
    <FrameView
      detail={detail}
      version={version}
      versions={versions}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores}
      {...(dataNote === undefined ? {} : { dataNote })}
    />
  ),
  doc: ({ detail, version, presence, onPublished }) => (
    <DocView
      detail={detail}
      version={version}
      presence={presence}
      {...(onPublished ? { onPublished } : {})}
    />
  ),
  slides: ({ detail, version, presence, onPublished }) => (
    <SlidesView
      detail={detail}
      version={version}
      presence={presence}
      {...(onPublished ? { onPublished } : {})}
    />
  ),
  dashboard: ({ detail, version, store, presence }) => (
    <DashboardView detail={detail} version={version} store={store} presence={presence} />
  ),
  sheet: ({ detail, store, room, presence, drawn }) =>
    store ? (
      <SheetView detail={detail} store={store} room={room} presence={presence} drawn={drawn} />
    ) : null,
  design: ({ detail, version, versions, room, presence, bridgeStores, onPublished }) => (
    <DesignView
      detail={detail}
      version={version}
      versions={versions}
      room={room}
      presence={presence}
      bridgeStores={bridgeStores}
      {...(onPublished ? { onPublished } : {})}
    />
  ),
};

export function ViewTab(props: KindViewProps) {
  return KIND_VIEWS[props.detail.kind](props);
}
