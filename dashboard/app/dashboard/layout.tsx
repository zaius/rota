import * as React from "react"
import { Outlet, useNavigate } from "react-router-dom"
import { AppNav } from "@/components/shell/app-nav"
import { api } from "@/lib/api"

export default function DashboardLayout() {
  const navigate = useNavigate()
  const [verified, setVerified] = React.useState(false)

  // Check the stored password on mount. Its presence says nothing about
  // whether it is still right, so ask the API: rendering first and bouncing on
  // the first 401 flashes protected content to a logged-out viewer.
  React.useEffect(() => {
    let cancelled = false

    if (!localStorage.getItem("auth_token")) {
      navigate("/login")
      return
    }

    api
      .checkAuth()
      .then(() => {
        if (!cancelled) setVerified(true)
      })
      .catch(() => {
        if (cancelled) return
        api.clearToken()
        navigate("/login")
      })

    return () => {
      cancelled = true
    }
  }, [navigate])

  if (!verified) {
    return <p className="text-muted-foreground grid min-h-svh place-items-center">Checking session…</p>
  }

  return (
    <div className="md:grid md:min-h-svh md:grid-cols-[14.5rem_1fr]">
      {/* The column carries the background so it spans pages taller than the viewport. */}
      <div className="bg-sidebar border-border relative md:border-r">
        <AppNav />
      </div>
      {/* min-w-0 lets wide tables scroll inside the page instead of stretching it past the viewport. */}
      <main className="flex min-w-0 flex-col">
        <Outlet />
      </main>
    </div>
  )
}
