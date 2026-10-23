/**
 * How each widget draws, the same in view and in edit: a contributed widget
 * is its section's own component, a layout widget is the design system's own
 * primitive.
 */

import { Button, Card, Divider, ErrorBoundary, Link, Note, Skeleton, Text } from "@crewlethq/ui";
import { LinkGlyph } from "@crewlethq/icons/glyphs";
import { renderMarkdown } from "~/lib/markdown.ts";
import { SERVICE_NAMES, useOrgArrived, useServicesRun, type ServicesRun } from "~/lib/services.ts";
import { useViewer } from "~/lib/viewer.ts";
import { WidgetNote, destinationOf, pageOpens, widgetMissing } from "./kit.tsx";
import { canUse, widgetDef } from "./registry.tsx";
import { isExternal, type ContribWidget, type Leaf, type LinksWidget } from "./model.ts";

export function LeafView({ widget }: { widget: Leaf }) {
  switch (widget.kind) {
    case "widget":
      return <Contributed widget={widget} />;
    case "note":
      return (
        <Note
          tone={widget.tone}
          align={widget.align}
          size={widget.size}
          className="home-block-note"
        >
          <div className="prose md">{renderMarkdown(widget.text, { headings: "text" })}</div>
        </Note>
      );
    case "heading": {
      const centred = widget.align === "center";
      return (
        <Text
          as="h3"
          variant={widget.size === "lg" ? "display" : "heading"}
          className={
            centred ? "home-block-heading home-block-heading--center" : "home-block-heading"
          }
        >
          {widget.text || "Heading"}
        </Text>
      );
    }
    case "divider":
      return (
        <Divider
          label={widget.label || undefined}
          align={widget.align}
          className="home-block-divider"
        />
      );
    case "links":
      return <LinksCard widget={widget} />;
  }
}

/** The card a contributed widget is drawn in when it cannot draw itself. */
function Unavailable({ children }: { children: string }) {
  return (
    <Card padding="none" className="home-card">
      <WidgetNote>{children}</WidgetNote>
    </Card>
  );
}

/** No service at all: what a widget is measured against to ask whether it reads one. */
const NONE_RUN: ServicesRun = { tracker: false, knowledge: false, chat: false };

/** The card a widget holds while the board waits to learn what the company runs. */
function WidgetSkeleton() {
  return (
    <Card padding="none" className="home-card">
      <Card.Body padding="md">
        <Skeleton variant="text" rows={2} label="Checking what the company runs" />
      </Card.Body>
    </Card>
  );
}

/**
 * A widget a section offers, drawn by the section's component, or the
 * sentence that says why it is not: its section no longer offers it, the
 * company does not run the service it reads (said only while Home is edited:
 * in view the board closes up over it), the reader's access no longer opens
 * its page, or it failed to draw. A failure costs that one widget, never the
 * board.
 */
function Contributed({ widget }: { widget: ContribWidget }) {
  const viewer = useViewer();
  const runs = useServicesRun();
  const arrived = useOrgArrived();
  const def = widgetDef(widget.widget);
  if (!def)
    return <Unavailable>This widget is no longer available. Remove it in Edit Home.</Unavailable>;
  // A WIDGET READING ONE OF CREWLET'S SERVICES WAITS FOR THE PROJECTION on
  // a cold load: drawn on the guess, a tracker widget on a company running
  // Jira asked its reads and was refused before the board closed over it.
  if (!arrived && widgetMissing(def, widget.params, NONE_RUN) !== undefined) {
    return <WidgetSkeleton />;
  }
  const service = widgetMissing(def, widget.params, runs);
  if (service !== undefined) {
    return (
      <Unavailable>{`Hidden: the company does not run ${SERVICE_NAMES[service]}. Remove it in Edit Home.`}</Unavailable>
    );
  }
  if (viewer.loading) return null;
  if (!canUse(def, viewer.grants)) {
    // THE PAGE WHERE THE PAGE IS CLOSED, the widget where only what it reads
    // is: Integrations opens for every reader while its widget is an Admin's.
    const dest = destinationOf(def.from);
    const closed = dest && !pageOpens(dest, viewer.grants) ? dest.label : def.label;
    return <Unavailable>{`You no longer have access to ${closed}.`}</Unavailable>;
  }
  const { Component } = def;
  return (
    <ErrorBoundary
      resetKey={JSON.stringify(widget.params)}
      fallback={(_error, reset) => (
        <Card padding="none" className="home-card">
          <p className="home-card-note" role="alert">
            {`${def.label} could not be drawn. `}
            <Button variant="ghost" size="small" onClick={reset}>
              Try again
            </Button>
          </p>
        </Card>
      )}
    >
      <Component params={widget.params} />
    </ErrorBoundary>
  );
}

function LinksCard({ widget }: { widget: LinksWidget }) {
  return (
    <Card padding="none" className="home-card">
      <Card.Header icon={<LinkGlyph size="sm" />}>
        <Card.Title as="h3">{widget.title || "Links"}</Card.Title>
      </Card.Header>
      {widget.links.length === 0 ? (
        <p className="home-card-note">No links yet. Add them in Edit Home.</p>
      ) : (
        <ul className="home-block-links-list">
          {widget.links.map((l, i) => {
            const external = isExternal(l.href);
            return (
              <li key={`${l.href}-${i}`}>
                <Link href={l.href} external={external} className="home-block-link">
                  {l.label || l.href}
                </Link>
              </li>
            );
          })}
        </ul>
      )}
    </Card>
  );
}
