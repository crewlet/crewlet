/**
 * What the Artifacts screens share: copying an artifact's address, a listed
 * artifact as the design system's card and who can open it, the card an
 * artifact's place shows when the reader may not open it, and the state a
 * refusal maps to.
 */

import type { ReactNode } from "react";
import {
  AccessDeniedCard,
  ArtifactCard,
  ButtonLink,
  type AccessDeniedReason,
  type ArtifactAccess,
  type ArtifactCardProps,
  type ArtifactCardSize,
  type ToastApi,
} from "@crewlethq/ui";
import { href } from "~/app/router.tsx";
import { queryErrorCode, type ArtifactListItem } from "~/protocol/index.ts";
import { useViewer } from "~/lib/viewer.ts";
import { artifactPath } from "~/app/routes.ts";
import type { ArtifactPeople } from "~/lib/artifacts/people.ts";
import { GalleryPreview } from "./GalleryPreview.tsx";

/**
 * Who can open an artifact, as its card draws it: the whole company when a
 * company share is among the shares this reader is shown, people when it
 * carries any share at all, else its owner alone. A reader below editor is
 * shown no share list, only the count, so a company-wide artifact reads as
 * shared to them, which is true.
 */
export function accessOf(item: Pick<ArtifactListItem, "shares" | "share_count">): ArtifactAccess {
  if (item.shares?.some((s) => s.audience.kind === "company")) return "company";
  return item.share_count > 0 ? "shared" : "private";
}

/**
 * A listed artifact as the design system's card: a gallery's card, with its
 * live preview and its own menu, or a row in a task's rail (`compact`). One
 * mapping, so an artifact reads the same wherever it is listed.
 *
 * WHO, ONCE. The owner is named unless it is the reader, whose own card says
 * nothing it would repeat on every one of theirs; the change is "by" somebody
 * only when that is not the owner already named: "by you" for the reader's
 * own change to somebody else's.
 */
export function ListedArtifactCard({
  item,
  people,
  now,
  size = "card",
  actions,
  selection,
}: {
  item: ArtifactListItem;
  people: ArtifactPeople;
  now: number;
  size?: ArtifactCardSize;
  /** The card's own menu, drawn beside its link. */
  actions?: ReactNode;
  /**
   * The card can be picked for a bulk action: whether the gallery is
   * choosing, whether this card is picked, and what a press of its box does.
   * A gallery's card only.
   */
  selection?:
    | {
        selecting: boolean;
        selected: boolean;
        onSelectedChange: NonNullable<ArtifactCardProps["onSelectedChange"]>;
      }
    | undefined;
}) {
  const reader = useViewer().owner;
  const mine = reader !== "" && item.owner === reader;
  // A CARD IN THE TRASH says when it went in, which is what its owner reads
  // a trash by, not when it was last edited.
  const trashed = item.status === "trashed";
  const changer = trashed ? "" : item.updated_by.name;
  const updatedBy =
    !changer || changer === item.owner
      ? undefined
      : changer === reader
        ? "you"
        : people.seatName(changer);
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
      size={size}
      now={now}
      {...(size === "card"
        ? {
            access: accessOf(item),
            trashed,
            preview: <GalleryPreview item={item} people={people} />,
            actions,
            ...(selection
              ? {
                  selectable: true,
                  selecting: selection.selecting,
                  selected: selection.selected,
                  onSelectedChange: selection.onSelectedChange,
                }
              : {}),
          }
        : {})}
    />
  );
}

/** A dashboard address, absolute, as a person pastes it into chat. */
function pageAddress(path: string[], query?: Record<string, string>): string {
  return `${location.origin}${location.pathname}${href(path, query)}`;
}

/**
 * An artifact's dashboard address, absolute, as a person pastes it into chat:
 * at one of its older versions where `version` names one.
 */
export function artifactAddress(id: string, version?: number): string {
  return pageAddress(artifactPath(id), version ? { version: String(version) } : undefined);
}

/** Copies an artifact's address, and says whether the browser let it. */
export async function copyAddress(id: string, toast: ToastApi): Promise<void> {
  return copyPageAddress(artifactPath(id), toast);
}

/** Copies a page's address (an artifact's, a folder's), and says whether the browser let it. */
export async function copyPageAddress(path: string[], toast: ToastApi): Promise<void> {
  try {
    if (!navigator.clipboard) throw new Error("no clipboard");
    await navigator.clipboard.writeText(pageAddress(path));
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
