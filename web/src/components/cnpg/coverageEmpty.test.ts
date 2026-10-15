import { describe, expect, it } from 'vitest'
import { coverageEmpty, partialLabel } from './shared'

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

describe('partialLabel', () => {
  it("names Radar's collection, not the caller, for uncollected namespaces", () => {
    expect(partialLabel({ deniedNamespaces: [], uncoveredNamespaces: ['beta'] })).toBe("Radar doesn't collect some namespaces")
  })
  it("names the caller's access only when Radar says the rest is collected", () => {
    expect(partialLabel({ deniedNamespaces: ['beta'], uncoveredNamespaces: [] })).toBe('no access in some namespaces')
    expect(partialLabel({ deniedNamespaces: ['beta'], uncoveredNamespaces: ['gamma'] })).toBe("no access in some namespaces, and Radar doesn't collect others")
  })
  it('blames no one when an older Radar does not separate the two', () => {
    expect(partialLabel({ deniedNamespaces: ['beta'] })).toBe('some namespaces not read')
  })
})
