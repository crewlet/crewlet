/**
 * The Charter lens: the company's own mission, vision and policies, and what
 * each unit is for.
 *
 * Founder prose, and the half of a company the whole product exists for:
 * policies render into every seat's prompt in full, so an operator asking why
 * a seat did something needs to read the standing instructions it was given.
 */

import { Section } from "~/components/common.tsx";
import type { OrgIndex } from "~/lib/seats.ts";
import type { OrgProjection } from "~/protocol/index.ts";
import { Card, EmptyState } from "@crewlethq/ui";
import { ExploreGlyph, ShieldGlyph, TargetGlyph } from "@crewlethq/icons/glyphs";

export function Charter({ org, index }: { org: OrgProjection; index: OrgIndex }) {
  const policies = org.policies ?? [];
  return (
    <div className="col gap-4">
      <Card as="section">
        <Card.Header icon={<TargetGlyph size="sm" />}>
          <Card.Title>Mission</Card.Title>
        </Card.Header>
        <p className="t-body measure">
          {org.mission || <span className="muted">No mission is set.</span>}
        </p>
      </Card>
      {org.vision && (
        <Card as="section">
          <Card.Header icon={<ExploreGlyph size="sm" />}>
            <Card.Title>Vision</Card.Title>
          </Card.Header>
          <p className="t-body measure">{org.vision}</p>
        </Card>
      )}
      <Card as="section">
        <Card.Header icon={<ShieldGlyph size="sm" />} count={policies.length}>
          <Card.Title>Policies</Card.Title>
        </Card.Header>
        {policies.length ? (
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
            icon={<ShieldGlyph />}
            title="No policies are set"
            description="Policies render into every planner's prompt in full. They are the company's standing instructions."
          />
        )}
      </Card>
      {index.units.length > 0 && (
        <Section title="Unit goals" hint="what each team is for">
          <div className="grid grid-auto">
            {index.units.map((u) => (
              <Card key={u.key} as="section">
                <Card.Header subtitle={u.type || undefined}>
                  <Card.Title>{u.name}</Card.Title>
                </Card.Header>
                {u.purpose && <p className="t-caption">{u.purpose}</p>}
                {u.goals.length ? (
                  <ul
                    className="col gap-1"
                    style={{ paddingLeft: "var(--spacing-4)", margin: "var(--spacing-2) 0 0" }}
                  >
                    {u.goals.map((g, i) => (
                      <li key={i} className="t-cell">
                        {g}
                      </li>
                    ))}
                  </ul>
                ) : (
                  <span className="t-caption">No goals set.</span>
                )}
                {u.knowledge.length > 0 && (
                  <p className="t-caption" style={{ marginTop: "var(--spacing-2)" }}>
                    Knowledge: {u.knowledge.join(", ")}
                  </p>
                )}
              </Card>
            ))}
          </div>
        </Section>
      )}
    </div>
  );
}
