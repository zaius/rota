import { useQuery, useQueryClient, type QueryKey } from "@tanstack/react-query"

/**
 * useResourceQuery wraps a list fetch in react-query, replacing the
 * hand-rolled `useState(loading) + useEffect + useCallback(load)` boilerplate
 * that every list page repeated. Because the cache is keyed, the same resource
 * fetched on multiple pages (e.g. pools on both the pools and users pages) is
 * deduped, and mutations refetch via `invalidate()` instead of a manual reload.
 */
export function useResourceQuery<T>(
  key: QueryKey,
  fetcher: () => Promise<T>,
  options?: { refetchInterval?: number; enabled?: boolean },
) {
  const qc = useQueryClient()
  const query = useQuery({
    queryKey: key,
    queryFn: fetcher,
    refetchInterval: options?.refetchInterval,
    enabled: options?.enabled,
  })

  return {
    data: query.data,
    isLoading: query.isLoading,
    isError: query.isError,
    error: query.error,
    refetch: query.refetch,
    invalidate: () => qc.invalidateQueries({ queryKey: key }),
  }
}
