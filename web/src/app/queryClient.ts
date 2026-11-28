import { QueryClient } from "@tanstack/react-query";

/** queryClient is the one TanStack Query cache of the SPA (ADR 0001 §13). */
export const queryClient = new QueryClient({
  defaultOptions: {
    queries: { retry: 1, refetchOnWindowFocus: false },
  },
});
