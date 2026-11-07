/**
 * How a Chatlet list says it is still being read, or could not be: a room's
 * transcript, a thread and a search's results all draw these two, so a reader
 * meets one loading shape and one failure with one way on.
 *
 * Both TAKE THE LIST'S PLACE, so the box under it stays where it is when the
 * answer lands or fails: a room's composer never jumps up the screen.
 */

import { Button, Skeleton, type SkeletonVariant } from "@crewlethq/ui";
import { RefreshCwGlyph } from "@crewlethq/icons/glyphs";

import { QueryState } from "~/components/common.tsx";
import { queryErrorCode } from "~/protocol/index.ts";
import type { QueryResult } from "~/lib/useQuery.ts";

/**
 * A list on its way, held back so a quick answer never flashes a
 * placeholder. `chat` is a transcript's rows; `list` is a search's hits.
 */
export function ReadPending({
  label,
  rows,
  variant = "chat",
}: {
  label: string;
  rows: number;
  variant?: Extract<SkeletonVariant, "chat" | "list">;
}) {
  return <Skeleton variant={variant} rows={rows} label={label} held />;
}

/**
 * Whether a failure is the reader's to ask again. The engine's own outages
 * (`unavailable`) and a dropped socket (`closed`) are asked again by the
 * screen; a refusal (`unauthorized`, `not_found`, `bad_params`) answers the
 * same however often it is asked. What is left failed once and says so.
 */
export function askedAgainByHand(error: string): boolean {
  const code = queryErrorCode(error);
  return code === null || code === "query_failed" || code === "timeout";
}

/** A list that could not be read: the engine's sentence, and Try again where it helps. */
export function ReadFailed({
  query,
}: {
  query: Pick<QueryResult<unknown>, "error" | "refusal" | "detail" | "loading" | "refetch">;
}) {
  if (!query.error) return null;
  return (
    <div className="chat-read-failed">
      <div className="chat-read-failed-reason">
        <QueryState
          error={query.error}
          refusal={query.refusal}
          detail={query.detail}
          loading={false}
        />
      </div>
      {askedAgainByHand(query.error) && (
        <Button
          variant="secondary"
          size="small"
          leadingIcon={<RefreshCwGlyph size="sm" />}
          loading={query.loading}
          onClick={query.refetch}
        >
          Try again
        </Button>
      )}
    </div>
  );
}
