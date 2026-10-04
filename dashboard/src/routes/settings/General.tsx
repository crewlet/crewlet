/**
 * Settings › General: the company's charter — its mission, its vision and the
 * standing policies every executor is given.
 *
 * # Why the charter is a setting
 *
 * It was a lens of the company screen beside the chart and the builder, and
 * it is neither a way of looking at the org nor a way of changing it: it is
 * what the company has told every seat, verbatim, in every executor's prompt.
 * That makes it the first thing an operator configures and the first place a
 * founder looks for "what did we tell them" — Settings, under Company.
 *
 * It reads the org PROJECTION rather than the guarded configuration, so a
 * reader without `config:read` can read it: the charter is not a
 * secret, and a settings screen that refused everybody would hide the one
 * section of Settings that is the company's own voice. A unit's goals are the
 * unit's, and are drawn at Agents › Teams.
 */

import { Card, EmptyState, Skeleton } from "@crewlethq/ui";
import { CompassGlyph, ShieldGlyph, TargetGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { PageActions } from "~/app/frame/PageActions.tsx";
import { PageNote } from "~/app/frame/PageNote.tsx";
import { useOrg } from "~/lib/store-hooks.ts";
import { PreviousRevisionNote } from "~/routes/org/builder/AfterSaveStrip.tsx";

export function General() {
  const org = useOrg();
  return (
    <>
      <PageActions>
        <a className="t-link" href={href(["agents", "edit"])}>
          Edit in org →
        </a>
      </PageActions>
      <PageNote>
        What the company has told every seat. Policies render into every executor&rsquo;s prompt in
        full.
      </PageNote>
      <PreviousRevisionNote />
      {org ? (
        <Charter org={org} />
      ) : (
        // NOTHING IS CLAIMED BEFORE THE CHART ARRIVES. "No mission is set" and
        // a policy count of 0 are statements about the company, and before
        // the org push lands this browser knows nothing about it at all — a
        // figure the engine did not answer is absent, never a zero.
        <div aria-busy="true">
          <Card>
            <Skeleton variant="text" rows={4} label="Loading the company's charter" />
          </Card>
        </div>
      )}
    </>
  );
}

/** The charter itself, drawn only from a chart the engine has sent. */
function Charter({ org }: { org: NonNullable<ReturnType<typeof useOrg>> }) {
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
