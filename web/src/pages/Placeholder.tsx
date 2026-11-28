import { useParams } from "react-router";

/** Placeholder stands in for a screen whose ticket has not built it yet. */
export function Placeholder() {
  return (
    <section className="glass oc-page">
      <p className="card-b">This screen is not built yet.</p>
    </section>
  );
}

/** DetailPlaceholderProps names the route parameter that holds the record id. */
export type DetailPlaceholderProps = { param: string };

/** DetailPlaceholder stands in for a detail screen and shows the record id as text. */
export function DetailPlaceholder({ param }: DetailPlaceholderProps) {
  const id = useParams()[param] ?? "";
  return (
    <section className="glass oc-page">
      <p className="card-b">This screen is not built yet.</p>
      <p className="card-b oc-page-id">
        <span className="muted">ID </span>
        <span className="mono">{id}</span>
      </p>
    </section>
  );
}
