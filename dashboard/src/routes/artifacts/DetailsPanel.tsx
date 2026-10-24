/**
 * An artifact's details, in its side panel: what it says about itself, then
 * the facts its page header used to stack over it (who owns it, when it last
 * changed and by whom, the version, the reader's role) and the few a reader
 * asks after once (when it was made, how many it is shared with, its id),
 * and whether it follows the company's brand.
 *
 * ONE READING OF THE HEAD, so nothing here can disagree with the bar: the
 * version is the one the bar's chip names, the title the bar's heading. A
 * sheet's version names nothing a reader sees change (its content is its live
 * data), so a sheet is not given one, as its header never was.
 */

import { useState } from "react";
import { Button, Copyable, useToast } from "@crewlethq/ui";
import { CopyGlyph, FolderInputGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { artifactFolderPath } from "~/app/routes.ts";
import { useArtifactAccess } from "~/lib/artifacts/access.ts";
import type { ArtifactPlace } from "~/lib/artifacts/folders.ts";
import { useNow } from "~/lib/clock.ts";
import { fmtDate, relTime } from "~/lib/format.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { KIND_WORDS, ROLE_WORDS } from "~/lib/artifacts/words.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { BrandFact } from "~/components/brand.tsx";
import { copyAddress } from "./parts.tsx";
import { MoveDialog } from "./MoveDialog.tsx";

export function DetailsPanel({
  detail,
  showing,
  place,
  onChanged,
}: {
  detail: ArtifactDetail;
  /** The version on show, which may be older than the head. */
  showing: number;
  /** Where it is kept, as the reader's tree says it. */
  place: ArtifactPlace;
  /** It moved: the page reads its head again. */
  onChanged: () => void;
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const toast = useToast();
  const file = useArtifactAccess({ app: true, role: "editor", held: detail.role });
  const [moving, setMoving] = useState(false);
  const updater = people.seatName(detail.updated_by.name);
  const creator = people.seatName(detail.created_by.name);
  const versioned = detail.head_number > 0 && detail.kind !== "sheet";
  return (
    <div className="artifact-details">
      {detail.description ? (
        <p className="artifact-details-description">{detail.description}</p>
      ) : null}
      <dl className="artifact-facts">
        <dt>Kind</dt>
        <dd>{KIND_WORDS[detail.kind]}</dd>
        {/* WHERE IT IS KEPT, a link to the folder's page; the engine names
            no folder the reader cannot see, which reads as not in a folder.
            Move… is held with its reason, never hidden. */}
        <dt>Folder</dt>
        <dd className="artifact-folder-fact">
          {place.at === "folder" ? (
            <a className="prose-link" href={href(artifactFolderPath(place.id))}>
              {place.path}
            </a>
          ) : place.at === "reading" ? (
            "Reading…"
          ) : (
            "Not in a folder"
          )}
          {detail.status === "live" && (
            <Button
              size="small"
              variant="ghost"
              leadingIcon={<FolderInputGlyph size="sm" />}
              disabledReason={file.can ? undefined : file.reason}
              onClick={() => setMoving(true)}
            >
              Move…
            </Button>
          )}
        </dd>
        <dt>Owner</dt>
        <dd>
          <a className="prose-link" href={href(["company", "org", "seats", detail.owner])}>
            {people.seatName(detail.owner)}
          </a>
        </dd>
        <dt>Updated</dt>
        <dd>
          <time dateTime={detail.updated_at} title={fmtDate(detail.updated_at)}>
            {relTime(detail.updated_at, now)}
          </time>{" "}
          by {updater}
        </dd>
        <dt>Created</dt>
        <dd>
          <time dateTime={detail.created_at} title={fmtDate(detail.created_at)}>
            {fmtDate(detail.created_at)}
          </time>{" "}
          by {creator}
        </dd>
        {versioned && (
          <>
            <dt>Version</dt>
            <dd>
              {showing === detail.head_number
                ? `${detail.head_number}, the latest`
                : `${showing} of ${detail.head_number}`}
            </dd>
          </>
        )}
        <BrandFact id={detail.id} kind={detail.kind} version={showing} made={detail.created_at} />
        <dt>Your role</dt>
        <dd>{detail.elevated ? "Administrator" : ROLE_WORDS[detail.role]}</dd>
        <dt>Shares</dt>
        <dd>{detail.share_count === 0 ? "None yet" : detail.share_count.toLocaleString()}</dd>
        <dt>ID</dt>
        <dd>
          <Copyable value={detail.id} ariaLabel="Copy the artifact's id" monospace truncate />
        </dd>
      </dl>
      {moving && (
        <MoveDialog
          subject={{ kind: "artifacts", items: [detail] }}
          onClose={() => setMoving(false)}
          onMoved={() => {
            setMoving(false);
            onChanged();
          }}
        />
      )}
      <div className="artifact-details-actions">
        <Button
          size="small"
          variant="secondary"
          leadingIcon={<CopyGlyph size="sm" />}
          onClick={() => void copyAddress(detail.id, toast)}
        >
          Copy link
        </Button>
      </div>
    </div>
  );
}
