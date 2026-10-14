import { describe, expect, it } from 'vitest'
import { coverageEmpty } from './shared'

describe('coverageEmpty', () => {
  it("says whose gap a partial read is, when Radar says", () => {
    expect(coverageEmpty({ state: 'partial', deniedNamespaces: ['a'], uncoveredNamespaces: [] }, 'Poolers')).toBe(
      'No Poolers visible. Some namespaces are not readable with your access.',
    )
    expect(coverageEmpty({ state: 'partial', deniedNamespaces: [], uncoveredNamespaces: ['b', 'c'] }, 'Poolers')).toBe(
      "No Poolers visible. Radar doesn't collect Poolers in b, c.",
    )
    expect(coverageEmpty({ state: 'partial', deniedNamespaces: ['a'], uncoveredNamespaces: ['b', 'c', 'd', 'e'] }, 'Poolers')).toBe(
      "No Poolers visible. Some namespaces are not readable with your access, and Radar doesn't collect Poolers in b, c, d and 1 more.",
    )
  })

  it("blames neither for an older Radar's partial read", () => {
    expect(coverageEmpty({ state: 'partial', deniedNamespaces: ['a'] }, 'Poolers')).toBe('No Poolers visible. Some namespaces were not read.')
  })
})
