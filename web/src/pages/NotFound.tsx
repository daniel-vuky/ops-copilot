import { Link } from "react-router";

/** NotFound is the page for a path no route matches. */
export function NotFound() {
  return (
    <section className="glass oc-page">
      <p className="card-b">
        No screen lives at this address.{" "}
        <Link className="link" to="/">
          Go to the dashboard
        </Link>
      </p>
    </section>
  );
}
