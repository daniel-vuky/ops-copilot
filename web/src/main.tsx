// SPA entry point (ADR 0001 §13). Fonts are bundled as hashed files, never
// loaded from another origin (the CSP is default-src 'self').
import "@fontsource-variable/inter/index.css";
import "@fontsource-variable/jetbrains-mono/index.css";
// Design tokens (a checked copy), the glass layer straight from design/ui,
// then the app overrides, in that order.
import "./styles/colors_and_type.css";
import "../../design/ui/oc-glass.css";
import "./styles/app.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./app/App";

const root = document.getElementById("root");
if (!root) {
  throw new Error("index.html has no #root element");
}

createRoot(root).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
