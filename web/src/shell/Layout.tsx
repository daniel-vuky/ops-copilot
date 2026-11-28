import { useEffect, useRef, type MouseEvent } from "react";
import { Outlet, useLocation, useMatches, type UIMatch } from "react-router";
import { placeholder } from "./placeholder";
import { Sidebar } from "./Sidebar";
import { Topbar } from "./Topbar";

/** RouteHandle is the `handle` of every route: the Topbar crumb and title. */
export type RouteHandle = { section: "Operate" | "Configure" | null; title: string };

/** The handle used when no matched route has one. */
const fallbackHandle: RouteHandle = { section: null, title: "Ops Copilot" };

/** The sections a RouteHandle may name. */
const sections: readonly unknown[] = [null, "Operate", "Configure"];

/** isRouteHandle reports whether a route's handle is a RouteHandle. */
function isRouteHandle(handle: unknown): handle is RouteHandle {
  if (typeof handle !== "object" || handle === null) {
    return false;
  }
  const { section, title } = handle as { section?: unknown; title?: unknown };
  return typeof title === "string" && sections.includes(section);
}

/** deepestHandle returns the handle of the deepest matched route that has one. */
function deepestHandle(matches: UIMatch[]): RouteHandle {
  for (let i = matches.length - 1; i >= 0; i--) {
    const handle = matches[i].handle;
    if (isRouteHandle(handle)) {
      return handle;
    }
  }
  return fallbackHandle;
}

/**
 * Layout is the app frame: the Sidebar, the Topbar for the current route and
 * the page. Page errors are caught by the pathless route under it (see
 * app/router.tsx), so the shell stays mounted and ErrorPanel renders in the
 * Outlet. Errors in the shell itself fall through to RootError.
 */
export function Layout() {
  const { section, title } = deepestHandle(useMatches());
  const { pathname } = useLocation();
  const titleRef = useRef<HTMLHeadingElement>(null);
  const mainRef = useRef<HTMLElement>(null);
  const shownPath = useRef(pathname);

  useEffect(() => {
    document.title = `${title} · Ops Copilot`;
  }, [title]);

  // After a navigation (not the first render), focus the new page's title so
  // screen readers announce it and focus never drops to <body> when the
  // collapsed menu hides the link that was followed.
  useEffect(() => {
    if (shownPath.current === pathname) {
      return;
    }
    shownPath.current = pathname;
    titleRef.current?.focus();
  }, [pathname]);

  const skipToMain = (event: MouseEvent<HTMLAnchorElement>) => {
    event.preventDefault();
    mainRef.current?.focus();
  };

  return (
    <div className="oc-root">
      <a className="oc-skip" href="#main" onClick={skipToMain}>
        Skip to content
      </a>
      <div className="oc-shell">
        <div className="oc-side-wrap">
          <Sidebar user={placeholder.user} />
        </div>
        <main className="oc-main" id="main" ref={mainRef} tabIndex={-1}>
          <Topbar
            section={section}
            title={title}
            mode={placeholder.mode}
            timeZoneLabel={placeholder.timeZoneLabel}
            user={placeholder.user}
            titleRef={titleRef}
          />
          <Outlet />
        </main>
      </div>
    </div>
  );
}
