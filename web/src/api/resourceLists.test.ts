import { describe, expect, it } from 'vitest'
import { normalizeResourceList } from './resourceLists'

const complete = { status: 'complete' as const, gaps: [], scope: { type: 'cluster' as const }, read: { type: 'cluster' as const } }

describe('versioned resource-list responses', () => {
  it('uses the new envelope and retains partial metadata with zero rows', () => {
    const coverage = { ...complete, status: 'partial' as const, gaps: [{ code: 'collector_scope_limited' }] }
    expect(normalizeResourceList({ items: [], kind: 'Pod', group: '', coverage }, true, false)).toEqual({ items: [], coverage, printerTable: null })
  })
  it('marks old array results as unknown rather than complete', () => {
    const rows = [{ metadata: { name: 'api' } }]
    expect(normalizeResourceList(rows, false, false)).toEqual({ items: rows, coverage: { status: 'unknown' }, printerTable: null })
  })
  it('supports older instances predating table mode', () => {
    expect(normalizeResourceList([], false, true).coverage.status).toBe('unknown')
  })
  it('keeps rows and printer cells from the same response in either version', () => {
    const body = { items: [{ metadata: { uid: 'one' } }], kind: 'Widget', group: 'example.io', columns: [{ name: 'State', type: 'string' }], cells: { one: ['Ready'] }, coverage: complete }
    for (const supported of [true, false]) {
      const result = normalizeResourceList(body, supported, true)
      expect(result.items).toEqual(body.items)
      expect(result.printerTable?.cells).toEqual(body.cells)
    }
  })
})
