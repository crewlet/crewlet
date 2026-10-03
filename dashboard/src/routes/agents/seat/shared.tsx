/**
 * What more than one of a seat's tabs draws: the guarded half's states, a
 * value from it in the form it may be shown, and a provider chain.
 */

import type { ReactNode } from "react";
import { EmptyState, InlineCode, Skeleton } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { configValueKind } from "~/lib/format.ts";
import { handleLabel, type Seat, type SeatReading } from "~/lib/seats.ts";

/**
 * The guarded half of a seat, said precisely when it cannot be shown.
 *
 * EVERY OUTCOME OF THE CHART READ HAS ITS OWN SENTENCE ([SeatReading]): a
 * read still out, a refusal naming the grants that would admit the reader, a
 * node that could not answer, a chart that holds no seat by this handle (the
 * roster and the chart can disagree for a moment either side of an apply),
 * and a runtime half the chart withheld from this reader.
 *
 * IT WAS THE COMPANY DOCUMENT, found by the seat's NAME, and the document
 * holds no seats any more — the chart left it for a log of its own — so that
 * read found nothing for every seat in every company.
 *
 * NONE OF THEM IS AN EMPTY VALUE. "Not set" over a field nobody was allowed to
 * read is a statement about the company, and it is the wrong one.
 */
export function SettingsState({
  reading,
  seat,
  children,
}: {
  reading: SeatReading;
  seat: Seat;
  children: ReactNode;
}) {
  switch (reading.state) {
    case "unread":
      return <Skeleton variant="text" rows={3} label="Loading this seat from the org chart" />;
    case "refused":
      // THE SHARED BANNER, which names the grants the engine did and carries
      // the way to sign in as somebody who holds one — a 401, where nothing
      // the engine accepted was presented, is the plain "sign in" banner.
      return (
        <QueryState
          error="unauthorized"
          refusal={
            reading.reason !== "" ? { reason: reading.reason, grants: [...reading.grants] } : null
          }
          loading={false}
        />
      );
    case "failed":
      // THE SHARED BANNER, for the failure it was. A bespoke "The engine did
      // not answer. This panel fills in when it does." was drawn over every
      // failure: a `500` the engine DID answer, a `503` it said waiting will
      // not clear, and a request past its deadline that nothing was going to
      // repeat. The banner says which, and whether the read asks again.
      return (
        <QueryState
          error={reading.failure.error}
          refusal={reading.failure.refusal}
          loading={false}
        />
      );
    case "absent":
      return (
        <EmptyState
          size="compact"
          icon={<KeyGlyph size={32} />}
          title={`The org chart holds no seat ${handleLabel(seat.handle)}`}
          description="The roster and the chart can disagree for a moment while a change is applied."
        />
      );
    case "stripped":
      return (
        <EmptyState
          size="compact"
          icon={<KeyGlyph size={32} />}
          title="This seat's runtime half was not shown to you"
          description="Its model, budget, contact identities and tool credentials are read with config:read, the grant that reads the company's configuration."
        />
      );
    case "read":
      return <>{children}</>;
  }
}

/**
 * A value from a seat's chart row that is NOT a credential — an email, a
 * contact identity — in the form it may be shown: a literal is the value (a
 * Slack member id is a public handle at a vendor), a whole `${VAR}` names an
 * entry in the secret store (a seat's address is SEALED there by the chart,
 * which serves the reference), and the mask says only that something is set.
 *
 * A CREDENTIAL FIELD NEVER COMES THROUGH HERE. Tool credentials are drawn by
 * their NAMES alone on the Settings tab (`credentialState`), so no value the
 * engine sent for one — a reference, the mask, or a literal a redaction bug
 * let through — reaches the page at all.
 */
export function ConfigValue({ value }: { value: string | undefined }) {
  switch (configValueKind(value, { secret: false })) {
    case "reference":
    case "literal":
      return <InlineCode>{value}</InlineCode>;
    case "hidden":
      return <span className="t-caption">A literal value is set (hidden)</span>;
    default:
      return <span className="muted">not set</span>;
  }
}

/**
 * A provider chain, in the order the fallback walks it. Each key opens its
 * model under Settings › Models & keys, where its keys and their cooldowns
 * say whether the seat can reach it right now.
 */
export function ModelChain({ keys }: { keys: readonly string[] }) {
  return (
    <span className="prof-chain">
      {keys.map((key, i) => (
        <span key={key} className="prof-chain-link">
          {i > 0 && (
            <span className="faint" title="falls back to">
              →
            </span>
          )}
          <a className="prof-chain-model" href={href(["settings", "models", key])}>
            <code className="inline">{key}</code>
          </a>
        </span>
      ))}
    </span>
  );
}
