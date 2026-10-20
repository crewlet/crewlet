/**
 * The gallery every proposal keeps: the shipped one, with the proposal's own
 * Make section in place of the shipped row and its cards a step smaller.
 *
 * THE SHIPPED GALLERY IS WRAPPED, NOT COPIED: its Make row is hidden by the
 * proposals' shared stylesheet and the proposal's own section is drawn where
 * it stood, under the same rule for when it shows (the reader's own sections,
 * no search, no type, not the trash, and only for a reader who may start
 * one).
 */

import type { ComponentType, ReactNode } from "react";
import { ArtifactCard, type ArtifactCardSize } from "@crewlethq/ui";
import { ArtifactGallery as Shipped } from "@original/routes/artifacts/Gallery.tsx";
import { accessOf } from "@original/routes/artifacts/parts.tsx";
import { GalleryPreview } from "~/routes/artifacts/GalleryPreview.tsx";
import { href, useParam } from "~/app/router.tsx";
import { artifactPath, type ArtifactSection } from "~/app/routes.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { ArtifactListItem } from "~/protocol/index.ts";

/** Whether the Make section shows, by the shipped gallery's own rule. */
export function useMakeShown(section: ArtifactSection): boolean {
  const [q] = useParam("q", "");
  const [kind] = useParam("kind", "");
  const [status] = useParam("status", "");
  const author = useArtifactAccess({ app: true });
  const trashed = section === "yours" && status === "trashed";
  return section !== "shared" && !trashed && q.trim() === "" && kind === "" && author.can;
}

/** The shipped gallery with `Make` drawn where its Make row was. */
export function withMake(Make: ComponentType<{ section: ArtifactSection }>) {
  return function ArtifactGallery({ section }: { section: ArtifactSection }) {
    const shown = useMakeShown(section);
    return (
      <div className="cf-gallery">
        {shown && <Make section={section} />}
        <Shipped section={section} />
      </div>
    );
  };
}

/**
 * The shipped card mapping, with the gallery's cards drawn as `tile` (a step
 * smaller), each still badged with its kind's glyph in its colour. A task
 * rail's `compact` row is left as it is.
 */
export function tileCards() {
  return function ListedArtifactCard({
    item,
    people,
    now,
    size = "card",
    actions,
  }: {
    item: ArtifactListItem;
    people: ArtifactPeople;
    now: number;
    size?: ArtifactCardSize;
    actions?: ReactNode;
  }) {
    const reader = useViewer().owner;
    const mine = reader !== "" && item.owner === reader;
    const trashed = item.status === "trashed";
    const changer = trashed ? "" : item.updated_by.name;
    const updatedBy =
      !changer || changer === item.owner
        ? undefined
        : changer === reader
          ? "you"
          : people.seatName(changer);
    const drawn: ArtifactCardSize = size === "compact" ? "compact" : "tile";
    return (
      <ArtifactCard
        title={item.title}
        kind={item.kind}
        href={href(artifactPath(item.id))}
        owner={mine ? undefined : people.seatPrincipal(item.owner)}
        updatedAt={trashed ? item.trashed_at : item.updated_at}
        updatedBy={updatedBy}
        {...(trashed ? { labels: { updated: "Trashed" } } : {})}
        pinned={item.pinned ?? false}
        version={item.head_number > 0 ? item.head_number : undefined}
        size={drawn}
        now={now}
        {...(drawn === "tile"
          ? {
              access: accessOf(item),
              trashed,
              preview: <GalleryPreview item={item} people={people} />,
              actions,
            }
          : {})}
      />
    );
  };
}
