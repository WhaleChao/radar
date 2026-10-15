import type { ReadNameSet } from '../types/read-coverage'
import type { ResourceCoverage } from '../types/resource-coverage'
import { pluralize } from './pluralize'

// Only a partial answer is qualified. Unknown coverage (an older Radar, or a
// host that passes none) keeps the presentation that predates coverage: plain
// counts, empty kinds hidden, and no notice.

// gapPhrases says why the rest of the scope was not read, most actionable first.
const gapPhrases: [string, (where: string) => string][] = [
  ['kind_sync_pending', where => `some ${where} are still loading`],
  ['permission_check_failed', where => `access to some ${where} could not be verified`],
  ['kind_watch_failed', () => "Radar's watch for this kind failed to start"],
  ['rbac_denied', where => `your permissions exclude some ${where}`],
  ['namespace_not_visible', where => `some ${where} aren't visible to you`],
  ['outside_namespace_scope', () => 'Radar is pinned to another namespace'],
  ['collector_forbidden', where => `Radar's collector could not read some ${where}`],
  ['kind_sync_failed', where => `some ${where} haven't finished loading`],
  ['collector_scope_limited', where => `Radar doesn't collect this kind in some ${where}`],
]

/** The first few names of a set and how many more there are; empty when the names are withheld. */
function namespacePreview({ sample, count }: ReadNameSet): string {
  const preview = sample.slice(0, 3).join(', ')
  const extra = count - Math.min(sample.length, 3)
  return preview && `${preview}${extra > 0 ? ` and ${extra} more` : ''}`
}

export function resourceCoverageNotice(coverage: ResourceCoverage | undefined): string | undefined {
  if (coverage?.status !== 'partial') return undefined
  const read = coverage.read
  if (read?.type !== 'namespaces' || !read.namespaces) return 'Partial results. Radar could not read the whole requested scope.'
  const scope = namespacePreview(read.namespaces) || pluralize(read.namespaces.count, 'namespace')
  // A broad request named no namespaces, so none of them were "requested".
  const where = coverage.scope.type === 'namespaces' && !coverage.narrowedBy?.length ? 'requested namespaces' : 'namespaces'
  const codes = new Set(coverage.gaps.map(gap => gap.code))
  const gap = gapPhrases.find(([code]) => codes.has(code))?.[1](where) ?? `other ${where} were not read`
  return `Partial results. Radar read ${scope}; ${gap}.`
}

/**
 * A complete answer whose scope Radar's own collection narrowed. The other
 * narrowings (the caller's access, Radar's pin, the saved pick) already show
 * in the namespace picker, so they get no note.
 */
export function resourceNarrowedNotice(coverage: ResourceCoverage | undefined): string | undefined {
  if (coverage?.status !== 'complete' || !coverage.narrowedBy?.includes('collection')) return undefined
  const namespaces = coverage.scope.namespaces
  if (!namespaces) return 'Radar collects this kind only in some namespaces.'
  const preview = namespacePreview(namespaces)
  return `Radar collects this kind only in ${pluralize(namespaces.count, 'namespace')}${preview ? `: ${preview}` : ''}.`
}

export function resourceEmptyMessage(kind: string, coverage: ResourceCoverage | undefined, hasRows: boolean): string {
  if (hasRows) return `No ${kind} match the current filters`
  if (coverage?.status === 'complete') return `No ${kind} found in the requested scope`
  if (coverage?.status === 'partial') return `No ${kind} found in the namespaces Radar could read`
  return `No ${kind} found`
}

/** A zero that hides its kind as empty. A partial zero proves nothing about the namespaces Radar couldn't read. */
export function isEmptyCount(count: number | null | undefined, coverage: ResourceCoverage | undefined): boolean {
  return count === 0 && coverage?.status !== 'partial'
}

/** A partial count is a lower bound, and a partial zero is no count at all. */
export function boundedCountLabel(count: number, partial: boolean): string {
  if (!partial) return count.toLocaleString()
  return count === 0 ? '–' : `≥${count.toLocaleString()}`
}

export function resourceCountLabel(count: number, coverage: ResourceCoverage | undefined): string {
  return boundedCountLabel(count, coverage?.status === 'partial')
}
