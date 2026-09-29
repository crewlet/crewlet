/**
 * "of input served from prompt cache" — `cache_read / input` over the window.
 *
 * THE ENGINE'S CONTRACT, divided and not re-derived: the input count already
 * includes the cached prefix (`tokens.Bucket`), so the share is a ratio of two
 * figures the answer carries, never a sum. ABSENT when nothing in the window
 * reported a cache — a backend that never states one leaves both counts at
 * zero, and "0%" over a provider that does not say is a claim about caching
 * nobody made. See `model.ts`' `cacheShare`.
 */

import { Skeleton } from "@crewlethq/ui";
import { cacheShare } from "./model.ts";
import { fmtExact } from "~/lib/format.ts";
import type { Bucket } from "~/protocol/types.ts";

export function CacheTile({
  totals,
  loading,
  words,
}: {
  totals: Bucket | null;
  loading: boolean;
  words: string | null;
}) {
  const share = cacheShare(totals);
  // ABSENT, not "None reported" and not 0%: a figure nobody stated has no
  // tile. The tile holds its place only while the window is still loading.
  if (share === null && !(loading && !totals)) return null;
  return (
    <div className="spend-tile">
      {share !== null ? (
        <span
          className="spend-tile-figure"
          title={`${fmtExact(totals?.cache_read_tokens ?? 0)} of ${fmtExact(totals?.input_tokens ?? 0)} input tokens${words ? `, ${words}` : ""}`}
        >
          {Math.round(share * 100)}%
        </span>
      ) : (
        <Skeleton width="3rem" height="1.25rem" />
      )}
      <span className="spend-label">of input served from prompt cache</span>
    </div>
  );
}
