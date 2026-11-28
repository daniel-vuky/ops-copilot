import { useState } from "react";
import { NavLink, useLocation } from "react-router";
import { navGroups } from "./nav";

/** The System panel rows. No health API exists yet, so none shows a state. */
const systemRows = ["Investigator", "Scheduler", "Company MCP", "Secret filter"];

/** SidebarProps holds the signed-in user shown at the foot of the Sidebar. */
export type SidebarProps = {
  user: { name: string; initials: string; role: string };
};

/**
 * Sidebar is the main navigation (design/ui/Sidebar.dc.html): the brand, the
 * Operate and Configure links, the System panel and the user block. On narrow
 * screens the Menu button shows the list; any navigation closes it.
 */
export function Sidebar({ user }: SidebarProps) {
  const location = useLocation();
  // The key of the location the menu was opened at. Every navigation (a nav
  // link, the Topbar search, an in-page link, back or forward) brings a new
  // key, so the menu is open only until the location changes, with no effect.
  const [openedAt, setOpenedAt] = useState<string | null>(null);
  const open = openedAt === location.key;
  const close = () => setOpenedAt(null);

  return (
    <nav className="glass-dark oc-nav" aria-label="Main navigation">
      <div className="oc-brand">
        <span className="oc-brand-mark" aria-hidden="true">
          <span className="oc-brand-sq oc-brand-sq-blue"></span>
          <span className="oc-brand-sq oc-brand-sq-green"></span>
        </span>
        <div className="oc-brand-text">
          <span className="oc-brand-name">Ops Copilot</span>
          <span className="oc-brand-sub">Internal tool</span>
        </div>
        <button
          className="btn btn-sm nav-burger oc-burger"
          type="button"
          aria-expanded={open ? "true" : "false"}
          aria-controls="oc-nav-list"
          onClick={() => setOpenedAt(open ? null : location.key)}
        >
          Menu
        </button>
      </div>

      <div className={open ? "nav-list nav-open" : "nav-list"} id="oc-nav-list">
        {navGroups.map((group) => {
          const headId = `oc-nav-h-${group.head.toLowerCase()}`;
          return (
            <div key={group.head} className="oc-nav-group">
              <p className="nav-sec" id={headId}>
                {group.head}
              </p>
              <ul className="nav-ul" aria-labelledby={headId}>
                {group.items.map((item) => (
                  <li key={item.key}>
                    <NavLink
                      to={item.to}
                      end={item.to === "/"}
                      className={({ isActive }) => (isActive ? "nav-a nav-on" : "nav-a")}
                      onClick={close}
                    >
                      <svg
                        width="18"
                        height="18"
                        viewBox="0 0 24 24"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2"
                        strokeLinecap="round"
                        strokeLinejoin="round"
                        aria-hidden="true"
                      >
                        <path d={item.icon}></path>
                      </svg>
                      <span>{item.label}</span>
                    </NavLink>
                  </li>
                ))}
              </ul>
            </div>
          );
        })}

        <section className="oc-sys" aria-labelledby="oc-sys-h">
          <p className="nav-sec oc-sys-h" id="oc-sys-h">
            System
          </p>
          {systemRows.map((label) => (
            <div key={label} className="oc-sys-row">
              <span className="dot dot-off" aria-hidden="true"></span>
              <span className="oc-sys-label">{label}</span>
              <span className="oc-sys-value">
                <span aria-hidden="true">—</span>
                <span className="sr">not connected yet</span>
              </span>
            </div>
          ))}
        </section>

        <div className="oc-user">
          <span className="avatar" aria-hidden="true">
            {user.initials}
          </span>
          <span className="oc-user-text">
            <span className="oc-user-name">{user.name}</span>
            <span className="oc-user-role">{user.role}</span>
          </span>
          <button className="linkbtn oc-signout" type="button" disabled>
            Sign out
          </button>
        </div>
      </div>
    </nav>
  );
}
