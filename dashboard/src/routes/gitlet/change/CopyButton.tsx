/** A name copied by one press, the control saying it landed: a branch's in the header, the reference's in the rail. */

import { IconButton, useClipboard } from "@crewlethq/ui";
import { CheckGlyph, CopyGlyph } from "@crewlethq/icons/glyphs";

export function CopyButton({ text, label }: { text: string; label: string }) {
  const { state, copy } = useClipboard({ failedResetMs: null });
  return (
    <IconButton
      label={
        state === "copied"
          ? `Copied ${text}`
          : state === "failed"
            ? "The browser refused the clipboard"
            : label
      }
      icon={state === "copied" ? <CheckGlyph /> : <CopyGlyph />}
      size="sm"
      variant="ghost"
      onClick={() => void copy(text)}
    />
  );
}
