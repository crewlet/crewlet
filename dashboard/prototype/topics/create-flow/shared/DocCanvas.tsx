/**
 * A new doc, open on its page: the title typed where it will be read, the
 * text under it in the doc editor's own `RichEditor`, so the first thing a
 * person sees after choosing Docs is a blank page with a caret in its title.
 * Enter in the title moves to the text, as every document editor does.
 */

import { useEffect, useRef, type KeyboardEvent } from "react";
import { Input, RichEditor, type DocumentSize, type RichEditorHandle } from "@crewlethq/ui";

export interface DocDraftState {
  title: string;
  body: string;
}

export function DocCanvas({
  draft,
  onChange,
  size = "md",
  autoFocus = true,
  placeholder = "Untitled doc",
  className,
}: {
  draft: DocDraftState;
  onChange: (next: DocDraftState) => void;
  size?: DocumentSize;
  autoFocus?: boolean;
  placeholder?: string;
  className?: string;
}) {
  const title = useRef<HTMLInputElement>(null);
  const text = useRef<RichEditorHandle>(null);
  useEffect(() => {
    if (autoFocus) title.current?.focus();
  }, [autoFocus]);
  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "Enter" || e.key === "ArrowDown") {
      e.preventDefault();
      text.current?.focus();
    }
  };
  return (
    <div className={["cf-doc", className].filter(Boolean).join(" ")}>
      <Input
        ref={title}
        appearance="title"
        width="full"
        aria-label="Title"
        placeholder={placeholder}
        value={draft.title}
        onKeyDown={onKey}
        onChange={(e) => onChange({ ...draft, title: e.target.value })}
      />
      <RichEditor
        ref={text}
        label="Doc text"
        size={size}
        placeholder="Start writing. Headings, lists, tables and checklists are in the toolbar, or type them as Markdown."
        value={draft.body}
        onChange={(body) => onChange({ ...draft, body })}
      />
    </div>
  );
}
