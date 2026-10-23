/**
 * Company › Integrations: one directory of everything the company works in,
 * grouped by what it is for, beside the detail of the one chosen.
 *
 * # Grouped by need, Crewlet's own service first
 *
 * Chat, Work tracking, Docs and knowledge, Code and Observability. Each of
 * Crewlet's own services opens its group (Chatlet, Worklet, Doclet), so
 * the built-in answer to a need sits beside the apps that answer it in its
 * place, with In use or Not in use on its line. Each names Crewlet beside
 * it as its maker, the way Jira names Atlassian. ATLASSIAN IS LISTED BY
 * PRODUCT: it is one connection that serves two needs, so Jira sits under work
 * tracking and Confluence under docs, each with its own product's state, and
 * both open the one Atlassian page, since it is one key and one account per
 * agent.
 *
 * # The address is the route
 *
 * `#/company/integrations/{kind}` selects a line: an app's key, a surface
 * (`jira`, `confluence`, the `forge` relay and `atlassian` select Jira's
 * line, on the Atlassian page), or a service (`worklet`, `doclet`, `chat`).
 * The bare address shows what the company has active.
 *
 * # Who reads it
 *
 * Every reader. The services are the overview every reader holds (the org
 * projection says what the company runs), so a reader without Admin sees them
 * in full and the apps with no state; their pages say an admin manages them.
 * Nothing on that page asks a question the engine keeps for Admins: the
 * `integrations` question and `/setup` are read only on an Admin's page.
 *
 * # Two columns that scroll on their own
 *
 * Above a phone's width the screen asks the frame for the window's height
 * (`useFillScreen`), as the Inbox does, so the directory holds its place
 * while the detail scrolls beside it. On a
 * phone the two are one column and the page scrolls as any other.
 */

import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  DirectoryList,
  ListDetail,
  Skeleton,
  StatusDot,
  useMediaMatch,
  type DirectoryGroup,
} from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { ChevronLeftGlyph, KeyGlyph, ShieldGlyph } from "@crewlethq/icons/glyphs";
import { href, useParam } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { needsWords } from "~/lib/access.ts";
import { useViewer } from "~/lib/viewer.ts";
import { goSignIn } from "~/lib/session.ts";
import { CATALOG, type Entry } from "./catalogue.tsx";
import {
  BUCKET_WORDS,
  useBuiltIns,
  useIntegrations,
  toolFor,
  type Intent,
  type Model,
} from "./model.ts";
import { AppPane } from "./AppPane.tsx";
import { DIRECTORY, entryOf, type Line } from "./categories.ts";
import { Landing } from "./Landing.tsx";
import { ServicePane } from "./ServicePane.tsx";
import { ServiceMark, ServiceState, useDisconnect } from "./parts.tsx";

/** Which line a route segment selects: Atlassian and its relay select Jira's. */
function selects(line: Line, kind: string | undefined): boolean {
  if (!kind) return false;
  if (line.key === kind) return true;
  return line.key === "jira" && (kind === "atlassian" || kind === "forge");
}

/** The intents an arrival may ask an app's page to open its form for: the home's fixes. */
const ARRIVALS: readonly Intent[] = ["connect", "continue", "rotate", "settings", "agents"];

export function Integrations({ kind }: { kind?: string }) {
  const viewer = useViewer();
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  // THE LIST AND THE PANE EACH SCROLL ON THEIR OWN, so the screen takes the
  // window's height above a phone, where the two are one column.
  useFillScreen(!phone);
  if (viewer.loading) return <Skeleton variant="text" rows={6} label="Checking your access" />;
  return viewer.admin ? <AdminPage kind={kind} /> : <Directory kind={kind} model={null} />;
}

/**
 * An Admin's page: the model the directory's states and the apps' panes read.
 * A reader without Admin never builds one.
 */
function AdminPage({ kind }: { kind?: string | undefined }) {
  const model = useIntegrations();
  if (model.loading) return <Skeleton variant="text" rows={8} label="Loading integrations" />;
  return (
    <>
      {model.managed !== null && (
        <Callout variant="neutral" icon={<ShieldGlyph size="md" />}>
          {model.managed}
        </Callout>
      )}
      {model.guarded && (
        <Callout variant="neutral" icon={<KeyGlyph size="md" />}>
          {needsWords("Reading the integrations' setup state", model.needs)}
        </Callout>
      )}
      <QueryState error={model.error} refusal={model.refusal} loading={false}>
        <Directory kind={kind} model={model} />
      </QueryState>
    </>
  );
}

function Directory({ kind, model }: { kind?: string | undefined; model: Model | null }) {
  const services = useBuiltIns();
  const service = services.find((s) => s.key === kind);
  const tool = model ? toolFor(model, kind) : undefined;
  const entry = CATALOG.find((e) => e.key === kind || e.surfaces.some((s) => s.key === kind));
  const product = DIRECTORY.flatMap((g) => g.lines).find(
    (l): l is Extract<Line, { kind: "product" }> => l.kind === "product" && l.key === kind,
  );
  // THE CRUMB NAMES WHAT THE PAGE IS ABOUT, not the segment.
  const crumb = service?.name ?? product?.name ?? entry?.name;
  usePageLabels(useMemo(() => (kind && crumb ? { [kind]: crumb } : {}), [kind, crumb]));
  const { disconnect, node } = useDisconnect(model);
  // AN ARRIVAL'S INTENT BELONGS TO THE SELECTION IT ARRIVED WITH, and is
  // taken out of the address once read, so a reload or a step back does not
  // open the form again. The page is never keyed on the selection (App.tsx),
  // so a fix followed from the home is a change of address under a mounted
  // page: the intent is held per selection rather than read once at mount,
  // and survives the address being cleared, because an app's pane reads it
  // when it mounts, which may be after the address was cleared.
  // Leaving for another selection drops it, so coming back through the list
  // opens no form.
  const [arrive, setArrive] = useParam("open", "");
  const asked = ARRIVALS.find((i) => i === arrive);
  const [arrival, setArrival] = useState<{ kind: string | undefined; intent: Intent } | null>(() =>
    asked ? { kind, intent: asked } : null,
  );
  if (asked && (!arrival || arrival.kind !== kind || arrival.intent !== asked)) {
    setArrival({ kind, intent: asked });
  } else if (!asked && arrival && arrival.kind !== kind) {
    setArrival(null);
  }
  const arrived = asked ?? (arrival && arrival.kind === kind ? arrival.intent : undefined);
  useEffect(() => {
    if (arrive !== "") setArrive("");
  }, [arrive, setArrive]);

  const groups = useMemo<DirectoryGroup[]>(
    () =>
      DIRECTORY.map((group) => ({
        key: group.key,
        label: group.label,
        items: group.lines.map((line) => {
          const selected = selects(line, kind);
          const itemHref = href(["company", "integrations", line.key]);
          if (line.kind === "service") {
            const s = services.find((x) => x.key === line.key)!;
            return {
              key: line.key,
              href: itemHref,
              selected,
              mark: <ServiceMark service={s} size="md" />,
              label: s.name,
              ...(s.vendor ? { hint: s.vendor } : {}),
              status: <ServiceState service={s} />,
            };
          }
          const app = entryOf(line)!;
          const appTool = model?.byKey.get(app.key);
          // A PRODUCT'S STATE IS ITS OWN: Jira says nothing while only
          // Confluence is connected.
          const on =
            appTool &&
            (line.kind === "product"
              ? appTool.sections.some((s) => s.tool.key === line.key && s.tool.configured)
              : appTool.configured);
          const words = appTool && on ? BUCKET_WORDS[appTool.bucket] : undefined;
          return {
            key: line.key,
            href: itemHref,
            selected,
            mark: <VendorMark vendor={app.vendor} size="md" />,
            label: line.kind === "product" ? line.name : app.name,
            ...(line.kind === "product" ? { hint: app.name } : {}),
            ...(words ? { status: <StatusDot tone={words.tone}>{words.label}</StatusDot> } : {}),
          };
        }),
      })),
    [kind, model, services],
  );

  let pane: ReactNode;
  if (service) {
    pane = <ServicePane key={service.key} service={service} model={model} />;
  } else if (tool && model) {
    pane = (
      <AppPane
        key={tool.entry.key}
        tool={tool}
        model={model}
        onDisconnect={() => disconnect(tool)}
        arrive={arrived}
      />
    );
  } else if (entry) {
    pane = <HeldPane key={entry.key} entry={entry} />;
  } else if (kind) {
    pane = <p className="t-caption">This build serves no integration called “{kind}”.</p>;
  } else {
    pane = <Landing model={model} services={services} note={<AdminNote />} />;
  }

  return (
    <>
      <ListDetail
        showing={kind ? "detail" : "list"}
        list={<DirectoryList label="Integrations" groups={groups} />}
      >
        {/* ONE COLUMN THE PAGE'S PARTS ARE SPACED IN, where a lone control
            keeps its own width rather than the column's. */}
        <div className="int-detail">
          {kind && (
            <ButtonLink
              className="int-back"
              size="small"
              variant="ghost"
              href={href(["company", "integrations"])}
            >
              <ChevronLeftGlyph size="sm" aria-hidden /> All integrations
            </ButtonLink>
          )}
          {pane}
        </div>
      </ListDetail>
      {node}
    </>
  );
}

/**
 * What a reader without Admin is told on an app's page and the summary.
 *
 * THE DOOR, FOR A READER WHO IS NOBODY: a note that only names the missing
 * access leaves them nothing on the page that can supply it. A signed-in
 * reader lacks a permission, which signing in again as themselves would not
 * change, so they get the sentence alone.
 */
function AdminNote() {
  const viewer = useViewer();
  return (
    <Callout
      variant="neutral"
      icon={<ShieldGlyph size="md" />}
      title="Admins connect and manage apps"
      action={
        viewer.anonymous ? (
          <Button size="small" leadingIcon={<KeyGlyph size="sm" />} onClick={goSignIn}>
            Sign in
          </Button>
        ) : undefined
      }
    >
      {needsWords("Seeing which apps are connected and how they are doing", ["admin"])}
    </Callout>
  );
}

/** An app's page for a reader without Admin: what it is, and who manages it. */
function HeldPane({ entry }: { entry: Entry }) {
  return (
    <>
      <ObjectHeader
        kind="Integration"
        icon="plug"
        identifier={entry.key}
        mark={<VendorMark vendor={entry.vendor} size={36} />}
        title={entry.name}
        description={entry.description}
      />
      <AdminNote />
    </>
  );
}
