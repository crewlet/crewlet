/**
 * The console's frame chunk: its sidebar, and the door the person sidebar
 * draws into it for an admin. See `app/lazyScreen.ts`.
 *
 * NOTHING OUTSIDE `app/` IMPORTS THIS FILE, and nothing inside it imports it
 * statically — a static import from the frame would pull the console's code
 * into the entry every member downloads.
 */

export { AdminDoor } from "./AdminDoor.tsx";
export { ConsoleSidebar } from "./ConsoleSidebar.tsx";
