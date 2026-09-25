import * as React from "react"
import { Outlet, useNavigate } from "react-router-dom"
import { AppNav } from "@/components/shell/app-nav"
import { api } from "@/lib/api"

export default function DashboardLayout() {
  const navigate = useNavigate()
  const [username, setUsername] = React.useState<string | null>(null)
  const [isLoading, setIsLoading] = React.useState(true)

  // Check authentication on mount. The presence of a token says nothing about
  // whether it is still valid, so ask the API: rendering first and bouncing on
  // the first 401 flashes protected content to a logged-out viewer.
  React.useEffect(() => {
    let cancelled = false

    const token = localStorage.getItem("auth_token")
    if (!token) {
      navigate("/login")
      setIsLoading(false)
      return
    }

    api
      .getAdminInfo()
      .then((info) => {
        if (cancelled) return
        setUsername(info.username)
      })
      .catch(() => {
        if (cancelled) return
        api.clearToken()
        navigate("/login")
      })
      .finally(() => {
        if (!cancelled) setIsLoading(false)
      })

    return () => {
      cancelled = true
    }
  }, [navigate])

  if (isLoading) {
    return <p className="text-muted-foreground grid min-h-svh place-items-center">Checking session…</p>
  }

  if (username === null) return null

  return (
    <div className="md:grid md:min-h-svh md:grid-cols-[14.5rem_1fr]">
      {/* The column carries the background so it spans pages taller than the viewport. */}
      <div className="bg-sidebar border-border relative md:border-r">
        <AppNav username={username} />
      </div>
      {/* min-w-0 lets wide tables scroll inside the page instead of stretching it past the viewport. */}
      <main className="flex min-w-0 flex-col">
        <Outlet />
      </main>
    </div>
  )
}
