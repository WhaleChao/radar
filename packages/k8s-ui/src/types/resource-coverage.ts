export interface ReadNamespaceSet {
  names: string[]
  total: number
  omitted: number
}

export interface ResourceReadScope {
  type: 'cluster' | 'all_namespaces' | 'namespaces'
  namespaces?: ReadNamespaceSet
}

export interface ResourceReadGap {
  reason: string
  namespaces?: ReadNamespaceSet
}

export interface ResourceReadCoverage {
  status: 'complete' | 'partial'
  requested: ResourceReadScope
  read: ResourceReadScope
  gaps: ResourceReadGap[]
}

export type ResourceCoverage = ResourceReadCoverage | { status: 'unknown' }

export interface ResourceListResponse<T> {
  items: T[]
  coverage: ResourceReadCoverage
  kind: string
  group: string
  columns?: import('../components/resources/printer-columns').PrinterColumnDef[] | null
  cells?: Record<string, unknown[]> | null
}
