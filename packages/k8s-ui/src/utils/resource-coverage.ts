import type { ResourceCoverage } from '../types/resource-coverage'

export function resourceCoverageNotice(coverage: ResourceCoverage | undefined): string | undefined {
  if (coverage?.status !== 'partial') return undefined
  const read = coverage.read
  if (read.type !== 'namespaces' || !read.namespaces) return 'Partial results. Radar could not read the whole requested scope.'
  const { names, total } = read.namespaces
  const preview = names.slice(0, 3).join(', ')
  const extra = total - Math.min(names.length, 3)
  const scope = preview ? `${preview}${extra > 0 ? ` and ${extra} more` : ''}` : `${total} namespaces`
  const reasons = new Set(coverage.gaps.map(gap => gap.reason))
  const gap = reasons.has('rbac_denied') ? 'your permissions exclude some requested namespaces'
    : reasons.has('kind_sync_failed') ? 'some requested namespaces failed to load'
    : reasons.has('collector_access_denied') ? "Radar's collector could not read some requested namespaces"
    : reasons.has('permission_check_failed') ? 'access to some requested namespaces could not be verified'
    : reasons.has('kind_sync_pending') ? 'some requested namespaces are still loading'
    : 'other requested namespaces were not covered'
  return `Partial results. Radar read ${scope}; ${gap}.`
}

export function resourceEmptyMessage(kind: string, coverage: ResourceCoverage | undefined, hasRows: boolean): string {
  if (hasRows) return `No ${kind} match the current filters`
  if (coverage?.status === 'complete') return `No ${kind} found in the requested scope`
  if (coverage?.status === 'partial') return `No ${kind} found in the namespaces Radar could read`
  return `No ${kind} returned. This Radar does not report read coverage.`
}

export function isConfirmedEmpty(count: number | null | undefined, coverage: ResourceCoverage | undefined): boolean {
  return count === 0 && coverage?.status === 'complete'
}

export function resourceCountLabel(count: number, coverage: ResourceCoverage | undefined): string {
  return `${coverage?.status === 'partial' ? '≥' : ''}${count.toLocaleString()}`
}
