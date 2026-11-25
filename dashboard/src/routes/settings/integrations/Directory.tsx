/**
 * Company › Integrations: one directory of everything the company works in,
 * grouped by what it is for, beside the detail of the one chosen.
 *
 * # Grouped by need, Crewlet's own service first
 *
 * Chat, Work tracking, Docs and knowledge, Code, Observability, Cloud and
 * Infrastructure, then the apps a company grows with: Mobile apps,
 * Advertising, Analytics and Ad revenue (`categories.ts`). Each of
 * Crewlet's own services opens its group (Chatlet, Worklet, Doclet, Gitlet), so
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
 * line, on the Atlassian page), a service (`worklet`, `doclet`, `chat`,
 * `gitlet`), or `google`, the connection every Google product shares; an
 * app's tab and form are its query (`address.ts`).
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

import { useMemo, type ReactNode } from "react";
import {
  Button,
  ButtonLink,
  Callout,
  DirectoryList,
  ListDetail,
  Skeleton,
  StatusDot,
  useMediaMatch,
  useScreenBelow,
  type DirectoryGroup,
} from "@crewlethq/ui";
import { ObjectHeader } from "~/app/frame/ObjectHeader.tsx";
import { VendorMark } from "@crewlethq/icons";
import { ChevronLeftGlyph, KeyGlyph } from "@crewlethq/icons/glyphs";
import { href } from "~/app/router.tsx";
import { useFillScreen } from "~/app/fill.tsx";
import { PHONE_BREAKPOINT } from "~/app/layout.ts";
import { usePageLabels } from "~/app/Shell.tsx";
import { QueryState } from "~/components/common.tsx";
import { needsWords } from "~/lib/access.ts";
import { authorityWords } from "~/lib/refusal.ts";
import { useViewer } from "~/lib/viewer.ts";
import { goSignIn } from "~/lib/session.ts";
import { CATALOG, appHeading, type Entry } from "./catalogue.tsx";
import {
  BUCKET_WORDS,
  useBuiltIns,
  useIntegrations,
  toolFor,
  type BuiltInService,
  type Model,
} from "./model.ts";
import { AppPane } from "./AppPane.tsx";
import { SHOWN, entryOf, type Group, type Line } from "./categories.ts";
import { Landing } from "./Landing.tsx";
import { ServicePane } from "./ServicePane.tsx";
import { ServiceDot, ServiceMark, useDisconnect } from "./parts.tsx";
import { GOOGLE } from "./address.ts";
import { googlePlace } from "./google.ts";
import { GooglePane, googleWords } from "./GooglePane.tsx";

/** Which line a route segment selects: Atlassian and its relay select Jira's. */
function selects(line: Line, kind: string | undefined): boolean {
  if (!kind) return false;
  if (line.key === kind) return true;
  return line.key === "jira" && (kind === "atlassian" || kind === "forge");
}

/**
 * The directory's groups as the list draws them: each line's mark, its name
 * (and its maker's, for a product), its state, and whether it is the one open.
 */
export function directoryGroups(
  shown: readonly Group[],
  {
    kind,
    model,
    services,
    catalog = CATALOG,
  }: {
    kind?: string | undefined;
    model: Model | null;
    services: BuiltInService[];
    catalog?: readonly Entry[];
  },
): DirectoryGroup[] {
  return shown.map((group) => ({
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
          status: <ServiceDot service={s} />,
        };
      }
      if (line.kind === "google") {
        const words = model ? googleWords(googlePlace(model)) : null;
        return {
          key: line.key,
          href: itemHref,
          selected,
          mark: <VendorMark vendor="google" size="md" />,
          label: "Google",
          ...(words ? { status: <StatusDot tone={words.tone}>{words.label}</StatusDot> } : {}),
        };
      }
      const app = entryOf(line, catalog)!;
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
  }));
}

export function Integrations({ kind }: { kind?: string }) {
  const viewer = useViewer();
  const phone = useMediaMatch(`(width < ${PHONE_BREAKPOINT}px)`);
  // THE LIST AND THE PANE EACH SCROLL ON THEIR OWN, so the screen takes the
  // window's height above a phone, where the two are one column.
  useFillScreen(!phone);
  // WHO THE READER IS COULD NOT BE READ: said, with its Try again, never a
  // skeleton waiting for an answer that is not coming.
  if (viewer.failed !== null)
    return <QueryState error={viewer.failed} refusal={viewer.failedRefusal} loading={false} />;
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
      {model.managed !== null && <Callout variant="neutral">{model.managed}</Callout>}
      {model.guarded && (
        <Callout variant="neutral">
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
  const product = SHOWN.flatMap((g) => g.lines).find(
    (l): l is Extract<Line, { kind: "product" }> => l.kind === "product" && l.key === kind,
  );
  // THE CRUMB NAMES WHAT THE PAGE IS ABOUT, not the segment.
  const google = kind === GOOGLE;
  const crumb = google ? "Google" : (service?.name ?? product?.name ?? entry?.name);
  usePageLabels(useMemo(() => (kind && crumb ? { [kind]: crumb } : {}), [kind, crumb]));
  const { disconnect, node } = useDisconnect(model);

  // THE SCREEN'S WIDTH, the question the kit's one-pane rule answers.
  const onePane = useScreenBelow(PHONE_BREAKPOINT);
  const groups = useMemo(
    () => directoryGroups(SHOWN, { kind, model, services }),
    [kind, model, services],
  );

  let pane: ReactNode;
  const cloud = model?.byKey.get("google_cloud");
  if (service) {
    pane = <ServicePane key={service.key} service={service} model={model} />;
  } else if (google && model && cloud) {
    pane = <GooglePane model={model} onDisconnect={() => disconnect(cloud)} />;
  } else if (google && !model) {
    pane = <GoogleHeld />;
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
    pane = <Landing model={model} services={services} note={<AdminNote />} />;
  }

  const list = <DirectoryList label="Integrations" groups={groups} />;
  // ON ONE PANE THE BARE ADDRESS IS THE LANDING, then the directory: the list
  // alone left what waits on the reader in a pane a phone never draws.
  const landingFirst = onePane && !kind;
  return (
    <>
      <ListDetail
        showing={kind || landingFirst ? "detail" : "list"}
        list={landingFirst ? null : list}
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
          {landingFirst && list}
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
      <ObjectHeader {...appHeading(entry, 36)} description={entry.description} />
      <AdminNote />
    </>
  );
}

/** The Google page for a reader without Admin. */
function GoogleHeld() {
  return (
    <>
      <ObjectHeader
        kind="Integration"
        icon="plug"
        mark={<VendorMark vendor="google" size={36} />}
        title="Google"
        description="One connection for every Google product your agents use."
      />
      <AdminNote />
    </>
  );
}
