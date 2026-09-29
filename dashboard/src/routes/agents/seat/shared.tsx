/**
 * What more than one of a seat's tabs draws: the guarded document's states,
 * a value from it in the form it may be shown, and a provider chain.
 */

import type { ReactNode } from "react";
import { EmptyState, InlineCode, Skeleton } from "@crewlethq/ui";
import { KeyGlyph } from "@crewlethq/icons/glyphs";
import { QueryState } from "~/components/common.tsx";
import { href } from "~/app/router.tsx";
import { configValueKind } from "~/lib/format.ts";
import type { Seat, SeatSettings } from "~/lib/seats.ts";
import type { CompanyDocument } from "~/protocol/index.ts";

/**
 * The operator-gated half of a seat, said precisely when it cannot be shown.
 *
 * `QueryState` covers a refused or failed read, which includes the guarded
 * banner with its Set token button. The three states after it are this
 * screen's own: no configuration is active, the document has no seat by this
 * name (the projection and the document can disagree for a moment either side
 * of an apply), and a name held by two seats in a revision stored before names
 * had to be unique.
 *
 * NONE OF THEM IS AN EMPTY VALUE. "Not set" over a field nobody was allowed to
 * read is a statement about the company, and it is the wrong one.
 */
export function SettingsState({
  error,
  loading,
  doc,
  settings,
  seat,
  children,
}: {
  error: string | null;
  loading: boolean;
  doc: CompanyDocument | null;
  settings: SeatSettings | null;
  seat: Seat;
  children: ReactNode;
}) {
  if (loading && !doc && !error) {
    return <Skeleton variant="text" rows={3} label="Loading the company document" />;
  }
  if (error) return <QueryState error={error} loading={loading} />;
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
        description="This revision was stored before seat names had to be unique, so its settings cannot be attributed to one of them. Rename one of the seats to fix it."
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
