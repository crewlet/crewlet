/**
 * How each widget draws, the same in view and in edit: a contributed widget
 * is its section's own component, a layout widget is the design system's own
 * primitive.
 */

import { Button, Card, Divider, ErrorBoundary, Link, Note, Text } from "@crewlethq/ui";
import { LinkGlyph } from "@crewlethq/icons/glyphs";
import { renderMarkdown } from "~/lib/markdown.ts";
import { useViewer } from "~/lib/viewer.ts";
import { WidgetNote, destinationOf } from "./kit.tsx";
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

/**
 * A widget a section offers, drawn by the section's component, or the
 * sentence that says why it is not: its section no longer offers it, the
 * reader's access no longer opens its page, or it failed to draw. A failure
 * costs that one widget, never the board.
 */
function Contributed({ widget }: { widget: ContribWidget }) {
  const viewer = useViewer();
  const def = widgetDef(widget.widget);
  if (!def)
    return <Unavailable>This widget is no longer available. Remove it in Edit Home.</Unavailable>;
  if (viewer.loading) return null;
  if (!canUse(def, viewer.grants)) {
    const page = destinationOf(def.from)?.label ?? def.label;
    return <Unavailable>{`You no longer have access to ${page}.`}</Unavailable>;
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
