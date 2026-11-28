import { render, screen, within } from "@testing-library/react";
import {
  createMemoryRouter,
  RouterProvider,
  type RouteObject,
} from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "./router";

/** Renders the app's routes in a memory router at path. */
function renderAt(path: string, routeList: RouteObject[] = routes) {
  const router = createMemoryRouter(routeList, { initialEntries: [path] });
  return render(<RouterProvider router={router} />);
}

/** The crumb paragraph of the Topbar, or null when the route has no section. */
function crumb(container: HTMLElement) {
  return container.querySelector(".crumb");
}

describe("routes", () => {
  const table: [path: string, section: string, title: string, id?: string][] = [
    ["/", "Operate", "Dashboard"],
    ["/investigations", "Operate", "Investigations"],
    ["/investigations/42", "Operate", "Investigation", "42"],
    ["/cve", "Operate", "CVE"],
    ["/cve/findings/7", "Operate", "CVE finding", "7"],
    ["/knowledge", "Operate", "Knowledge"],
    ["/knowledge/documents/abc", "Operate", "Document", "abc"],
    ["/projects", "Configure", "Projects"],
    ["/settings", "Configure", "Settings"],
  ];

  it.each(table)("%s renders %s / %s in the shell", (path, section, title, id) => {
    const { container } = renderAt(path);

    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent(title);
    expect(crumb(container)).toHaveTextContent(section);
    expect(crumb(container)?.textContent).toBe(section);
    expect(document.title).toBe(`${title} · Ops Copilot`);
    expect(screen.getByRole("navigation", { name: "Main navigation" })).toBeInTheDocument();
    expect(screen.getByText("This screen is not built yet.")).toBeInTheDocument();
    if (id !== undefined) {
      expect(screen.getByText(id)).toBeInTheDocument();
    }
  });

  it("an unknown path renders Page not found with no crumb", () => {
    const { container } = renderAt("/nope");

    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Page not found");
    expect(crumb(container)).toBeNull();
    expect(document.title).toBe("Page not found · Ops Copilot");
    expect(screen.getByRole("navigation", { name: "Main navigation" })).toBeInTheDocument();
  });
});

describe("error boundary", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  /** A page that always throws while rendering. */
  function Boom(): never {
    throw new Error("boom-secret-detail");
  }

  /** The app's routes with a throwing child added under the layout route. */
  function routesWithThrowingChild(): RouteObject[] {
    const [layout, ...rest] = routes;
    return [
      {
        ...layout,
        children: [
          { path: "boom", element: <Boom />, handle: { section: "Operate", title: "Boom" } },
          ...(layout.children ?? []),
        ],
      } as RouteObject,
      ...rest,
    ];
  }

  it("renders the error panel inside the shell without the message", () => {
    // React logs the caught render error; silence that one expected log.
    vi.spyOn(console, "error").mockImplementation(() => {});

    renderAt("/boom", routesWithThrowingChild());

    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Main navigation" });
    expect(within(nav).getByRole("link", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.queryByText(/boom-secret-detail/)).toBeNull();
    expect(document.body.textContent).not.toContain("boom-secret-detail");
  });
});
