/** Gitlet › Search's facets: the answer's matching lines by repository and directory, each row
 *  narrowing the search on the engine. */

import { memo, type MouseEvent } from "react";
import { SidebarNav, Tag } from "@crewlethq/ui";
import { FolderGlyph, LayersGlyph, SquareCodeGlyph } from "@crewlethq/icons/glyphs";
import { href, useNavigator, useRoute } from "~/app/router.tsx";
import type { Facets as FacetCounts } from "~/lib/codeSearch.ts";
import { fmtCount, plural } from "~/lib/format.ts";

const COUNTED = "matching lines";

/** The `path=` a directory row writes: anchored, as a pattern holding a `/` is. */
export const dirPattern = (dir: string) => `${dir}/**`;

const PARTIAL_WHY = {
  cut: "Counted from the lines answered: the answer was cut, so some lines are not counted.",
  "no total": "Counted from the lines answered: a file’s total was not answered.",
} as const;

function Partial({ partial }: { partial: FacetCounts["partial"] }) {
  if (!partial) return null;
  return (
    <Tag appearance="outline" size="xs" title={PARTIAL_WHY[partial]}>
      Partial
    </Tag>
  );
}

/** Whether a directory row's count holds with `path=` set: under a directory pattern only. */
function countsUnder(path: string, dir: string): boolean {
  if (path === "") return true;
  if (!path.endsWith("/**")) return false;
  const under = path.slice(0, -2);
  return dir === under.slice(0, -1) || dir.startsWith(under);
}

export const Facets = memo(function Facets({ facets }: { facets: FacetCounts }) {
  const route = useRoute();
  const nav = useNavigator();
  const repo = route.query.get("repo") ?? "";
  const path = route.query.get("path") ?? "";
  // Each row is a link to the narrowed address; a press narrows in place, as a filter does.
  const row = (key: "repo" | "path", value: string) => {
    const query = new URLSearchParams(route.query);
    if (value) query.set(key, value);
    else query.delete(key);
    return {
      href: href(route.path, Object.fromEntries(query)),
      onClick: (e: MouseEvent<HTMLElement>) => {
        e.preventDefault();
        nav.filter({ [key]: value || null });
      },
    };
  };
  const heading = (words: string) => (
    <span className="code-search-facets__head">
      {words}
      <Partial partial={facets.partial} />
    </span>
  );
  return (
    <div className="code-search-facets">
      <SidebarNav label="Narrow by repository" dense>
        <SidebarNav.Group label={heading("Repositories")}>
          <SidebarNav.Item
            label="All"
            icon={<LayersGlyph size="sm" />}
            // Narrowed to one repository, the answer holds no count of All.
            count={repo ? undefined : { value: fmtCount(facets.all), label: COUNTED }}
            current={repo === ""}
            {...row("repo", "")}
          />
          {facets.repos.map((r) => (
            <SidebarNav.Item
              key={r.key}
              label={r.key}
              icon={<SquareCodeGlyph size="sm" />}
              count={{ value: fmtCount(r.count), label: COUNTED }}
              current={repo === r.key}
              {...row("repo", repo === r.key ? "" : r.key)}
            />
          ))}
        </SidebarNav.Group>
      </SidebarNav>
      {facets.paths.length > 0 && (
        <SidebarNav label="Narrow by path" dense>
          <SidebarNav.Group label={heading("Paths")}>
            {facets.paths.map((p) => {
              const pattern = dirPattern(p.key);
              return (
                <SidebarNav.Item
                  key={p.key}
                  label={<span className="mono">{p.key}</span>}
                  icon={<FolderGlyph size="sm" />}
                  count={
                    countsUnder(path, p.key)
                      ? { value: fmtCount(p.count), label: COUNTED }
                      : undefined
                  }
                  current={path === pattern}
                  {...row("path", path === pattern ? "" : pattern)}
                />
              );
            })}
          </SidebarNav.Group>
        </SidebarNav>
      )}
      {facets.morePaths > 0 && (
        <p className="code-search-facets__more t-caption muted">
          and {plural(facets.morePaths, "more directory", "more directories")}
        </p>
      )}
    </div>
  );
});
