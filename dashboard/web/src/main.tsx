import "@fontsource-variable/inter/index.css";
import "./index.css";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import App from "./App";

// retry: 1 (not the v5 default of 3 with exponential backoff): dashboard reads
// either fail definitively (server down) or self-heal on one retry — three
// retries just holds the auth gate on a spinner for ~7s before showing the
// error. Every query inherits this unless it overrides.
const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1 } },
});

const rootEl = document.getElementById("root");
if (!rootEl) {
  throw new Error("index.html is missing the #root element");
}

createRoot(rootEl).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
);
