import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
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
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});

    renderAt("/boom", routesWithThrowingChild());

    expect(consoleError).toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    const nav = screen.getByRole("navigation", { name: "Main navigation" });
    expect(within(nav).getByRole("link", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.queryByText(/boom-secret-detail/)).toBeNull();
    expect(document.body.textContent).not.toContain("boom-secret-detail");
  });

  it("recovers when a nav link is followed from the error frame", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const user = userEvent.setup();

    renderAt("/boom", routesWithThrowingChild());
    const nav = screen.getByRole("navigation", { name: "Main navigation" });
    await user.click(within(nav).getByRole("link", { name: "Projects" }));

    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Projects");
    expect(screen.queryByText("Something went wrong")).toBeNull();
    expect(screen.getByText("This screen is not built yet.")).toBeInTheDocument();
  });
});

describe("focus", () => {
  it("does not move focus on the first render", () => {
    renderAt("/");
    expect(document.activeElement).toBe(document.body);
  });

  it("moves focus to the new page title after navigation", async () => {
    const user = userEvent.setup();
    renderAt("/");
    const nav = screen.getByRole("navigation", { name: "Main navigation" });

    await user.click(within(nav).getByRole("link", { name: "Knowledge" }));

    const title = screen.getByRole("heading", { level: 1 });
    expect(title).toHaveTextContent("Knowledge");
    expect(title).toHaveFocus();
    expect(title).toHaveAttribute("tabindex", "-1");
  });

  it("offers a skip link to the main content as the first focusable element", async () => {
    const user = userEvent.setup();
    renderAt("/");
    const skip = screen.getByRole("link", { name: "Skip to content" });
    const main = screen.getByRole("main");

    expect(skip).toHaveAttribute("href", "#main");
    expect(main).toHaveAttribute("id", "main");
    expect(main).toHaveAttribute("tabindex", "-1");

    await user.tab();
    expect(skip).toHaveFocus();
    await user.click(skip);
    expect(main).toHaveFocus();
  });
});
