import type { ResourceCoverage } from '../types/resource-coverage'

// gapPhrases says why requested namespaces were not read, most actionable first.
const gapPhrases: [string, string][] = [
  ['kind_sync_pending', 'some requested namespaces are still loading'],
  ['permission_check_failed', 'access to some requested namespaces could not be verified'],
  ['kind_watch_failed', "Radar's watch for this kind failed to start"],
  ['rbac_denied', 'your permissions exclude some requested namespaces'],
  ['namespace_not_visible', "some requested namespaces aren't visible to you"],
  ['outside_namespace_scope', 'Radar is pinned to another namespace'],
  ['collector_forbidden', "Radar's collector could not read some requested namespaces"],
  ['kind_sync_failed', "some requested namespaces haven't finished loading"],
  ['collector_scope_limited', "Radar doesn't collect this kind in some requested namespaces"],
]

export function resourceCoverageNotice(coverage: ResourceCoverage | undefined): string | undefined {
  if (coverage?.status !== 'partial') return undefined
  const read = coverage.read
  if (read?.type !== 'namespaces' || !read.namespaces) return 'Partial results. Radar could not read the whole requested scope.'
  const { sample, count } = read.namespaces
  const preview = sample.slice(0, 3).join(', ')
  const extra = count - Math.min(sample.length, 3)
  const scope = preview ? `${preview}${extra > 0 ? ` and ${extra} more` : ''}` : `${count} namespaces`
  const codes = new Set(coverage.gaps.map(gap => gap.code))
  const gap = gapPhrases.find(([code]) => codes.has(code))?.[1] ?? 'other requested namespaces were not read'
  return `Partial results. Radar read ${scope}; ${gap}.`
}

export function resourceEmptyMessage(kind: string, coverage: ResourceCoverage | undefined, hasRows: boolean): string {
  if (hasRows) return `No ${kind} match the current filters`
  if (coverage?.status === 'complete') return `No ${kind} found in the requested scope`
  if (coverage?.status === 'partial') return `No ${kind} found in the namespaces Radar could read`
  return `No ${kind} returned. Update this cluster’s Radar to verify that the requested scope was fully read.`
}

export function isConfirmedEmpty(count: number | null | undefined, coverage: ResourceCoverage | undefined): boolean {
  return count === 0 && coverage?.status === 'complete'
}

export function resourceCountLabel(count: number, coverage: ResourceCoverage | undefined): string {
  if (count === 0 && coverage?.status !== 'complete') return '–'
  return `${coverage?.status !== 'complete' ? '≥' : ''}${count.toLocaleString()}`
}
