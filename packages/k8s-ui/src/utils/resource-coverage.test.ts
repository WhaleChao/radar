import { describe, expect, it } from 'vitest'
import type { ResourceReadCoverage } from '../types/resource-coverage'
import { resourceCoverageNotice, resourceEmptyMessage, resourceCountLabel, isConfirmedEmpty } from './resource-coverage'

const partial: ResourceReadCoverage = {
  status: 'partial', requested: { type: 'all_namespaces' },
  read: { type: 'namespaces', namespaces: { names: ['alpha', 'beta', 'gamma', 'delta'], total: 1000, omitted: 996 } },
  gaps: [{ reason: 'collector_scope_limited' }],
}
const complete: ResourceReadCoverage = { status: 'complete', gaps: [], requested: { type: 'all_namespaces' }, read: { type: 'all_namespaces' } }

describe('resource coverage presentation', () => {
  it('keeps large namespace lists bounded while showing the true scope size', () => {
    expect(resourceCoverageNotice(partial)).toContain('alpha, beta, gamma and 997 more')
    expect(resourceCoverageNotice(partial)).not.toContain('delta')
  })
  it('explains loading and collector denial without calling either absence', () => {
    expect(resourceCoverageNotice({ ...partial, gaps: [{ reason: 'kind_sync_pending' }] })).toContain('still loading')
    expect(resourceCoverageNotice({ ...partial, gaps: [{ reason: 'collector_access_denied' }] })).toContain("Radar's collector could not read")
  })
  it('does not warn on positive rows from an older instance', () => {
    expect(resourceCoverageNotice({ status: 'unknown' })).toBeUndefined()
  })
  it('distinguishes complete, partial, unknown and filtered-empty results', () => {
    expect(resourceEmptyMessage('Pod', complete, false)).toBe('No Pod found in the requested scope')
    expect(resourceEmptyMessage('Pod', partial, false)).toContain('namespaces Radar could read')
    expect(resourceEmptyMessage('Pod', { status: 'unknown' }, false)).toContain('Update this cluster’s Radar')
    expect(resourceEmptyMessage('Pod', partial, true)).toContain('match the current filters')
  })
  it('retains partial numeric counts without treating zero as absence', () => {
    expect(resourceCountLabel(23, partial)).toBe('≥23')
    expect(resourceCountLabel(0, partial)).toBe('–')
    expect(resourceCountLabel(0, { status: 'unknown' })).toBe('–')
    expect(resourceCountLabel(23, { status: 'unknown' })).toBe('≥23')
    expect(resourceCountLabel(23, complete)).toBe('23')
    expect(isConfirmedEmpty(0, partial)).toBe(false)
    expect(isConfirmedEmpty(0, undefined)).toBe(false)
    expect(isConfirmedEmpty(0, complete)).toBe(true)
  })
})
