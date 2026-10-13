import { useResources } from './client'

// useNamespaceQuotas fetches a namespace's ResourceQuota objects via
// /api/resources/resourcequotas?namespace=<ns> (a bare array). Backs the
// NamespaceRenderer quota-usage section — quota saturation is otherwise
// surfaced nowhere in the UI, yet it's exactly why a namespace stops
// admitting new pods.
export function useNamespaceQuotas(namespace: string, enabled = true) {
  return useResources<any>('resourcequotas', namespace, undefined, { enabled: enabled && !!namespace })
}

export function useNamespaceLimitRanges(namespace: string, enabled = true) {
  return useResources<any>('limitranges', namespace, undefined, { enabled: enabled && !!namespace })
}

export function podLimitRangeNames(limitRanges: any[] | undefined): string[] {
  return (limitRanges ?? [])
    .filter(lr => lr.spec.limits?.some((entry: { type: string }) => entry.type === 'Container' || entry.type === 'Pod'))
    .map(lr => lr.metadata.name)
}
