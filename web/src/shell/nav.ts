// The Sidebar navigation, shared by the Sidebar and its tests. Labels, order
// and icon paths follow design/ui/Sidebar.dc.html.

/** NavItem is one Sidebar link; icon is the SVG path data of a 24×24 icon. */
export type NavItem = { key: string; label: string; to: string; icon: string };

/** NavGroup is a titled group of Sidebar links. */
export type NavGroup = { head: "Operate" | "Configure"; items: NavItem[] };

/** navGroups lists the Sidebar groups and links in display order. */
export const navGroups: NavGroup[] = [
  {
    head: "Operate",
    items: [
      {
        key: "dashboard",
        label: "Dashboard",
        to: "/",
        icon: "M3 3h7v7H3zM14 3h7v7h-7zM14 14h7v7h-7zM3 14h7v7H3z",
      },
      {
        key: "investigations",
        label: "Investigations",
        to: "/investigations",
        icon: "M11 3a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM21 21l-4.3-4.3",
      },
      {
        key: "cve",
        label: "CVE",
        to: "/cve",
        icon: "M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10zM12 8v4M12 16h.01",
      },
      {
        key: "knowledge",
        label: "Knowledge",
        to: "/knowledge",
        icon: "M2 4h7a3 3 0 0 1 3 3v13a2 2 0 0 0-2-2H2zM22 4h-7a3 3 0 0 0-3 3v13a2 2 0 0 1 2-2h8z",
      },
    ],
  },
  {
    head: "Configure",
    items: [
      {
        key: "projects",
        label: "Projects",
        to: "/projects",
        icon: "M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z",
      },
      {
        key: "settings",
        label: "Settings",
        to: "/settings",
        icon: "M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6",
      },
    ],
  },
];
