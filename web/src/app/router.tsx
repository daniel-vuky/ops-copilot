import { createBrowserRouter, type RouteObject } from "react-router";
import { NotFound } from "../pages/NotFound";
import { DetailPlaceholder, Placeholder } from "../pages/Placeholder";
import { ErrorPanel } from "../shell/ErrorPanel";
import { Layout, type RouteHandle } from "../shell/Layout";
import { RootError } from "../shell/RootError";

export type { RouteHandle } from "../shell/Layout";

/** handle builds a typed route handle. */
function handle(section: RouteHandle["section"], title: string): RouteHandle {
  return { section, title };
}

/**
 * routes are the SPA's routes: one layout route around every page. The pages
 * sit under one pathless route whose error element replaces only the page, so
 * the shell stays mounted; the layout route's static RootError catches errors
 * in the shell itself. The findings/ and documents/ segments leave room for
 * sibling sub-pages (/cve/stacks, ...) that cannot clash with ids.
 */
export const routes: RouteObject[] = [
  {
    path: "/",
    element: <Layout />,
    errorElement: <RootError />,
    children: [
      {
        errorElement: <ErrorPanel />,
        children: [
          { index: true, element: <Placeholder />, handle: handle("Operate", "Dashboard") },
          {
            path: "investigations",
            element: <Placeholder />,
            handle: handle("Operate", "Investigations"),
          },
          {
            path: "investigations/:investigationId",
            element: <DetailPlaceholder param="investigationId" />,
            handle: handle("Operate", "Investigation"),
          },
          { path: "cve", element: <Placeholder />, handle: handle("Operate", "CVE") },
          {
            path: "cve/findings/:findingId",
            element: <DetailPlaceholder param="findingId" />,
            handle: handle("Operate", "CVE finding"),
          },
          { path: "knowledge", element: <Placeholder />, handle: handle("Operate", "Knowledge") },
          {
            path: "knowledge/documents/:documentId",
            element: <DetailPlaceholder param="documentId" />,
            handle: handle("Operate", "Document"),
          },
          { path: "projects", element: <Placeholder />, handle: handle("Configure", "Projects") },
          { path: "settings", element: <Placeholder />, handle: handle("Configure", "Settings") },
          { path: "*", element: <NotFound />, handle: handle(null, "Page not found") },
        ],
      },
    ],
  },
];

/** router is the browser router the app renders. */
export const router = createBrowserRouter(routes);
