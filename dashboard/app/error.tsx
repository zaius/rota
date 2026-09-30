import { isRouteErrorResponse, useRouteError } from "react-router-dom"
import { AlertTriangle } from "lucide-react"
import { Button } from "@/components/ui/button"
import NotFound from "@/app/not-found"

// ErrorPage replaces any route whose render threw, so one broken page never
// blanks the whole app.
export default function ErrorPage() {
  const error = useRouteError()
  if (isRouteErrorResponse(error) && error.status === 404) return <NotFound />

  const message = error instanceof Error && error.message ? error.message : "The page threw while rendering."
  return (
    <div className="grid min-h-svh place-items-center px-6 py-16">
      <div className="w-full max-w-md">
        <AlertTriangle className="text-warning size-5" aria-hidden />
        <h1 className="mt-4 text-[1.125rem] font-semibold tracking-tight">Something broke on this page</h1>
        <p className="text-muted-foreground mt-2">
          {message} First check that the core API is reachable, then try again.
        </p>
        <Button variant="outline" className="mt-6" onClick={() => window.location.reload()}>
          Try again
        </Button>
      </div>
    </div>
  )
}
