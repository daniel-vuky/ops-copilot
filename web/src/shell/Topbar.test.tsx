import { render, screen, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { describe, expect, it } from "vitest";
import type { InvestigatorMode } from "./placeholder";
import { modeLabel, Topbar } from "./Topbar";

/** Renders the Topbar in a memory router (its search box is a router Form). */
function renderTopbar(mode: InvestigatorMode, section: string | null = "Operate") {
  const router = createMemoryRouter(
    [
      {
        path: "*",
        element: (
          <Topbar
            section={section}
            title="Dashboard"
            mode={mode}
            timeZoneLabel="ICT · UTC+7"
            user={{ name: "Signed-in user", initials: "SU" }}
          />
        ),
      },
    ],
    { initialEntries: ["/"] },
  );
  return render(<RouterProvider router={router} />);
}

describe("Topbar", () => {
  it("shows the cli mode chip", () => {
    renderTopbar("cli");
    expect(screen.getByText("POC · cli")).toHaveClass("mode");
    expect(screen.queryByText("Production · api")).toBeNull();
  });

  it("shows the api mode chip", () => {
    renderTopbar("api");
    expect(screen.getByText("Production · api")).toHaveClass("mode");
  });

  it("maps each mode to its label", () => {
    expect(modeLabel("cli")).toBe("POC · cli");
    expect(modeLabel("api")).toBe("Production · api");
  });

  it("shows the crumb, title and time-zone note", () => {
    const { container } = renderTopbar("cli");
    expect(container.querySelector(".crumb")?.textContent).toBe("Operate");
    expect(screen.getByRole("heading", { level: 1, name: "Dashboard" })).toBeInTheDocument();
    expect(screen.getByText("ICT · UTC+7")).toBeInTheDocument();
  });

  it("omits the crumb when there is no section", () => {
    const { container } = renderTopbar("cli", null);
    expect(container.querySelector(".crumb")).toBeNull();
  });

  it("has a document search form", () => {
    renderTopbar("cli");
    const form = screen.getByRole("search", { name: "Site search" });
    expect(form).toHaveAttribute("method", "get");
    expect(form).toHaveAttribute("action", "/knowledge");
    const box = within(form).getByRole("searchbox", { name: "Search documents" });
    expect(box).toHaveAttribute("placeholder", "Search documents…");
    expect(box).toHaveAttribute("name", "q");
  });

  it("names the signed-in user for screen readers", () => {
    renderTopbar("cli");
    expect(screen.getByText("Signed in as Signed-in user")).toHaveClass("sr");
  });
});
