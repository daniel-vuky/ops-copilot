// Static shell values until their tickets wire them to the server. The mode
// chip and time-zone note come from the investigator mode and DISPLAY_TZ
// (UC-25); the user comes from the session (UC-01).

/** InvestigatorMode is the investigator a deployment runs (ADR 0003). */
export type InvestigatorMode = "cli" | "api";

/** placeholder holds every value the shell shows before it has an API. */
export const placeholder = {
  mode: "cli" as InvestigatorMode,
  timeZoneLabel: "ICT · UTC+7",
  user: { name: "Signed-in user", initials: "SU", role: "Member" },
};
