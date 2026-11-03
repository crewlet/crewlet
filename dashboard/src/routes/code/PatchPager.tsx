/**
 * Load more of a file's patch while git wrote more of it than the pages read
 * hold (`lib/patchPages.ts`) — and why the last page asked for did not arrive,
 * beside the press that asks for it again (`PageFailed`). A commit's diff and
 * a comparison draw it (`commit/DiffFiles.tsx`), and so does a change's
 * (`change/Files.tsx`).
 */

import { Button } from "@crewlethq/ui";
import { PageFailed } from "~/components/common.tsx";
import type { Paged } from "~/lib/usePaged.ts";

export function PatchPager({
  paged,
}: {
  paged: Pick<Paged<unknown, unknown>, "more" | "next" | "paging" | "pageError">;
}) {
  if (!paged.more && !paged.pageError) return null;
  return (
    <div className="row gap-2 wrap">
      {paged.more && (
        <Button
          size="small"
          variant="secondary"
          onClick={paged.next}
          disabled={paged.paging}
          loading={paged.paging}
        >
          Load more lines
        </Button>
      )}
      {paged.pageError && <PageFailed error={paged.pageError} />}
    </div>
  );
}
