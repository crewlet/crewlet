/**
 * What more than one place that draws a seat draws: the guarded document's
 * states, a value from it in the form it may be shown, a provider chain, and
 * the way from the reader's own seat to their Account — which the profile's
 * Overview and the seat's peek both carry.
 */

import type { ReactNode } from "react";
import { Card, EmptyState, InlineCode, Skeleton } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { GrantRequired } from "~/app/frame/GrantRequired.tsx";
import { configValueKind } from "~/lib/format.ts";
import {
  isOwnSeat,
  RUNTIME_GRANT,
  type Seat,
  type SeatReading,
  type SeatSettings,
} from "~/lib/seats.ts";
import { useViewer } from "~/lib/viewer.ts";
import type { CompanyDocument, LogRefusal, QueryRefusal } from "~/protocol/index.ts";

/**
 * The operator-gated half of a seat, said precisely when it cannot be shown.
 *
 * A READ NEVER SENT COMES FIRST. The document takes `config:read` and is not
 * asked of a reader without it who leads no unit over the seat (`useSeatSetup`
 * reads that as `withheld`), so there is no answer to draw — and the panel says
 * which grant reads it, in the frame's words (`GrantRequired`), and that
 * leading the seat's unit does too where it has one. Read as a document that
 * answered with nothing, a person opening their own seat's Settings was told
 * "No company configuration is active" about a company running beside it.
 *
 * `QueryState` covers a refused or failed read, which includes the guarded
 * banner with its Set token button. The three states after it are this
 * screen's own: no configuration is active — only ever an ANSWER, a read that
 * came back with no document — the document has no seat by this name (the
 * projection and the document can disagree for a moment either side of an
 * apply), and a name held by two seats — unique names are an admission rule an
 * apply does not refuse, so a revision a newer node activated can hold two.
 *
 * NONE OF THEM IS AN EMPTY VALUE. "Not set" over a field nobody was allowed to
 * read is a statement about the company, and it is the wrong one.
 */
export function SettingsState({
  reading,
  error,
  refusal,
  loading,
  doc,
  settings,
  seat,
  children,
}: {
  /** [useSeatSetup]'s reading, which alone knows whether the read was sent. */
  reading: SeatReading;
  error: string | null;
  refusal: QueryRefusal | LogRefusal | null;
  /** In flight — or waiting on the viewer before it can be asked at all. */
  loading: boolean;
  doc: CompanyDocument | null;
  settings: SeatSettings | null;
  seat: Seat;
  children: ReactNode;
}) {
  const viewer = useViewer();
  if (reading.state === "withheld") {
    return (
      <GrantRequired
        what="Reading this seat's settings"
        grants={[RUNTIME_GRANT]}
        // THE VIEWER HAS ANSWERED by construction: `withheld` is decided only
        // once it has (`useSeatSetup`), so it names the reader it decided on.
        reader={viewer.login}
        size="compact"
        also={
          seat.unit
            ? `Whoever leads ${seat.unit.name}, or a unit above it, reads them without it.`
            : undefined
        }
      />
    );
  }
  if (loading && !doc && !error) {
    return <Skeleton variant="text" rows={3} label="Loading the company document" />;
  }
  if (error) return <QueryState error={error} refusal={refusal} loading={loading} />;
  if (!doc) {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title="No company configuration is active"
        description="This seat's settings live in the company document, and none is active on this engine."
      />
    );
  }
  if (settings?.state === "missing") {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title={`The active configuration has no seat named ${seat.name}`}
        description="The org chart and the configuration can disagree for a moment while a new revision is applied."
      />
    );
  }
  if (settings?.state === "ambiguous") {
    return (
      <EmptyState
        size="compact"
        icon={<KeyGlyph size={32} />}
        title={`More than one seat is named ${seat.name}`}
        description="The active revision holds two seats with this name, so its settings cannot be attributed to one of them. Rename one of the seats to fix it."
      />
    );
  }
  return <>{children}</>;
}

/**
 * A value from the redacted document that is NOT a credential — an email, a
 * contact identity — in the form it may be shown: a literal is the value (a
 * Slack member id is a public handle at a vendor), a whole `${VAR}` names an
 * entry in the secret store.
 *
 * A CREDENTIAL FIELD NEVER COMES THROUGH HERE. Tool credentials are drawn by
 * their NAMES alone on the Settings tab (`CredentialState`), so no value the
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

/**
 * The way from the reader's own seat to their own Account — and nothing on
 * anybody else's.
 *
 * THE SEAT IS NOT THE ACCOUNT, and from here it looked like one: a person
 * opening their own page met their name, "Person" and a Settings tab, with no
 * password, no second factor and no sessions on it — and concluded the
 * dashboard had no two-step verification to set up. The seat is the company's
 * page about a place in the chart; how the person in it signs in is theirs,
 * and lives at `#/account`.
 *
 * IT DECIDES FOR ITSELF WHOSE SEAT THIS IS, as Message does, so every place
 * that draws a seat carries it on the reader's own and on no other: the
 * profile drew it and the peek — the chart's first answer to "who is this" —
 * did not, so a person who found themselves on the chart found no way to their
 * own security there. Nobody manages another person's second factor from a
 * seat.
 */
export function YourAccount({ handle }: { handle: string }) {
  const viewer = useViewer();
  if (!isOwnSeat(viewer, handle)) return null;
  return (
    <Card padding="none" className="prof-card">
      <Card.Header divided={false}>
        <Card.Title as="h3">Your account</Card.Title>
      </Card.Header>
      <p className="prof-note">
        This seat is yours. Your password, two-step verification, where you are signed in and your
        personal access tokens are on your{" "}
        <a className="t-link prose-link" href={href(["account"])}>
          Account
        </a>{" "}
        page.
      </p>
    </Card>
  );
}
