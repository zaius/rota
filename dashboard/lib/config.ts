// PROXY_PORT is the port the proxy listens on, shown in copyable proxy URLs.
// It mirrors the core's PROXY_PORT and defaults to the stack's 8000; set
// VITE_PROXY_PORT at build time when the proxy listens elsewhere.
export const PROXY_PORT: string = import.meta.env.VITE_PROXY_PORT || "8000"

// API_ORIGIN is where the core API answers, for commands shown to copy. The
// Go server serves the dashboard and the API from one origin unless
// VITE_API_URL points the dashboard elsewhere.
export const API_ORIGIN = (): string => import.meta.env.VITE_API_URL || window.location.origin
