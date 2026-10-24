/**
 * Company › Policies: the standing policies every agent reads on every turn.
 *
 * # Plain words, and only those
 *
 * A policy is a sentence the company writes and every executor is given,
 * verbatim: "every amount is exact to the cent". The numbers the engine holds
 * a turn to (its review rounds, tool rounds, delegation depth) are not
 * policies the company writes but caps the engine enforces, and live once, in
 * Settings › Limits; this page says where, and draws none of them.
 *
 * Read from the org projection by every reader, and drawn only once it has
 * arrived ([CharterPage]).
 */

import { Card, EmptyState } from "@crewlethq/ui";
import { ShieldGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import type { OrgProjection } from "~/protocol/index.ts";
import { CharterPage } from "./charter.tsx";

/** Where the caps the engine holds every turn to are read. */
const LIMITS_PATH = ["settings", "limits"];

export function Policies() {
  return (
    <CharterPage loading="Loading the company's policies">
      {(org) => <PolicyList org={org} />}
    </CharterPage>
  );
}

function PolicyList({ org }: { org: OrgProjection }) {
  const policies = org.policies ?? [];
  return (
    <div className="col gap-3">
      <Card>
        <Card.Header icon={<ShieldGlyph size="sm" />} count={policies.length}>
          <Card.Title>Standing policies</Card.Title>
        </Card.Header>
        {policies.length > 0 ? (
          <ol className="col gap-2" style={{ paddingLeft: "var(--spacing-4)", margin: 0 }}>
            {policies.map((p, i) => (
              <li key={i} className="t-body measure">
                {p}
              </li>
            ))}
          </ol>
        ) : (
          <EmptyState
            size="compact"
            icon={<ShieldGlyph size={32} />}
            title="No policies are set"
            description="Standing instructions for every agent."
          />
        )}
      </Card>
      <p className="t-caption muted measure">
        The caps the engine holds every turn to are in{" "}
        <a className="t-link prose-link" href={href(LIMITS_PATH)}>
          Settings › Limits
        </a>
        .
      </p>
    </div>
  );
}
