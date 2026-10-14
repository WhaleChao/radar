// Coverage of a Radar read, as Radar's pkg/resourceapi serializes it. Codes
// come from Radar's pkg/readreason.

/** A known, finite set: `count` members, the first of them (sorted) in `sample`. */
export interface ReadNameSet {
  count: number
  sample: string[]
}

/** A kind; an omitted group is the core group. */
export interface ReadKindRef {
  kind: string
  group?: string
}

export interface ReadObjectRef {
  kind: string
  group?: string
  namespace?: string
  name: string
}

/**
 * Part of an answer Radar did not read, and why. A gap covers every
 * combination of its present dimensions; an absent dimension is not narrowed
 * (the gap may reach any kind or namespace of the answer), never "none".
 */
export interface ReadGap {
  code: string
  kinds?: { count: number; sample: ReadKindRef[] }
  namespaces?: ReadNameSet
  objects?: { count: number; sample: ReadObjectRef[] }
}

/** Why an implicitly broad read covers less than everything. */
export type ReadNarrowing = 'caller_access' | 'namespace_pin' | 'namespace_selection' | 'collection' | (string & {})

/**
 * A search's coverage: `partial` when something the query named couldn't be
 * searched or a read failed; `narrowedBy` when a broad query searched only
 * what the caller's access and Radar's collection allow.
 */
export interface SearchCoverage {
  status: 'complete' | 'partial'
  narrowedBy?: ReadNarrowing[]
  gaps: ReadGap[]
}
