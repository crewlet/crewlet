/**
 * Which key table a node's API admits, against the table of the node serving
 * this dashboard — the words Engine › Nodes and Company › People & access
 * both draw it in.
 *
 * Tier A is per node, so every node serving the API judges a request by its
 * own `api.auth`, and two that disagree are a way to reach more through one of
 * them: a key that is a member's on one node and an admin's on the next. Each
 * node publishes a keyed fingerprint of its table; the engine compares every
 * live node's against the answering node's and says `same`, `different` or
 * `incomparable`. The fleet-wide verdict is the `auth_diverged` alarm's, which
 * appears where every alarm does.
 *
 * ONE TABLE OF WORDS for both screens, exhaustive over the contract's union, so
 * an agreement the engine adds fails the typecheck here rather than drawing as
 * nothing — and so the two screens cannot describe one comparison two ways.
 */

import { EmptyValue, Tag } from "@crewlethq/ui";
import type { AuthAgreement, TableDigest } from "~/contract/access.ts";
import type { NodeAuth } from "~/contract/fleet.ts";

export const AGREEMENT_WORDS: Record<
  AuthAgreement,
  { label: string; tone: "success" | "danger" | "neutral"; hint: string }
> = {
  same: {
    label: "Same keys",
    tone: "success",
    hint: "Its API admits the same keys as the node serving this dashboard, each with the same role and value, and the same anonymous posture.",
  },
  different: {
    label: "Different keys",
    tone: "danger",
    hint: "Its API judges keys by a different table from the node serving this dashboard, so a key can reach more through one of them. Give every node that serves the API the same api.auth and restart the ones that differ — mid-way through a rolling key rotation this is expected until the last node restarts.",
  },
  incomparable: {
    label: "Cannot compare",
    tone: "neutral",
    hint: "The two nodes share no keyring key — usually one of them runs with no keyring at all — so nothing can show whether they admit the same keys.",
  },
};

/**
 * How many hex digits of a digest a reader is shown: enough to tell two
 * tables apart by eye, as a short commit id is, with the whole digest in the
 * title.
 */
const SHORT_DIGEST = 12;

/** A node's digests as one line, `k1 3f9a1c2b0d4e`, for a title or a caption. */
export function digestLine(digests: TableDigest[]): string {
  return digests.map((d) => `${d.key_id} ${d.digest.slice(0, SHORT_DIGEST)}`).join(" · ");
}

/**
 * One node's key table as a tag: how it stands against this dashboard's node,
 * with the digests it published in the title.
 *
 * ABSENT IS A NODE SERVING NO API, which admits nobody by a key and is compared
 * with nothing — worded, never a blank. A node serving the API with NO KEYRING
 * publishes no digest and is said to be that, rather than "cannot compare",
 * because its remedy is its own: a keyring, not a restart.
 */
export function KeyTableTag({ auth, here }: { auth?: NodeAuth; here?: boolean }) {
  if (!auth) {
    return <EmptyValue label="Serves no API, so it admits nobody by a key" />;
  }
  if (auth.digests.length === 0) {
    return (
      <Tag
        variant="neutral"
        appearance="outline"
        title="It serves the API with no keyring, so it publishes no fingerprint of its keys and cannot be compared with any other node."
      >
        No keyring
      </Tag>
    );
  }
  if (here) {
    return (
      <Tag variant="neutral" appearance="outline" title={digestLine(auth.digests)}>
        This node&rsquo;s
      </Tag>
    );
  }
  const words = AGREEMENT_WORDS[auth.agreement];
  return (
    <Tag variant={words.tone} title={`${words.hint} ${digestLine(auth.digests)}`}>
      {words.label}
    </Tag>
  );
}
