import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { describe, expect, it } from "vitest";
import { placeholder } from "./placeholder";
import { Sidebar } from "./Sidebar";

/** Renders the Sidebar alone in a memory router at path. */
function renderAt(path: string) {
  const router = createMemoryRouter(
    [{ path: "*", element: <Sidebar user={placeholder.user} /> }],
    { initialEntries: [path] },
  );
  render(<RouterProvider router={router} />);
  return screen.getByRole("navigation", { name: "Main navigation" });
}

/** The links of the nav whose aria-current is "page". */
function currentLinks(nav: HTMLElement) {
  return within(nav)
    .getAllByRole("link")
    .filter((link) => link.getAttribute("aria-current") === "page");
}

describe("Sidebar", () => {
  it("keeps the section active on a detail route", () => {
    const nav = renderAt("/investigations/42");
    const current = currentLinks(nav);
    expect(current).toHaveLength(1);
    expect(current[0]).toHaveAccessibleName("Investigations");
    expect(current[0]).toHaveClass("nav-a", "nav-on");
  });

  it("marks only Dashboard at /", () => {
    const nav = renderAt("/");
    const current = currentLinks(nav);
    expect(current).toHaveLength(1);
    expect(current[0]).toHaveAccessibleName("Dashboard");
    const others = within(nav)
      .getAllByRole("link")
      .filter((link) => link !== current[0]);
    expect(others).toHaveLength(5);
    for (const link of others) {
      expect(link).not.toHaveClass("nav-on");
    }
  });

  it("lists the Operate and Configure groups in design order", () => {
    const nav = renderAt("/");
    const names = (group: string) =>
      within(within(nav).getByRole("list", { name: group }))
        .getAllByRole("link")
        .map((link) => link.textContent);
    expect(names("Operate")).toEqual(["Dashboard", "Investigations", "CVE", "Knowledge"]);
    expect(names("Configure")).toEqual(["Projects", "Settings"]);
    expect(within(nav).getByRole("link", { name: "CVE" })).toHaveAttribute("href", "/cve");
  });

  it("toggles the menu and closes it on navigation", async () => {
    const user = userEvent.setup();
    const nav = renderAt("/");
    const menu = within(nav).getByRole("button", { name: "Menu" });
    const list = nav.querySelector("#oc-nav-list");

    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(menu).toHaveAttribute("aria-controls", "oc-nav-list");
    expect(list).not.toHaveClass("nav-open");

    await user.click(menu);
    expect(menu).toHaveAttribute("aria-expanded", "true");
    expect(list).toHaveClass("nav-list", "nav-open");

    await user.click(within(nav).getByRole("link", { name: "Knowledge" }));
    expect(menu).toHaveAttribute("aria-expanded", "false");
    expect(list).not.toHaveClass("nav-open");
    expect(currentLinks(nav)[0]).toHaveAccessibleName("Knowledge");
  });

  it("shows the System panel without fake health", () => {
    const nav = renderAt("/");
    const panel = within(nav).getByRole("region", { name: "System" });
    for (const label of ["Investigator", "Scheduler", "Company MCP", "Secret filter"]) {
      expect(within(panel).getByText(label)).toBeInTheDocument();
    }
    expect(within(panel).getAllByText("—")).toHaveLength(4);
    expect(within(panel).getAllByText("not connected yet")).toHaveLength(4);
    expect(panel.textContent).not.toMatch(/Reachable|Running|Ready/);
    expect(panel.querySelectorAll(".dot.dot-off")).toHaveLength(4);
    expect(panel.querySelector(".dot-ok")).toBeNull();
  });

  it("shows the placeholder user and a disabled Sign out", () => {
    const nav = renderAt("/");
    expect(within(nav).getByText("Signed-in user")).toBeInTheDocument();
    expect(within(nav).getByText("Member")).toBeInTheDocument();
    expect(within(nav).getByRole("button", { name: "Sign out" })).toBeDisabled();
  });

  it("shows the brand", () => {
    const nav = renderAt("/");
    expect(within(nav).getByText("Ops Copilot")).toBeInTheDocument();
    expect(within(nav).getByText("Internal tool")).toBeInTheDocument();
  });
});
