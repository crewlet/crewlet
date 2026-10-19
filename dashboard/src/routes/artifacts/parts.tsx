/**
 * What the Artifacts screens share: copying an artifact's address, a listed
 * artifact as the design system's card, the card an artifact's place shows
 * when the reader may not open it, and the state a refusal maps to.
 */

import type { ReactNode } from "react";
import {
  AccessDeniedCard,
  ArtifactCard,
  ButtonLink,
  type AccessDeniedReason,
  type ArtifactCardSize,
  type ToastApi,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { queryErrorCode, type ArtifactKind, type ArtifactListItem } from "~/protocol/index.ts";
import { artifactPath } from "~/lib/artifacts/api.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { DashboardCardCover } from "./DashboardCardCover.tsx";
import { SlidesCover } from "./SlidesCover.tsx";

/**
 * A listed artifact as the design system's card: a gallery's card, or a row
 * in a task's rail (`compact`). One mapping, so an artifact reads the same
 * wherever it is listed.
 */
/**
 * The native cover each kind draws on its gallery card (D64), from the kind's
 * own structure and never a screenshot; a kind with none shows its glyph.
 */
const KIND_COVERS: Partial<Record<ArtifactKind, (item: ArtifactListItem) => ReactNode>> = {
  slides: (item) => <SlidesCover item={item} />,
  dashboard: (item) => <DashboardCardCover item={item} />,
};

export function ListedArtifactCard({
  item,
  people,
  now,
  size = "card",
}: {
  item: ArtifactListItem;
  people: ArtifactPeople;
  now: number;
  size?: ArtifactCardSize;
}) {
  return (
    <ArtifactCard
      title={item.title}
      kind={item.kind}
      href={href(artifactPath(item.id))}
      owner={people.seatPrincipal(item.owner)}
      updatedAt={item.updated_at}
      updatedBy={people.seatName(item.updated_by.name)}
      pinned={item.pinned ?? false}
      version={item.head_number > 0 ? item.head_number : undefined}
      size={size}
      now={now}
      {...(size !== "compact" && KIND_COVERS[item.kind]
        ? { preview: KIND_COVERS[item.kind]!(item) }
        : {})}
    />
  );
}

/** An artifact's dashboard address, absolute, as a person pastes it into chat. */
export function artifactAddress(id: string): string {
  return `${location.origin}${location.pathname}${href(artifactPath(id))}`;
}

/** Copies an artifact's address, and says whether the browser let it. */
export async function copyAddress(id: string, toast: ToastApi): Promise<void> {
  try {
    if (!navigator.clipboard) throw new Error("no clipboard");
    await navigator.clipboard.writeText(artifactAddress(id));
    toast.ok("Link copied");
  } catch {
    toast.show({
      variant: "warning",
      title: "The browser refused the clipboard",
      message: "Copy the address from the address bar instead.",
    });
  }
}

/**
 * Which card a refused read of an artifact shows. `not_found` is what the
 * engine answers for an artifact the reader holds no relation to as much as
 * for one that does not exist, and the card says both together, so a link
 * never tells anybody an artifact is there.
 */
export function deniedReason(code: string | null): AccessDeniedReason | null {
  switch (queryErrorCode(code)) {
    case "not_found":
      return "not-found";
    case "unauthorized":
      return "no-access";
    default:
      return null;
  }
}

/** The card an artifact's place shows when the reader may not open it, with the way back. */
export function Denied({
  reason,
  owner,
  action,
}: {
  reason: AccessDeniedReason;
  owner?: string;
  action?: ReactNode;
}) {
  return (
    <div className="artifacts-denied">
      <AccessDeniedCard
        reason={reason}
        owner={owner}
        action={
          action ?? (
            <ButtonLink size="small" variant="secondary" href={href(["artifacts"])}>
              Back to artifacts
            </ButtonLink>
          )
        }
      />
    </div>
  );
}
