import type { Ref } from "react";
import { Form } from "react-router";
import type { InvestigatorMode } from "./placeholder";

/** modeLabel is the mode chip text for an investigator mode. */
export function modeLabel(mode: InvestigatorMode): string {
  return mode === "api" ? "Production · api" : "POC · cli";
}

/** TopbarProps holds what the Topbar shows; section is null on pages outside the nav. */
export type TopbarProps = {
  section: string | null;
  title: string;
  mode: InvestigatorMode;
  timeZoneLabel: string;
  user: { name: string; initials: string };
  /** titleRef receives the h1, which takes focus after navigation. */
  titleRef?: Ref<HTMLHeadingElement>;
};

/**
 * Topbar is the page header (design/ui/Topbar.dc.html): the crumb and title of
 * the current route, the document search, the mode chip, the time-zone note
 * and the signed-in user.
 */
export function Topbar({ section, title, mode, timeZoneLabel, user, titleRef }: TopbarProps) {
  return (
    <header className="glass topbar oc-topbar">
      <div className="oc-topbar-l">
        {section !== null && <p className="crumb oc-crumb">{section}</p>}
        <h1 className="h1 oc-page-title" ref={titleRef} tabIndex={-1}>
          {title}
        </h1>
      </div>
      <div className="topbar-r">
        <Form
          className="search"
          role="search"
          aria-label="Site search"
          method="get"
          action="/knowledge"
        >
          <svg
            width="16"
            height="16"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
          >
            <path d="M11 3a8 8 0 1 0 0 16 8 8 0 0 0 0-16zM21 21l-4.3-4.3"></path>
          </svg>
          <input
            type="search"
            name="q"
            aria-label="Search documents"
            placeholder="Search documents…"
          />
        </Form>
        <span className="mode">
          <span className="sr">Environment and investigator mode: </span>
          {modeLabel(mode)}
        </span>
        <span className="muted small mono">
          <span className="sr">All times are in </span>
          {timeZoneLabel}
        </span>
        <span className="avatar" aria-hidden="true">
          {user.initials}
        </span>
        <span className="sr">{`Signed in as ${user.name}`}</span>
      </div>
    </header>
  );
}
