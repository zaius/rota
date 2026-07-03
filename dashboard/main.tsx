import React from "react"
import ReactDOM from "react-dom/client"
import { RouterProvider } from "react-router-dom"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"

// Self-hosted Fira Code (replaces next/font). The weights match the previous
// next/font configuration.
import "@fontsource/fira-code/300.css"
import "@fontsource/fira-code/400.css"
import "@fontsource/fira-code/500.css"
import "@fontsource/fira-code/600.css"
import "@fontsource/fira-code/700.css"

import "@/app/globals.css"
import { router } from "@/router"

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      refetchOnWindowFocus: false,
      retry: 1,
    },
  },
})

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </React.StrictMode>,
)
