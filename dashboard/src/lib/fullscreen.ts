/**
 * Give the screen back from fullscreen, where something is holding it. A
 * fullscreen element paints only its own subtree, so a gesture that opens
 * something beside it (a sheet, a dialog of the shell) leaves fullscreen
 * first. A browser that refuses has nothing to give back.
 */
export function leaveFullscreen(): void {
  if (document.fullscreenElement && typeof document.exitFullscreen === "function") {
    void document.exitFullscreen().catch(() => {});
  }
}
