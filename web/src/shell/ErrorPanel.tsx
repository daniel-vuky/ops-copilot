/**
 * ErrorPanel replaces the page when a route fails to render. It never shows
 * the error itself: messages can carry alert or tool text (ADR 0002 §6).
 */
export function ErrorPanel() {
  return (
    <section className="glass" aria-labelledby="oc-error-h">
      <div className="state">
        <p className="state-title" id="oc-error-h">
          Something went wrong
        </p>
        <p className="state-body">
          This screen could not be shown. Reload the page or pick another screen.
        </p>
      </div>
    </section>
  );
}
