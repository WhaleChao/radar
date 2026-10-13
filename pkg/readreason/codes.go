// Package readreason names why Radar could not give a complete answer to a
// resource read or a search. The values are wire contracts: MCP list_resources
// and get_resource errors carry one as structuredContent.reason, a partial
// list_resources result as coverage.gaps[].reason, and search coverage gaps as
// unsearched[].reason on REST and MCP. Clients
// branch on them, so a value never changes its spelling. Codes that name the
// caller (rbac_denied) and codes that name Radar's collector (collector_*)
// stay distinct because different grants fix them.
package readreason

// Code is one reason a read or search could not give a complete answer.
type Code string

const (
	// RBACDenied: the caller's own RBAC denies the exact operation requested.
	RBACDenied Code = "rbac_denied"
	// CollectorAccessDenied: Kubernetes refused Radar's own identity (its
	// service account or kubeconfig user); granting the caller access does not
	// help. The evidence may not distinguish a 401 from a 403.
	CollectorAccessDenied Code = "collector_access_denied"
	// CollectorUnauthorized: Kubernetes rejected Radar's own credentials (401).
	CollectorUnauthorized Code = "collector_unauthorized"
	// PermissionCheckFailed: Radar could not complete the caller's permission
	// check; this is not a denial, and a retry can succeed.
	PermissionCheckFailed Code = "permission_check_failed"
	// NoNamespaceAccess: no requested namespace passes Radar's namespace
	// membership check (the caller can list pods or deployments there).
	NoNamespaceAccess Code = "no_namespace_access"
	// KindSyncPending: the kind is still loading; retry shortly.
	KindSyncPending Code = "kind_sync_pending"
	// KindSyncFailed: the kind's initial sync failed or stalled.
	KindSyncFailed Code = "kind_sync_failed"
	// KindNotWatched: Radar has no watch for the kind, and nothing more precise
	// is known.
	KindNotWatched Code = "kind_not_watched"
	// CollectorScopeLimited: the requested scope reaches beyond the namespaces
	// Radar's collector covers.
	CollectorScopeLimited Code = "collector_scope_limited"
	// UnknownKind: neither the built-in catalog nor discovery resolves the kind.
	UnknownKind Code = "unknown_kind"
	// CollectionNotSupported: the API cannot be watched, or Radar reads it
	// directly by policy; warming a watch will not help.
	CollectionNotSupported Code = "collection_not_supported"
	// NamespaceExcluded: caller visibility or a namespace selection left out
	// requested namespaces; it says nothing about whether they exist.
	NamespaceExcluded Code = "namespace_excluded"
	// NamespaceScopeLimited: the saved namespace pick or --namespace-scope
	// narrowed a query that asked for more namespaces.
	NamespaceScopeLimited Code = "namespace_scope_limited"
	// OutsideNamespaceScope: the requested namespace is outside Radar's
	// --namespace-scope pin.
	OutsideNamespaceScope Code = "outside_namespace_scope"
	// NamespaceRequired: a get of a namespaced kind named no namespace.
	NamespaceRequired Code = "namespace_required"
	// ReadError: an unclassified read or discovery failure; the accompanying
	// explanation carries the cause.
	ReadError Code = "read_error"
)
