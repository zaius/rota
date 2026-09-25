import * as React from "react"
import { useSearchParams } from "react-router-dom"

type SetOptions = { replace?: boolean }

/**
 * useUrlState keeps a page's filters, sorting and paging in the query string,
 * so a view is a shareable link, survives a reload, and works with the back
 * button. It leaves out values equal to their default to keep URLs short.
 */
export function useUrlState<T extends Record<string, string>>(defaults: T) {
  const [searchParams, setSearchParams] = useSearchParams()

  const state = React.useMemo(() => {
    const out: Record<string, string> = { ...defaults }
    for (const key of Object.keys(defaults)) {
      const v = searchParams.get(key)
      if (v !== null) out[key] = v
    }
    return out as T
  }, [searchParams, defaults])

  const set = React.useCallback(
    (patch: Partial<T>, opts?: SetOptions) => {
      setSearchParams(prev => {
        const next = new URLSearchParams(prev)
        for (const [key, value] of Object.entries(patch)) {
          if (value === undefined || value === defaults[key]) next.delete(key)
          else next.set(key, value)
        }
        return next
      }, { replace: opts?.replace })
    },
    [setSearchParams, defaults],
  )

  return [state, set] as const
}
