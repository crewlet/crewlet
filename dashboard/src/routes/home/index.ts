/**
 * The Home workspace's chunk: every screen `app/lazyScreen.ts` loads for it.
 * Nothing outside `app/` imports this file — a static import of it from the
 * frame would pull the workspace back into the entry chunk.
 */

export { Home } from "./Home.tsx";
