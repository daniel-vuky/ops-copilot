import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { App } from "./app/App";

describe("TestRootRenders", () => {
  it("renders the shell at / with the providers", async () => {
    render(<App />);
    expect(await screen.findByRole("heading", { level: 1, name: "Dashboard" })).toBeInTheDocument();
    expect(screen.getByRole("navigation", { name: "Main navigation" })).toBeInTheDocument();
  });
});
