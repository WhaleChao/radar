import {
  type IssueRecentChange,
  displayKind,
  englishPlural,
  kindToPlural,
} from "@skyhook-io/k8s-ui";
import { apiVersionToGroup } from "../../../../utils/navigation";
import {
  ProjectionBuilder,
  addChanges,
  addEvents,
  addNarrowHint,
  addResourceObservation,
  contextFrom,
  invalidPayload,
  relevanceForResource,
  scopeFromArgs,
} from "../observations";
import {
  event,
  kubernetesResource,
  nonEmptyString,
  parseJSON,
  recentChange,
  record,
  resourceSummary,
  stringArray,
} from "../parse";
import {
  type InvestigationEventEvidence,
  type InvestigationEvidenceSource,
  type InvestigationResourceSummary,
} from "../types";

export function adaptGetResource(
  builder: ProjectionBuilder,
  source: InvestigationEvidenceSource,
  payload: unknown,
): void {
  // Current producer modes are discriminated by the resource's own identity,
  // never by guessing an undocumented legacy wrapper:
  //   1. bare Kubernetes resource
  //   2. {resource, resourceContext, warnings}
  //   3. the same wrapper with requested extras.
  // Core Secrets use the producer's deliberately value-free detail shape,
  // normalized by kubernetesResource above.
  const bare = kubernetesResource(payload);
  const wrapper = record(payload);
  const resource = bare ?? kubernetesResource(wrapper?.resource);
  if (!resource) {
    invalidPayload(builder, source);
    return;
  }
  const value = bare ? undefined : wrapper;
  const context = value ? contextFrom(value.resourceContext) : undefined;
  if (value?.resourceContext !== undefined && !context) {
    invalidPayload(builder, source, "Resource context");
  }
  const warnings = stringArray(value?.warnings) ?? [];
  const relevance = relevanceForResource(builder, {
    kind: resource.kind,
    group: apiVersionToGroup(resource.apiVersion),
    namespace: resource.metadata.namespace,
    name: resource.metadata.name,
  });
  addResourceObservation(
    builder,
    source,
    resource,
    context,
    warnings,
    undefined,
    false,
    relevance,
  );
  if (!value) return;
  addNarrowHint(builder, source, value);

  const errors: Array<[string, string]> = [
    ["eventsError", "Events"],
    ["recentChangesError", "Recent changes"],
    ["metricsError", "Metrics"],
    ["revisionsError", "Revisions"],
    ["includeError", "Requested include"],
  ];
  for (const [field, label] of errors) {
    if (nonEmptyString(value[field])) {
      builder.limit(source, label, value[field] as string, "error");
    }
  }

  if (Array.isArray(value.events)) {
    const events = value.events
      .map(event)
      .filter((item): item is InvestigationEventEvidence => Boolean(item));
    if (events.length === value.events.length) {
      addEvents(
        builder,
        source,
        events,
        `events:get-resource:${scopeFromArgs(source)}`,
        (typeof value.eventsTotalGroups !== "number" ||
          value.eventsTotalGroups <= events.length) &&
          !nonEmptyString(value.eventsError),
        true,
        relevance,
      );
      if (
        typeof value.eventsTotalGroups === "number" &&
        value.eventsTotalGroups > events.length
      ) {
        builder.limit(
          source,
          "Events",
          `Radar received ${events.length} of ${value.eventsTotalGroups} event groups.`,
          "truncated",
        );
      }
    } else {
      invalidPayload(builder, source, "Events");
    }
  }
  const resourceChangesSubject = {
    kind: resource.kind,
    namespace: resource.metadata.namespace,
    name: resource.metadata.name,
  };
  const recentChangesRaw = value.recentChanges;
  const recentChangesSaturatedRaw = value.recentChangesSaturated;
  const recentChangesCoverageLimitedRaw = value.recentChangesCoverageLimited;
  const hasRecentChangesResult =
    recentChangesRaw !== undefined ||
    recentChangesSaturatedRaw !== undefined ||
    recentChangesCoverageLimitedRaw !== undefined;
  const recentChangesMetadataValid =
    typeof recentChangesSaturatedRaw === "boolean" &&
    typeof recentChangesCoverageLimitedRaw === "boolean";
  if (hasRecentChangesResult && !recentChangesMetadataValid) {
    invalidPayload(builder, source, "Recent changes");
  }
  if (Array.isArray(recentChangesRaw)) {
    const changes = recentChangesRaw
      .map(recentChange)
      .filter((item): item is IssueRecentChange => Boolean(item));
    if (changes.length === recentChangesRaw.length) {
      addChanges(
        builder,
        source,
        changes,
        `changes:get-resource:${scopeFromArgs(source)}`,
        undefined,
        recentChangesMetadataValid &&
          recentChangesSaturatedRaw === false &&
          recentChangesCoverageLimitedRaw === false &&
          !nonEmptyString(value.recentChangesError),
        true,
        relevance,
        resourceChangesSubject,
      );
    } else {
      invalidPayload(builder, source, "Recent changes");
    }
  } else if (
    recentChangesRaw === undefined &&
    recentChangesMetadataValid &&
    !nonEmptyString(value.recentChangesError)
  ) {
    addChanges(
      builder,
      source,
      [],
      `changes:get-resource:${scopeFromArgs(source)}`,
      undefined,
      recentChangesSaturatedRaw === false &&
        recentChangesCoverageLimitedRaw === false,
      true,
      relevance,
      resourceChangesSubject,
    );
  } else if (recentChangesRaw !== undefined) {
    invalidPayload(builder, source, "Recent changes");
  }
  if (recentChangesSaturatedRaw === true) {
    builder.limit(
      source,
      "Recent changes",
      "The recent-change result limit was reached; additional changes may exist in the requested window.",
      "truncated",
    );
  }
  if (recentChangesCoverageLimitedRaw === true) {
    builder.limit(
      source,
      "Recent changes",
      `Change history for ${scopeFromArgs(source)} is incomplete.`,
      "unknown",
      "history",
    );
  }
}
// A namespace listing is an inventory like any other; its entries carry no
// kind of their own, so the card gets one.
export function adaptListNamespaces(
  builder: ProjectionBuilder,
  source: InvestigationEvidenceSource,
  payload: unknown,
): void {
  if (!Array.isArray(payload)) {
    invalidPayload(builder, source);
    return;
  }
  adaptListResources(
    builder,
    source,
    payload.map((entry) => {
      const item = record(entry);
      return item
        ? {
            kind: "Namespace",
            name: item.name,
            status: item.status,
            terminating: item.status === "Terminating",
          }
        : entry;
    }),
  );
}

// A list Radar's collector could read only in some namespaces arrives as
// `{coverage, items}`, naming the namespaces it read; a complete list is the
// bare array.
function listedRows(
  payload: unknown,
):
  | {
      rows: unknown[];
      coverage?: {
        namespacesRead: string[];
        namespacesTotal: number;
        namespacesOmitted: number;
        note: string;
      };
    }
  | undefined {
  if (Array.isArray(payload)) return { rows: payload };
  const value = record(payload);
  const coverage = record(value?.coverage);
  const read = record(coverage?.read);
  const namespaceSet = record(read?.namespaces);
  const namespacesRead = stringArray(namespaceSet?.names);
  const total = namespaceSet?.total;
  const omitted = namespaceSet?.omitted;
  if (
    !value ||
    !Array.isArray(value.items) ||
    coverage?.status !== "partial" ||
    read?.type !== "namespaces" ||
    !namespacesRead?.length ||
    typeof total !== "number" ||
    !Number.isInteger(total) ||
    typeof omitted !== "number" ||
    !Number.isInteger(omitted) ||
    omitted < 0 ||
    total !== namespacesRead.length + omitted ||
    !nonEmptyString(coverage.note)
  ) {
    return undefined;
  }
  return {
    rows: value.items,
    coverage: {
      namespacesRead,
      namespacesTotal: total,
      namespacesOmitted: omitted,
      note: coverage.note,
    },
  };
}

export function adaptListResources(
  builder: ProjectionBuilder,
  source: InvestigationEvidenceSource,
  payload: unknown,
  options: { title?: string } = {},
): void {
  const listed = listedRows(payload);
  if (!listed) {
    invalidPayload(builder, source);
    return;
  }
  const { rows, coverage } = listed;
  const namespacesRead = coverage?.namespacesRead;
  const resources = rows
    .map(resourceSummary)
    .filter((item): item is InvestigationResourceSummary => Boolean(item));
  if (resources.length !== rows.length) {
    invalidPayload(builder, source);
    return;
  }
  const scope = scopeFromArgs(source);
  const args = record(parseJSON(source.args ?? ""));
  const kinds = [...new Set(resources.map((resource) => resource.kind))];
  const noun =
    kinds.length === 1
      ? kindToPlural(kinds[0]) === kinds[0].toLowerCase()
        ? displayKind(kinds[0])
        : englishPlural(displayKind(kinds[0]))
      : "Resources";
  const namespace = nonEmptyString(args?.namespace)
    ? args.namespace
    : undefined;
  const title = options.title ?? (namespace ? `${noun} in ${namespace}` : noun);
  if (resources.length === 0) {
    // list_resources intentionally returns [] for some RBAC-filtered reads;
    // even a successful transport outcome therefore cannot prove absence.
    // The requested kind may be an alias ("svc"), so a partial read's copy
    // is the server's note, which names the resource it resolved.
    builder.limit(
      source,
      "Resource inventory",
      coverage
        ? `The listing returned nothing. ${coverage.note}`
        : `Radar found no matching resources for ${scope}. Anything you do not have permission to read was not searched.`,
      "unknown",
    );
    return;
  }
  const hasAdverseResource = resources.some((resource) => {
    const health = resource.summaryContext?.health?.toLowerCase();
    return (
      health === "unhealthy" ||
      health === "degraded" ||
      (resource.summaryContext?.issueCount ?? 0) > 0 ||
      nonEmptyString(resource.issue)
    );
  });
  // Listings of different things share an argument shape (a cluster-wide
  // package list and a cluster-wide Helm list are both "{}"), so the tool is
  // part of what makes one listing the same card as an earlier read.
  builder.observe(
    `inventory:${source.tool}:${source.args ?? scope}`,
    "inventory",
    source,
    {
      tier: "context",
      relevance: "broader",
      tone: hasAdverseResource ? "warning" : "neutral",
      title,
      summary: [
        `${resources.length} returned`,
        nonEmptyString(args?.group) ? args.group : undefined,
        namespacesRead
          ? `only ${coverage!.namespacesTotal > 2 ? `${coverage!.namespacesTotal} namespaces` : namespacesRead.join(", ")} read`
          : undefined,
      ]
        .filter(Boolean)
        .join(" · "),
      data: namespacesRead
        ? {
            type: "inventory",
            resources,
            scope,
            namespacesRead,
            namespacesReadOmitted: coverage!.namespacesOmitted,
          }
        : { type: "inventory", resources, scope },
    },
  );
}
