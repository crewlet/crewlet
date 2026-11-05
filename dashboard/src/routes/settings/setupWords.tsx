/**
 * The engine's words on a connect form, as a person reads them: a sentence
 * whose `{link}` (`setup.LinkMark`) is the vendor page it names. Shared by the
 * form's field help and its Setup steps, which place links the same way.
 */

import { Fragment, type ReactNode } from "react";
import { marked } from "~/ui/Problems.tsx";

/** Where a field's vendor link sits in its words (`setup.LinkMark`). */
export const LINK_MARK = "{link}";

/** A field's vendor link, resolved: its address ("" until it has one) and its words. */
export interface FieldLink {
  href: string;
  text: string;
}

/**
 * The engine's words with `{link}` drawn as the link it names, and every other
 * literal picked out as the rest of the form's copy is ([marked]).
 *
 * THE WORDS OUTLIVE THE ADDRESS. A per-organization page has no address until
 * the organization id is typed, and the name of the page is still true and
 * still the useful half, so the words stay and only the anchor waits.
 */
export function withLink(text: string, link: FieldLink | undefined): ReactNode {
  const parts = text.split(LINK_MARK);
  return parts.map((part, i) => (
    <Fragment key={i}>
      {marked(part)}
      {i < parts.length - 1 &&
        (link?.href ? (
          <a className="prose-link" href={link.href} target="_blank" rel="noreferrer">
            {link.text}
          </a>
        ) : (
          (link?.text ?? "")
        ))}
    </Fragment>
  ));
}
