/**
 * RootError is the outer fallback when the shell itself fails to render. It
 * depends on nothing the shell reads (session, mode) and shows no error
 * details (ADR 0002 §6).
 */
export function RootError() {
  return (
    <p className="oc-root-error" role="alert">
      Something went wrong. Reload the page.
    </p>
  );
}
