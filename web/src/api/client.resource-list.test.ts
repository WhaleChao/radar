import { afterEach, describe, expect, it, vi } from 'vitest'
import { fetchResourceList } from './client'
import { setApiBase, setAuthHeadersProvider, setCredentialsMode } from './config'

afterEach(() => {
  vi.unstubAllGlobals()
  setApiBase('/api')
  setAuthHeadersProvider(() => ({}))
  setCredentialsMode('same-origin')
})

const coverage = { status: 'complete', requested: { type: 'all_namespaces' }, read: { type: 'all_namespaces' }, gaps: [] }
const envelope = { items: [], kind: 'Pod', group: '', coverage }

describe('resource list transport', () => {
  it('uses the configured cluster route and auth with a bounded new list', async () => {
    setApiBase('/c/cluster/api')
    setAuthHeadersProvider(() => ({ Authorization: 'Bearer test' }))
    setCredentialsMode('include')
    const mock = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      expect(url).toBe('/c/cluster/api/resources/pods?namespaces=alpha&maxItems=25000')
      expect(init?.credentials).toBe('include')
      expect(new Headers(init?.headers).get('Authorization')).toBe('Bearer test')
      return new Response(JSON.stringify(envelope), { status: 200 })
    })
    vi.stubGlobal('fetch', mock)
    expect((await fetchResourceList('pods', { supportsCoverage: true, namespaces: ['alpha'] })).coverage).toEqual(coverage)
    expect(mock).toHaveBeenCalledTimes(1)
  })
  it('uses the summary budget only for summary requests', async () => {
    vi.stubGlobal('fetch', vi.fn(async (url: RequestInfo | URL) => {
      expect(url).toBe('/api/resources/pods?include=summary&maxItems=50000')
      return new Response(JSON.stringify(envelope), { status: 200 })
    }))
    await fetchResourceList('pods', { supportsCoverage: true, summary: true })
  })
  it('reads old arrays without probing or inventing complete coverage', async () => {
    const mock = vi.fn(async (url: RequestInfo | URL) => {
      expect(url).toBe('/api/resources/pods?')
      return new Response('[]', { status: 200 })
    })
    vi.stubGlobal('fetch', mock)
    expect((await fetchResourceList('pods', { supportsCoverage: false })).coverage).toEqual({ status: 'unknown' })
    expect(mock).toHaveBeenCalledTimes(1)
  })
  it('preserves oversized-list errors instead of returning empty rows', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify({ error: 'Too many resources to load', error_code: 'resource_list_too_large' }), { status: 400, headers: { 'Content-Type': 'application/json' } })))
    await expect(fetchResourceList('pods', { supportsCoverage: true })).rejects.toMatchObject({ status: 400, data: { error_code: 'resource_list_too_large' } })
  })
})
