import { useEffect } from "react";
import { Outlet, useMatches, type UIMatch } from "react-router";
import { ErrorPanel } from "./ErrorPanel";
import { placeholder } from "./placeholder";
import { Sidebar } from "./Sidebar";
import { Topbar } from "./Topbar";

/** RouteHandle is the `handle` of every route: the Topbar crumb and title. */
export type RouteHandle = { section: "Operate" | "Configure" | null; title: string };

/** The handle used when no matched route has one. */
const fallbackHandle: RouteHandle = { section: null, title: "Ops Copilot" };

/** isRouteHandle reports whether a route's handle is a RouteHandle. */
function isRouteHandle(handle: unknown): handle is RouteHandle {
  return (
    typeof handle === "object" &&
    handle !== null &&
    typeof (handle as { title?: unknown }).title === "string"
  );
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

/** LayoutProps: error is set when the layout renders as the route error element. */
export type LayoutProps = { error?: boolean };

/**
 * Layout is the app frame: the Sidebar, the Topbar for the current route and
 * the page. As the layout route's error element it shows ErrorPanel in place
 * of the page, so the navigation stays usable.
 */
export function Layout({ error = false }: LayoutProps) {
  const { section, title } = deepestHandle(useMatches());

  useEffect(() => {
    document.title = `${title} · Ops Copilot`;
  }, [title]);

  return (
    <div className="oc-root">
      <div className="oc-shell">
        <div className="oc-side-wrap">
          <Sidebar user={placeholder.user} />
        </div>
        <main className="oc-main">
          <Topbar
            section={section}
            title={title}
            mode={placeholder.mode}
            timeZoneLabel={placeholder.timeZoneLabel}
            user={placeholder.user}
          />
          {error ? <ErrorPanel /> : <Outlet />}
        </main>
      </div>
    </div>
  );
}
