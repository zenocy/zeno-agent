import React from "react";
import ReactDOM from "react-dom/client";
import {
  MutationCache,
  QueryCache,
  QueryClient,
  QueryClientProvider,
} from "@tanstack/react-query";
import App from "./App";
import { ToastProvider } from "./components/Toast";
import AuthGate from "./auth/AuthGate";
import "./index.css";

// A session expiring mid-use lands as a 401 on whatever data fetch tries
// next. Reload so AuthGate re-runs and the login page comes up — preserves
// the user's URL and avoids each hook having to special-case 401.
let handlingUnauthorized = false;
function on401(error: unknown) {
  if (handlingUnauthorized) return;
  if (
    error instanceof Error &&
    /\b(401|unauthorized)\b/i.test(error.message)
  ) {
    handlingUnauthorized = true;
    window.location.reload();
  }
}

const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: on401 }),
  mutationCache: new MutationCache({ onError: on401 }),
  defaultOptions: {
    queries: {
      staleTime: 30 * 1000,
      refetchOnWindowFocus: false,
    },
  },
});

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <AuthGate>
          <App />
        </AuthGate>
      </ToastProvider>
    </QueryClientProvider>
  </React.StrictMode>
);
