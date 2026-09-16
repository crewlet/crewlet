/**
 * The knowledge base, the company's own pages.
 *
 * # Browsing and searching are different questions
 *
 * This screen BROWSES: a container, a tree, a page and its history. The
 * Knowledge screen SEARCHES, and ranks. Folding them together would make the
 * common case ("show me what the platform team has written down") a search for
 * a word somebody has to guess.
 *
 * # A company on Confluence has none of this
 *
 * The `pages` question is registered only where this node runs the native
 * knowledge base. On Confluence there is no local copy to browse, by design:
 * search there is live at query time and there is no index to walk.
 *
 * # Read-only, for the reason the work board is
 *
 * A page is written by a seat's own tools or by an operator through MCP, both
 * attributed to somebody. A dashboard button would write as "the dashboard",
 * which is nobody and cannot be asked why.
 */

import { useCallback, useMemo } from "react";
import { href, useParam } from "~/app/router.tsx";
import { QueryState, SeatChip, useTableChoices } from "~/components/common.tsx";
import { useQuery } from "~/lib/useQuery.ts";
import { useOrg } from "~/lib/store-hooks.ts";
import { indexOrg } from "~/lib/seats.ts";
import { fmtDateTime, plural, tsKey } from "~/lib/format.ts";
import type { PageSummary } from "~/protocol/index.ts";
import {
  AccountTreeGlyph,
  Book2Glyph,
  ChatGlyph,
  DescriptionGlyph,
  TimelineGlyph,
  VisibilityGlyph,
} from "@crewlethq/icons/glyphs";
import {
  Card,
  DataView,
  EmptyState,
  EmptyValue,
  FilterChip,
  FilterChipGroup,
  Inline,
  InlineCode,
  Link,
  List,
  PageHeader,
  Prose,
  RelativeTime,
  SegmentedControl,
  Select,
  Skeleton,
  Stack,
  Tag,
  useNow,
  type DataViewColumn,
  type FilterDef,
  type FilterValues,
  type SelectOption,
  type Tone,
} from "@crewlethq/ui";

const STATUS_TONE: Record<string, Tone> = {
  published: "success",
  draft: "warning",
  trashed: "neutral",
};

/**
 * The three answers the kind lens gives, and why the third is a word.
 *
 * THREE STATES on the wire and three here: only the tool-skill pages (auditing
 * the catalogue), everything but them (an ordinary browse), and everything. A
 * checkbox would make one of the three unreachable.
 *
 * `all` is spelled out rather than left empty because a filter set to the
 * empty string is DELETED from the URL, and a deleted `kind` reads back as
 * this screen's fallback. Spelled `""`, the third state put the reader
 * straight back on Pages the moment they chose it, which is the very failure
 * the three values exist to avoid.
 */
const KINDS = ["prose", "skills", "all"] as const;
type Kind = (typeof KINDS)[number];
const DEFAULT_KIND: Kind = "prose";

export function Pages() {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();

  const [container, setContainer] = useParam("container", "");
  const [title, setTitle] = useParam("title", "");
  const [kindParam, setKind] = useParam("kind", DEFAULT_KIND);
  // A URL is the least trusted place there is, and a value the lens does not
  // offer would leave the control showing nothing selected at all.
  const kind: Kind = (KINDS as readonly string[]).includes(kindParam)
    ? (kindParam as Kind)
    : DEFAULT_KIND;

  // THE CONTAINERS ARE A SEPARATE QUESTION from the rows: they are the
  // company's own shelves, which change at the pace somebody creates one,
  // where the rows are re-asked on every filter change.
  const containers = useQuery("containers", undefined, { pollMs: 60_000 });

  const params: Record<string, unknown> = {};
  if (container) params.container = container;
  if (title) params.title = title;
  if (kind === "skills") params.skills = true;
  if (kind === "prose") params.skills = false;

  const { data, loading, error } = useQuery("pages", params, { pollMs: 20_000 });

  const rows = useMemo(
    () => [...(data?.pages ?? [])].sort((a, b) => tsKey(b.updated_at) - tsKey(a.updated_at)),
    [data],
  );

  const knownContainers = useMemo(
    () =>
      [...(containers.data?.containers ?? [])].sort((a, b) =>
        a.key < b.key ? -1 : a.key > b.key ? 1 : 0,
      ),
    [containers.data],
  );

  const containerOptions = useMemo<SelectOption[]>(
    () => [
      { value: "", label: "Every container" },
      ...knownContainers.map((c): SelectOption => {
        // A container's PURPOSE, where the company recorded one. It was a
        // tooltip on the chip row; a second line in the list is the same fact
        // where a reader is actually making the choice.
        const purpose = c.purpose || c.name;
        return purpose
          ? { value: c.key, label: c.key, description: purpose }
          : { value: c.key, label: c.key };
      }),
    ],
    [knownContainers],
  );

  const columns = useMemo<DataViewColumn<PageSummary>[]>(
    () => [
      {
        key: "title",
        header: "Title",
        sortable: true,
        // WHICH PAGE THIS IS. Every other cell here is a fact about a page,
        // and a row of them with no title is a row nobody can act on.
        hideable: false,
        sortValue: (r) => r.title,
        render: (r) => <span className="truncate">{r.title}</span>,
      },
      {
        key: "container",
        header: "Container",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.container,
        render: (r) => (
          <Tag appearance="outline" monospace>
            {r.container}
          </Tag>
        ),
      },
      {
        key: "kind",
        header: "Kind",
        shrink: true,
        sortable: true,
        sortValue: (r) => (r.skill ? "skill" : r.onboarding ? "onboarding" : "page"),
        render: (r) =>
          r.skill ? (
            // A TOOL SKILL IS MACHINERY, marked so a reader does not take it
            // for guidance somebody wrote to be read: it is documentation the
            // engine injects into a phase.
            <Tag variant="info" title="Injected into a phase by the tool-skill registry">
              tool skill
            </Tag>
          ) : r.onboarding ? (
            <Tag variant="warning" title="Where a new seat's reading starts">
              onboarding
            </Tag>
          ) : (
            // An ordinary page takes no chip at all. A mark on every row is a
            // mark that says nothing, and the two above are the exceptions.
            <span className="muted">page</span>
          ),
      },
      {
        key: "status",
        header: "Status",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.status,
        render: (r) => (
          <Tag variant={STATUS_TONE[r.status] ?? "neutral"} dot>
            {r.status}
          </Tag>
        ),
      },
      {
        key: "version",
        header: "Version",
        shrink: true,
        align: "right",
        firstDirection: "desc",
        sortable: true,
        sortValue: (r) => r.version,
        // Tabular figures, which is what makes a right-aligned column of
        // versions line up on its digits rather than on its widths.
        render: (r) => <span className="mono">v{r.version}</span>,
      },
      {
        key: "author",
        header: "Author",
        shrink: true,
        sortable: true,
        sortValue: (r) => r.author ?? "",
        render: (r) =>
          r.author ? (
            <SeatChip name={index.byHandle.get(r.author)?.name ?? r.author} handle={r.author} />
          ) : (
            // A page the engine wrote has no seat behind it. A dash a screen
            // reader says "dash" to is not an answer; this one is named.
            <EmptyValue label="No author recorded" />
          ),
      },
      {
        key: "updated",
        header: "Updated",
        shrink: true,
        align: "right",
        firstDirection: "desc",
        sortable: true,
        sortValue: (r) => tsKey(r.updated_at),
        render: (r) => (
          <RelativeTime value={r.updated_at} now={now} title={fmtDateTime(r.updated_at)} />
        ),
      },
    ],
    [now, index],
  );

  /*
   * THE SCREEN OWNS THE NARROWING. Every axis is a URL parameter and every one
   * is sent to the engine, so the rows that come back are already the answer:
   * a browser applying the same filter a second time would narrow a list that
   * had been narrowed once already, against fields the row may not carry.
   */
  const filters = useMemo<FilterDef<PageSummary>[]>(
    () => [
      {
        name: "title",
        label: "Find a page by title",
        role: "search",
        placeholder: "Words from the title",
      },
    ],
    [],
  );
  const filterValues: FilterValues = { title };
  const onFilterValuesChange = useCallback(
    (next: FilterValues) => setTitle(String(next.title ?? "")),
    [setTitle],
  );

  const choices = useTableChoices({
    screen: "pages",
    columns,
    defaultSort: { key: "updated", direction: "desc" },
    filterKey: `${title}|${container}|${kind}`,
  });

  return (
    <>
      <PageHeader
        title="Pages"
        description="The company's own knowledge base, browsed. To find pages about a subject rather than in a place, search from the Knowledge screen, which ranks."
        badges={<Tag appearance="outline">{plural(rows.length, "page")}</Tag>}
      />

      {/* EVERY CONTAINER THE COMPANY HAS, not the ones this page of rows
          happens to mention: a reader narrowed to one shelf has to be able to
          reach the next. The picker in the toolbar is the same choice for a
          company with more containers than fit on a line. */}
      {knownContainers.length > 0 && (
        <FilterChipGroup
          label="Container"
          semantics="radio"
          allowNone
          value={container || null}
          onValueChange={(next) => setContainer(next ?? "")}
        >
          {knownContainers.map((c) => (
            <FilterChip key={c.key} value={c.key} title={c.purpose || c.name || c.key}>
              {c.key}
            </FilterChip>
          ))}
        </FilterChipGroup>
      )}

      {loading && !rows.length && (
        <Skeleton label="Loading the knowledge base" variant="text" rows={6} />
      )}
      {error && <QueryState error={error} loading={loading} />}

      <DataView<PageSummary>
        framed
        {...choices}
        columns={columns}
        rows={rows}
        rowKey="id"
        getRowHref={(r) => href(["pages", r.id])}
        filters={filters}
        filterValues={filterValues}
        onFilterValuesChange={onFilterValuesChange}
        loading={loading && !rows.length}
        /* A REFUSED QUERY IS NOT AN EMPTY ONE, and the empty state below is
           specific enough to be wrong about it: told "nothing has been written
           down yet", a reader on a Confluence company would be reading a claim
           about a knowledge base this node cannot see at all. The design
           system draws this INSTEAD of the empty state rather than beside it,
           so the two can never contradict each other, and the refusal's own
           sentence stays in the callout above. */
        error={
          error
            ? "The engine did not answer this query, so there are no rows to show. The message above says why."
            : undefined
        }
        toolbarActions={
          <>
            <Select
              ariaLabel="Container"
              size="sm"
              width="auto"
              active={Boolean(container)}
              value={container}
              onChange={(next) => setContainer(String(next))}
              options={containerOptions}
            />
            <SegmentedControl<Kind>
              label="Pages or tool skills"
              semantics="radio"
              size="sm"
              value={kind}
              onValueChange={setKind}
              options={[
                { value: "prose", label: "Pages", title: "Everything but the tool-skill pages" },
                {
                  value: "skills",
                  label: "Tool skills",
                  title: "The machinery a phase is offered",
                },
                { value: "all", label: "All" },
              ]}
            />
          </>
        }
        emptyMessage={
          /* TWO DIFFERENT FACTS, and only ever one of them on screen. A
             company that has never written anything down is told how a
             container comes to exist; a narrowed list that matched nothing is
             told what it was matched against. Drawn together, as they were,
             the first reads as a second empty state under the first.

             THE CONTAINERS ARE A SECOND QUESTION and it answers on its own
             schedule, so "nothing has been written down" is only sayable once
             it has answered. Asked before then it is a guess, and the two
             queries land far enough apart for a reader to read the guess. */
          !containers.loading && knownContainers.length === 0 && !container && !title ? (
            <EmptyState
              size="compact"
              icon={<Book2Glyph />}
              title="Nothing has been written down yet"
              description={
                <>
                  A container is created the first time somebody writes into it. Give a unit a{" "}
                  <InlineCode>space</InlineCode> and its seats will have somewhere to file what they
                  learn.
                </>
              }
            />
          ) : (
            <EmptyState
              size="compact"
              icon={<Book2Glyph />}
              title="No pages here"
              description={
                <>
                  Nothing in this node's copy of the knowledge base matches. Seats write pages with{" "}
                  <InlineCode>write_page</InlineCode>, and a page's container comes from the unit's{" "}
                  <InlineCode>space</InlineCode> field.
                </>
              }
            />
          )
        }
      />
    </>
  );
}

/** One page: its body, where it sits, and everything that changed it. */
export function PageView({ id }: { id: string }) {
  const org = useOrg();
  const index = useMemo(() => indexOrg(org), [org]);
  const now = useNow();
  const { data, loading, error } = useQuery("page", { id }, { enabled: id !== "", pollMs: 20_000 });

  const seatName = (handle: string) => index.byHandle.get(handle)?.name ?? handle;
  const page = data?.page;
  const ancestors = data?.ancestors ?? [];
  const children = data?.children ?? [];
  const comments = data?.comments ?? [];
  const history = data?.history ?? [];

  return (
    <>
      <PageHeader
        title={page?.title || "Page"}
        description={
          page ? (
            /* THE BREADCRUMB IS THE ANCESTOR CHAIN, outermost first: a page's
               place is what makes it findable, and a title alone says nothing
               about which team's tree it is in. The separator is hidden from
               assistive technology, which reads the links themselves. */
            <Inline as="span" gap={1} wrap>
              <Link size="caption" href={href(["pages"], { container: page.container })}>
                {page.container}
              </Link>
              {ancestors.map((a) => (
                <Inline as="span" gap={1} key={a.id}>
                  <span aria-hidden="true">/</span>
                  <Link size="caption" href={href(["pages", a.id])}>
                    {a.title}
                  </Link>
                </Inline>
              ))}
            </Inline>
          ) : undefined
        }
        badges={
          page ? (
            <>
              <Tag variant={STATUS_TONE[page.status] ?? "neutral"} dot>
                {page.status}
              </Tag>
              <Tag appearance="outline" monospace>
                v{page.version}
              </Tag>
              {page.skill && <Tag variant="info">tool skill</Tag>}
            </>
          ) : undefined
        }
      />

      {loading && !page && <Skeleton label="Loading the page" variant="text" rows={8} />}

      <QueryState error={error} loading={loading}>
        {page && (
          <>
            <Card as="section">
              {page.body ? (
                <Prose>{page.body}</Prose>
              ) : (
                <Prose tone="muted">This page has no body.</Prose>
              )}
            </Card>

            {children.length > 0 && (
              <Card as="section">
                <Card.Header icon={<AccountTreeGlyph size="sm" />} count={children.length}>
                  <Card.Title>Children</Card.Title>
                </Card.Header>
                <List variant="divided">
                  {children.map((child) => (
                    <List.Item key={child.id} href={href(["pages", child.id])}>
                      {child.title}
                    </List.Item>
                  ))}
                </List>
              </Card>
            )}

            {page.watchers?.length ? (
              <Card as="section">
                <Card.Header icon={<VisibilityGlyph size="sm" />} count={page.watchers.length}>
                  <Card.Title>Watching</Card.Title>
                </Card.Header>
                <Inline gap={2} wrap>
                  {page.watchers.map((w) => (
                    <SeatChip key={w} name={seatName(w)} handle={w} />
                  ))}
                </Inline>
              </Card>
            ) : null}

            <Card as="section">
              <Card.Header icon={<ChatGlyph size="sm" />} count={comments.length}>
                <Card.Title>Comments</Card.Title>
              </Card.Header>
              {comments.length ? (
                <List variant="divided">
                  {comments.map((c) => (
                    <List.Item key={c.id}>
                      <Stack as="span" gap={1}>
                        <Inline as="span" gap={2}>
                          <SeatChip name={seatName(c.author)} handle={c.author} />
                          <RelativeTime
                            className="t-caption"
                            value={c.created_at}
                            now={now}
                            title={fmtDateTime(c.created_at)}
                          />
                          {c.edited_at && <span className="t-caption muted">(edited)</span>}
                        </Inline>
                        {/* A LIST ROW'S CONTENT SLOT IS A `span`, which holds
                            phrasing content only, so the body takes one too
                            rather than the paragraph Prose draws by default.
                            It is a flex item of the stack either way, so the
                            measure and the leading are unchanged. */}
                        <Prose as="span">{c.body}</Prose>
                      </Stack>
                    </List.Item>
                  ))}
                </List>
              ) : (
                <span className="muted">Nobody has commented.</span>
              )}
            </Card>

            <Card as="section">
              <Card.Header icon={<TimelineGlyph size="sm" />} count={history.length}>
                <Card.Title>History</Card.Title>
              </Card.Header>
              {history.length ? (
                <List variant="divided">
                  {history.map((rev) => (
                    <List.Item
                      key={rev.version}
                      leading={<DescriptionGlyph size="sm" />}
                      trailing={
                        <RelativeTime
                          className="t-caption"
                          value={rev.created_at}
                          now={now}
                          title={fmtDateTime(rev.created_at)}
                        />
                      }
                    >
                      <Inline as="span" gap={2}>
                        <span className="mono">v{rev.version}</span>
                        <span>{rev.author ? seatName(rev.author) : "the engine"}</span>
                        {rev.message && <span className="muted truncate">{rev.message}</span>}
                      </Inline>
                    </List.Item>
                  ))}
                </List>
              ) : (
                // The METADATA ONLY note matters: a reader who expected to
                // click a version and read it should be told why they cannot
                // rather than left looking for the link.
                <span className="muted">
                  Only this version exists. Past versions are kept as metadata here; reading one
                  back is a coordination read the engine does on demand.
                </span>
              )}
            </Card>
          </>
        )}
      </QueryState>
    </>
  );
}
