/**
 * An artifact's details, in its side panel: what it says about itself, then
 * the facts its page header used to stack over it (who owns it, when it last
 * changed and by whom, the version, the reader's role) and the few a reader
 * asks after once (when it was made, how many it is shared with, its id).
 *
 * ONE READING OF THE HEAD, so nothing here can disagree with the bar: the
 * version is the one the bar's chip names, the title the bar's heading. A
 * sheet's version names nothing a reader sees change (its content is its live
 * data), so a sheet is not given one, as its header never was.
 */

import { Button, Copyable, useToast } from "@crewlethq/ui";
import { CopyGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useNow } from "~/lib/clock.ts";
import { fmtDate, relTime } from "~/lib/format.ts";
import { useArtifactPeople } from "~/lib/artifacts/people.ts";
import { KIND_WORDS, ROLE_WORDS } from "~/lib/artifacts/words.ts";
import type { ArtifactDetail } from "~/protocol/index.ts";
import { copyAddress } from "./parts.tsx";

export function DetailsPanel({
  detail,
  showing,
}: {
  detail: ArtifactDetail;
  /** The version on show, which may be older than the head. */
  showing: number;
}) {
  const people = useArtifactPeople();
  const now = useNow();
  const toast = useToast();
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
        <dt>Owner</dt>
        <dd>
          <a className="prose-link" href={href(["agents", "seats", detail.owner])}>
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
        <dt>Your role</dt>
        <dd>{detail.elevated ? "Administrator" : ROLE_WORDS[detail.role]}</dd>
        <dt>Shares</dt>
        <dd>{detail.share_count === 0 ? "None yet" : detail.share_count.toLocaleString()}</dd>
        <dt>ID</dt>
        <dd>
          <Copyable value={detail.id} ariaLabel="Copy the artifact's id" monospace truncate />
        </dd>
      </dl>
      <div className="artifact-form-actions">
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
