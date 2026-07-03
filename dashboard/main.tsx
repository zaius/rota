import React from "react"
import ReactDOM from "react-dom/client"
import { RouterProvider } from "react-router-dom"

// Self-hosted Fira Code (replaces next/font). The weights match the previous
// next/font configuration.
import "@fontsource/fira-code/300.css"
import "@fontsource/fira-code/400.css"
import "@fontsource/fira-code/500.css"
import "@fontsource/fira-code/600.css"
import "@fontsource/fira-code/700.css"

import "@/app/globals.css"
import { router } from "@/router"

ReactDOM.createRoot(document.getElementById("root")!).render(
  <React.StrictMode>
    <RouterProvider router={router} />
  </React.StrictMode>,
)
