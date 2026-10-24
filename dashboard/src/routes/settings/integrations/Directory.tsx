/**
 * Company › Integrations: one directory of everything the company works in,
 * grouped by what it is for, beside the detail of the one chosen.
 *
 * # Grouped by need, Crewlet's own service first
 *
 * Chat, Work tracking, Docs and knowledge, Code and Observability. Each of
 * Crewlet's own services opens its group (Crewlet Chat, Works, Knowledge),
 * so the built-in answer to a need sits beside the apps that answer it in its
 * place, with its On or Off on its line. ATLASSIAN IS LISTED BY
 * PRODUCT: it is one connection that serves two needs, so Jira sits under work
 * tracking and Confluence under docs, each with its own product's state, and
 * both open the one Atlassian page, since it is one key and one account per
 * agent.
 *
 * # The address is the route
 *
 * `#/company/integrations/{kind}` selects a line: an app's key, a surface
 * (`jira`, `confluence`, the `forge` relay and `atlassian` select Jira's
 * line, on the Atlassian page), or a service (`works`, `knowledge`, `chat`).
 * The bare address shows what needs a look.
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
 * (`useFillScreen`), as the Inbox does, so the directory holds its place with
 * the engine's address at its foot while the detail scrolls beside it. On a
 * phone the two are one column and the page scrolls as any other.
 */

import { useMemo, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  DirectoryList,
  ListDetail,
  Section,
  Skeleton,
  StatusDot,
  useMediaMatch,
  type DirectoryGroup,
} from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { ChevronLeftGlyph, KeyGlyph, ShieldGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { needsWords } from "~/lib/access.ts";
import { authorityWords } from "~/lib/refusal.ts";
import { useViewer } from "~/lib/viewer.ts";
import { goSignIn } from "~/lib/session.ts";
import { CATALOG, type Entry } from "./catalogue.tsx";
import {
  BUCKET_WORDS,
  useBuiltIns,
  useIntegrations,
  toolFor,
  type BuiltInService,
  type Model,
} from "./model.ts";
import { AppPane } from "./AppPane.tsx";
import { ServicePane } from "./ServicePane.tsx";
import { EngineAddress, IssueRow, ServiceMark, ServiceState, useDisconnect } from "./parts.tsx";

/** One line of the directory: a built-in service, an app, or one product of an app. */
type Line =
  | { kind: "service"; key: BuiltInService["key"] }
  | { kind: "app"; key: string }
  | { kind: "product"; key: string; name: string; app: string };

/** The directory's groups, in the order a company reaches for them. */
const DIRECTORY: { key: string; label: string; lines: Line[] }[] = [
  {
    key: "chat",
    label: "Chat",
    lines: [
      { kind: "service", key: "chat" },
      { kind: "app", key: "slack" },
      { kind: "app", key: "mattermost" },
    ],
  },
  {
    key: "work",
    label: "Work tracking",
    lines: [
      { kind: "service", key: "works" },
      { kind: "product", key: "jira", name: "Jira", app: "atlassian" },
    ],
  },
  {
    key: "docs",
    label: "Docs and knowledge",
    lines: [
      { kind: "service", key: "knowledge" },
      { kind: "product", key: "confluence", name: "Confluence", app: "atlassian" },
    ],
  },
  {
    key: "code",
    label: "Code",
    lines: [
      { kind: "app", key: "github" },
      { kind: "app", key: "gitlab" },
    ],
  },
  { key: "alerts", label: "Observability", lines: [{ kind: "app", key: "datadog" }] },
];

/** Which line a route segment selects: Atlassian and its relay select Jira's. */
function selects(line: Line, kind: string | undefined): boolean {
  if (!kind) return false;
  if (line.key === kind) return true;
  return line.key === "jira" && (kind === "atlassian" || kind === "forge");
}

/** The catalogue entry an app line or a product line stands for. */
function entryOf(line: Line): Entry | undefined {
  if (line.kind === "service") return undefined;
  return CATALOG.find((e) => e.key === (line.kind === "product" ? line.app : line.key));
}

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
          {authorityWords("Reading the integrations' setup state", model.refused)}
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
      />
    );
  } else if (entry) {
    pane = <HeldPane key={entry.key} entry={entry} />;
  } else if (kind) {
    pane = <p className="t-caption">This build serves no integration called “{kind}”.</p>;
  } else {
    pane = <Summary model={model} services={services} />;
  }

  return (
    <>
      <ListDetail
        showing={kind ? "detail" : "list"}
        list={
          <DirectoryList
            label="Integrations"
            groups={groups}
            foot={model?.base ? <EngineAddress value={model.base} /> : undefined}
          />
        }
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

/** With nothing chosen: what needs a look, or where to start. */
function Summary({ model, services }: { model: Model | null; services: BuiltInService[] }) {
  if (!model) {
    const on = services.filter((s) => s.on).map((s) => s.name);
    return (
      <>
        <ObjectHeader
          kind="Company"
          icon="plug"
          title="Integrations"
          description={
            (on.length > 0
              ? `${on.join(" and ")} ${on.length === 1 ? "is" : "are"} on for your company.`
              : "Your company runs none of Crewlet's own services.") +
            " Pick one to see what it holds."
          }
        />
        <AdminNote />
      </>
    );
  }
  const attention = model.tools.filter((t) => t.bucket === "attention");
  const connected = model.tools.filter((t) => t.configured).length;
  return (
    <>
      <ObjectHeader
        kind="Company"
        icon="plug"
        title="Integrations"
        description={
          connected === 0
            ? "No app is connected yet. Pick one to see what it gives your agents."
            : `${connected} of ${model.tools.length} apps connected. ${attention.length === 0 ? "Nothing needs you." : "Pick an item to manage it."}`
        }
      />
      {attention.length > 0 && (
        <Section title="Needs a look">
          <div className="int-issues">
            {attention.flatMap((t) =>
              t.issues.map((issue) => (
                <IssueRow
                  key={issue.key}
                  tool={t}
                  issue={issue}
                  go={href(["company", "integrations", t.entry.key])}
                  showTool
                />
              )),
            )}
          </div>
        </Section>
      )}
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
