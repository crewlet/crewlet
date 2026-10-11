/**
 * The company's charter — its mission, its vision and the standing policies
 * every executor is given — drawn from the org projection.
 *
 * ONE DRAWING FOR BOTH SHELLS. A teammate reads it as Team › About; an admin
 * edits what it says from the console's Company, which draws this beside the
 * way to edit it. Two drawings of one charter would be two places a policy's
 * wording could be shown differently from the prompt it renders into.
 *
 * It reads the PROJECTION rather than the guarded configuration, so a reader
 * without an admin key can read it — a member, and under the default
 * `api.auth.anonymous: public` anybody at all: the charter is not a secret,
 * and it is the one part of the company document that is the company's own
 * voice. A unit's goals are the unit's, and are drawn at Team › Units.
 */

import { Card, EmptyState, Skeleton } from "@crewlethq/ui";
import { CompassGlyph, ShieldGlyph, TargetGlyph } from "@crewlethq/icons/glyphs";
import { useOrg } from "~/lib/store-hooks.ts";

/** The charter, or a placeholder until the chart arrives — never a claim made from no answer. */
export function Charter() {
  const org = useOrg();
  if (!org) {
    // NOTHING IS CLAIMED BEFORE THE CHART ARRIVES. "No mission is set" and a
    // policy count of 0 are statements about the company, and before the org
    // push lands this browser knows nothing about it at all — a figure the
    // engine did not answer is absent, never a zero.
    return (
      <div aria-busy="true">
        <Card>
          <Skeleton variant="text" rows={4} label="Loading the company's charter" />
        </Card>
      </div>
    );
  }
  const policies = org.policies ?? [];
  return (
    <div className="col gap-4">
      <Card>
        <Card.Header icon={<TargetGlyph size="sm" />}>
          <Card.Title>Mission</Card.Title>
        </Card.Header>
        <p className="t-body measure">
          {org.mission || <span className="muted">No mission is set.</span>}
        </p>
      </Card>
      {org.vision && (
        <Card>
          <Card.Header icon={<CompassGlyph size="sm" />}>
            <Card.Title>Vision</Card.Title>
          </Card.Header>
          <p className="t-body measure">{org.vision}</p>
        </Card>
      )}
      <Card>
        <Card.Header icon={<ShieldGlyph size="sm" />} count={policies.length}>
          <Card.Title>Policies</Card.Title>
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
            description="Policies render into every executor's prompt in full. They are the company's standing instructions."
          />
        )}
      </Card>
    </div>
  );
}
