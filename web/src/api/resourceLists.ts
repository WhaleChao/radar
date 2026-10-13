import { sanitizePrinterTable, type PrinterTable } from '@skyhook-io/k8s-ui/components/resources/printer-columns'
import type { ResourceCoverage, ResourceListResponse } from '@skyhook-io/k8s-ui/types/resource-coverage'

export interface ResourceListResult<T> {
  items: T[]
  coverage: ResourceCoverage
  printerTable: PrinterTable | null
}

export function normalizeResourceList<T>(body: ResourceListResponse<T> | T[] | (PrinterTable & { items: T[] }), supportsCoverage: boolean, table: boolean): ResourceListResult<T> {
  if (supportsCoverage) {
    const result = body as ResourceListResponse<T>
    return { items: result.items, coverage: result.coverage, printerTable: table ? sanitizePrinterTable(result) : null }
  }
  // The older instances Hub supports may predate printer-column responses.
  if (!table || Array.isArray(body)) return { items: body as T[], coverage: { status: 'unknown' }, printerTable: null }
  const result = body as PrinterTable & { items: T[] }
  return { items: result.items as T[], coverage: { status: 'unknown' }, printerTable: sanitizePrinterTable(result) }
}
