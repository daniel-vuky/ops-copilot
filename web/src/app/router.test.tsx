import { act, render, screen, within } from "@testing-library/react";
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
  return { router, ...render(<RouterProvider router={router} />) };
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

  /** The app's routes with a throwing page added under the pathless page route. */
  function routesWithThrowingChild(): RouteObject[] {
    const [layout, ...rest] = routes;
    const [pages, ...otherChildren] = layout.children ?? [];
    expect(pages.path).toBeUndefined();
    expect(pages.errorElement).toBeDefined();
    return [
      {
        ...layout,
        children: [
          {
            ...pages,
            children: [
              { path: "boom", element: <Boom />, handle: { section: "Operate", title: "Boom" } },
              ...(pages.children ?? []),
            ],
          } as RouteObject,
          ...otherChildren,
        ],
      } as RouteObject,
      ...rest,
    ];
  }

  /** The Sidebar navigation landmark. */
  function nav() {
    return screen.getByRole("navigation", { name: "Main navigation" });
  }

  it("renders the error panel inside the mounted shell without the message", async () => {
    // React logs the caught render error; silence that one expected log.
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    const { router } = renderAt("/", routesWithThrowingChild());
    const navBefore = nav();

    await act(() => router.navigate("/boom"));

    expect(consoleError).toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Something went wrong");
    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    expect(nav()).toBe(navBefore);
    expect(within(nav()).getByRole("link", { name: "Dashboard" })).toBeInTheDocument();
    expect(screen.queryByText(/boom-secret-detail/)).toBeNull();
    expect(document.body.textContent).not.toContain("boom-secret-detail");
  });

  it("recovers when a nav link is followed from the error frame", async () => {
    vi.spyOn(console, "error").mockImplementation(() => {});
    const user = userEvent.setup();
    renderAt("/boom", routesWithThrowingChild());
    const navBefore = nav();

    await user.click(within(navBefore).getByRole("link", { name: "Projects" }));

    const title = screen.getByRole("heading", { level: 1 });
    expect(title).toHaveTextContent("Projects");
    expect(title).toHaveFocus();
    expect(nav()).toBe(navBefore);
    expect(screen.queryByText("Something went wrong")).toBeNull();
    expect(screen.getByText("This screen is not built yet.")).toBeInTheDocument();
  });

  it("falls back to a static message when the shell itself fails", () => {
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => {});
    const [layout, ...rest] = routes;

    renderAt("/", [{ ...layout, element: <Boom /> } as RouteObject, ...rest]);

    expect(consoleError).toHaveBeenCalled();
    expect(screen.getByText("Something went wrong. Reload the page.")).toBeInTheDocument();
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(document.body.textContent).not.toContain("boom-secret-detail");
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

describe("collapsed menu", () => {
  /** The Menu button of the Sidebar. */
  function menu() {
    const nav = screen.getByRole("navigation", { name: "Main navigation" });
    return within(nav).getByRole("button", { name: "Menu" });
  }

  /** Renders the app at path and opens the collapsed menu. */
  async function openAt(path: string) {
    const user = userEvent.setup();
    const rendered = renderAt(path);
    await user.click(menu());
    expect(menu()).toHaveAttribute("aria-expanded", "true");
    expect(document.querySelector("#oc-nav-list")).toHaveClass("nav-open");
    return { user, ...rendered };
  }

  /** Checks that the menu is closed. */
  function expectClosed() {
    expect(menu()).toHaveAttribute("aria-expanded", "false");
    expect(menu()).toHaveAttribute("aria-controls", "oc-nav-list");
    expect(document.querySelector("#oc-nav-list")).not.toHaveClass("nav-open");
  }

  it("closes after a Topbar search", async () => {
    const { user } = await openAt("/");
    await user.type(screen.getByRole("searchbox", { name: "Search documents" }), "runbook{Enter}");
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Knowledge");
    expectClosed();
  });

  it("closes after an in-page link", async () => {
    const { user } = await openAt("/nope");
    await user.click(screen.getByRole("link", { name: "Go to the dashboard" }));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Dashboard");
    expectClosed();
  });

  it("closes after back and forward", async () => {
    const { router } = renderAt("/");
    await act(() => router.navigate("/cve"));
    const user = userEvent.setup();
    await user.click(menu());
    expect(menu()).toHaveAttribute("aria-expanded", "true");
    await act(() => router.navigate(-1));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Dashboard");
    expectClosed();

    await user.click(menu());
    expect(menu()).toHaveAttribute("aria-expanded", "true");
    await act(() => router.navigate(1));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("CVE");
    expectClosed();
  });

  it("stays closed when going back after a Topbar search", async () => {
    const { user, router } = await openAt("/");
    await user.type(screen.getByRole("searchbox", { name: "Search documents" }), "runbook{Enter}");
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Knowledge");
    await act(() => router.navigate(-1));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Dashboard");
    expectClosed();
  });

  it("stays closed when going back after an in-page link", async () => {
    const { user, router } = await openAt("/nope");
    await user.click(screen.getByRole("link", { name: "Go to the dashboard" }));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Dashboard");
    await act(() => router.navigate(-1));
    expect(screen.getByRole("heading", { level: 1 })).toHaveTextContent("Page not found");
    expectClosed();
  });

  it("still closes on a nav link and toggles with the Menu button", async () => {
    const { user } = await openAt("/");
    await user.click(menu());
    expectClosed();
    await user.click(menu());
    const nav = screen.getByRole("navigation", { name: "Main navigation" });
    await user.click(within(nav).getByRole("link", { name: "Settings" }));
    expectClosed();
  });
});
