/**
 * Quick's gallery: the shipped one, with its Make row as one line of kinds,
 * each a key that starts one from anywhere on the gallery.
 *
 * A KEY IS TAKEN ONLY WHEN NOTHING ELSE WANTS IT: not while a field, an
 * editable region or a dialog holds the focus, and never with a modifier,
 * so typing a search or a browser's own shortcut is never turned into a new
 * artifact.
 */

import { useEffect } from "react";
import { ArtifactKindTile } from "@crewlethq/ui";
import { href, useNavigator } from "~/app/router.tsx";
import { KIND, MAKE_ORDER } from "../../shared/kinds.ts";
import { withMake } from "../../shared/gallery.tsx";

export { noMatch } from "@original/routes/artifacts/Gallery.tsx";

function typingIn(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  return (
    target.isContentEditable ||
    target.closest("input, textarea, select, [role='dialog'], [role='menu'], [role='listbox']") !==
      null
  );
}

function QuickMake() {
  const nav = useNavigator();
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey || typingIn(e.target)) return;
      const kind = MAKE_ORDER.find((k) => KIND[k].key.toLowerCase() === e.key.toLowerCase());
      if (!kind) return;
      e.preventDefault();
      nav.to(["artifacts", "new"], { kind });
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [nav]);
  return (
    <section className="cfq-make" aria-labelledby="cfq-title">
      <h2 className="cfq-title" id="cfq-title">
        New
      </h2>
      <ul className="cfq-row" role="list">
        {MAKE_ORDER.map((k) => (
          <li key={k}>
            <ArtifactKindTile
              appearance="chip"
              kind={k}
              label={KIND[k].tile}
              shortcut={KIND[k].key}
              href={href(["artifacts", "new"], { kind: k })}
            />
          </li>
        ))}
      </ul>
    </section>
  );
}

export const ArtifactGallery = withMake(QuickMake);
