import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type {
  Diagnosis,
  DiagnosisEvidenceItem,
} from "../../../../api/diagnose";
import { ResultCard } from "../../AssessmentCard";
import { InvestigationEvidencePane } from "../../InvestigationEvidencePane";
import { resolveInvestigationCase } from "../../investigationCase";
import { investigationEvidenceCoverageLimited } from "../../investigationState";
import { StoryExcerpt } from "../../StoryExcerpt";
import { namedInventoryRows } from "../bodies/inventory";
import type { InvestigationEvidenceTimelineItem } from "../types";
import { evidenceRef, groupsOf, project, tool } from "../evidenceFixtures";

// The list_resources results a partial read returns over MCP. internal/mcp
// pins them to the producer and internal/ai proves every agent backend hands
// their joined text blocks to Findings unchanged; this suite projects that
// text the way Findings does.
const FIXTURE = resolve(
  dirname(fileURLToPath(import.meta.url)),
  "../../../../../../testdata/mcp/list-resources-partial-coverage.json",
);

interface FixtureCase {
  name: string;
  arguments: Record<string, unknown>;
  result: { content: { type: string; text: string }[] };
}

const cases: FixtureCase[] = JSON.parse(readFileSync(FIXTURE, "utf8")).cases;

function projectCase(
  name: string,
  ref?: string,
  preceding: InvestigationEvidenceTimelineItem[] = [],
) {
  const fixture = cases.find((item) => item.name === name);
  if (!fixture) throw new Error(`no fixture case ${name}`);
  return project([
    ...preceding,
    tool(
      "list",
      "list_resources",
      fixture.result.content.map((block) => block.text).join(""),
      { summary: JSON.stringify(fixture.arguments), evidenceRef: ref },
    ),
  ]);
}

describe("a list Radar's collector read only in some namespaces", () => {
  it("shows the rows it read with the namespaces they came from", () => {
    const projection = projectCase("partial", undefined, [
      tool("diagnose", "diagnose", {
        resource: {
          apiVersion: "apps/v1",
          kind: "Deployment",
          metadata: { name: "api", namespace: "shop" },
        },
        pods: 0,
        events: [],
        recentChanges: [],
      }),
    ]);
    const [inventory] = groupsOf(projection.groups, "inventory");
    if (inventory?.latest.data.type !== "inventory") {
      throw new Error("expected an inventory card");
    }
    expect(
      inventory.latest.data.resources.map((row) => [row.namespace, row.name]),
    ).toEqual([["alpha", "alpha-object"]]);
    expect(inventory.latest.summary).toBe(
      "1 returned · only alpha, gamma read",
    );
    expect(inventory.latest.data.namespacesRead).toEqual(["alpha", "gamma"]);
    expect(projection.limitations).toEqual([]);
    const coverageLimited = investigationEvidenceCoverageLimited(
      projection,
      "Deployment",
    );
    expect(coverageLimited).toBe(false);
    const html = renderToStaticMarkup(
      createElement(ResultCard, {
        diagnosis: {
          healthy: true,
          rootCause: "",
          report: "The workload appears healthy.",
          remediation: [],
        } as Diagnosis,
        coverageLimited,
      }),
    );
    expect(html).toContain("border-emerald-500/30");
    expect(html).not.toContain("No problem identified in available evidence");
    expect(html).not.toContain("Evidence coverage is limited");
  });

  it("does not present an empty partial read as an empty cluster", () => {
    const projection = projectCase("partial and empty");
    expect(groupsOf(projection.groups, "inventory")).toEqual([]);
    expect(projection.limitations.map((item) => item.message)).toEqual([
      'The listing returned nothing. Radar read services only in namespaces "alpha,gamma"; objects in other namespaces were not read, so this result cannot show they don\'t exist.',
    ]);
    expect(investigationEvidenceCoverageLimited(projection, "Service")).toBe(
      true,
    );
  });

  it("keeps omitted namespace details unknown in citations", () => {
    const namespaces = Array.from(
      { length: 40 },
      (_, index) => `team-${String(index + 1).padStart(2, "0")}`,
    );
    const coverage = {
      status: "partial",
      requested: { type: "all_namespaces" },
      read: {
        type: "namespaces",
        namespaces: { names: namespaces.slice(0, 8), total: 40, omitted: 32 },
      },
      gaps: [{ reason: "collector_scope_limited" }],
      note: 'Radar read services only in namespaces "team-01,team-02,team-03 and 37 more"; objects in other namespaces were not read, so this result cannot show they don\'t exist.',
    };
    const rows = [
      { kind: "Service", name: "present", namespace: namespaces[0] },
      { kind: "Service", name: "observed", namespace: "team-39" },
    ];
    const projection = project([
      tool(
        "list",
        "list_resources",
        { coverage, items: rows },
        {
          summary: JSON.stringify({ kind: "services" }),
        },
      ),
    ]);
    const [inventory] = groupsOf(projection.groups, "inventory");
    if (inventory.latest.data.type !== "inventory") {
      throw new Error("expected an inventory card");
    }
    const data = inventory.latest.data;
    expect(data.namespacesRead).toEqual(namespaces.slice(0, 8));
    expect(data.namespacesReadOmitted).toBe(32);
    expect(inventory.latest.summary).toBe(
      "2 returned · only 40 namespaces read",
    );
    const entries = namedInventoryRows(
      data.resources,
      [
        { subject: { kind: "Service", name: "api" } },
        { subject: { kind: "Service", name: "api", namespace: "team-40" } },
        { subject: { kind: "Service", name: "api", namespace: "unread" } },
        { subject: { kind: "Service", name: "api", namespace: "team-39" } },
      ],
      undefined,
      data.namespacesRead,
      data.namespacesReadOmitted,
    );
    expect(entries[0].coverageUnknown).toBe(true);
    expect(entries[1].coverageUnknown).toBe(true);
    expect(entries[2].coverageUnknown).toBe(true);
    expect(entries[2].notRead).toBeUndefined();
    expect(entries[3].coverageUnknown).toBe(false);
    expect(entries[3].notRead).toBeUndefined();
    const html = renderToStaticMarkup(
      createElement(StoryExcerpt, {
        excerpt: { kind: "rows", entries },
        data,
        prominence: "primary",
        open: false,
        canExpand: false,
      }),
    );
    expect(html).toContain(
      "api: read coverage is unknown here; namespace details were abbreviated",
    );
    expect(html).not.toContain("team-04");
    expect(html).not.toContain("Not in this listing: team-40/api");
    expect(html).toContain("Not in this listing: team-39/api");
    expect(html).not.toContain("unread/api: unread was not read");
    expect(html).toContain("unread/api: read coverage is unknown");
    const empty = project([
      tool("empty", "list_resources", { coverage, items: [] }),
    ]);
    expect(empty.limitations.map((item) => item.message)).toEqual([
      `The listing returned nothing. ${coverage.note}`,
    ]);
    expect(empty.limitations[0].message).not.toContain("team-04");
  });

  it("names the resource Radar read, not the alias the agent asked for", () => {
    const projection = projectCase("partial and empty, kind by alias");
    const [message] = projection.limitations.map((item) => item.message);
    expect(message).toContain("Radar read services only in");
    expect(message).not.toMatch(/svcs/i);
  });

  it("says a cited name's namespace was not read instead of calling it absent", () => {
    const ref = evidenceRef("a", "b");
    const projection = projectCase("partial", ref);
    const evidence: DiagnosisEvidenceItem[] = [
      { namespace: "beta", claim: "Service api is missing from beta." },
      { namespace: "gamma", claim: "Service api is missing from gamma." },
      { namespace: undefined, claim: "No Service api exists." },
    ].map(({ namespace, claim }) => ({
      status: "linked",
      ref,
      role: "cause",
      claim,
      subject: { kind: "Service", namespace, name: "api" },
    }));
    const investigationCase = resolveInvestigationCase(
      projection,
      { evidence },
      0,
    );
    expect(investigationCase.items.map((item) => item.placement)).toEqual([
      "card",
      "card",
      "card",
    ]);
    const html = renderToStaticMarkup(
      createElement(InvestigationEvidencePane, {
        projection,
        investigationCase,
        story: {
          report: "The Ingress has no backend.\n\n[[radar:evidence=0]]",
          evidence,
        },
        collecting: false,
        animateGroupIds: new Set<string>(),
        onViewSource: () => {},
        onViewActivity: () => {},
      }),
    );
    const start = html.indexOf("data-story-inventory-rows");
    expect(start).toBeGreaterThan(-1);
    const block = html.slice(start, html.indexOf('-body"', start));
    // beta was never read, so the listing cannot say api is missing there.
    expect(block).toContain("beta/api: beta was not read");
    expect(block).not.toContain("Not in this listing: beta/api");
    // gamma was read and holds nothing: there the listing does show absence.
    expect(block).toContain("Not in this listing: gamma/api");
    // An unqualified name was looked for only where the listing read.
    expect(block).toContain("api: not in the namespaces read (alpha, gamma)");
    expect(block).not.toContain("Not in this listing: api<");
  });

  it("keeps a complete list's rows and copy unchanged", () => {
    const complete = project([
      tool(
        "list",
        "list_resources",
        [{ kind: "Service", name: "alpha-object", namespace: "alpha" }],
        { summary: JSON.stringify({ kind: "services" }) },
      ),
    ]);
    const [inventory] = groupsOf(complete.groups, "inventory");
    expect(inventory.latest.summary).toBe("1 returned");
    expect(inventory.latest.data).not.toHaveProperty("namespacesRead");
    expect(complete.limitations).toEqual([]);
    const empty = project([
      tool("list", "list_resources", [], {
        summary: JSON.stringify({ kind: "services" }),
      }),
    ]);
    expect(empty.limitations.map((item) => item.message)).toEqual([
      "Radar found no matching resources for services resources. Anything you do not have permission to read was not searched.",
    ]);
  });

  it("does not guess at a coverage object it cannot read", () => {
    for (const payload of [
      { items: [], coverage: { complete: false } },
      { items: [], coverage: { complete: true, namespaces: ["alpha"] } },
      { items: {}, coverage: { complete: false, namespaces: ["alpha"] } },
    ]) {
      const projection = project([
        tool("list", "list_resources", payload, {
          summary: JSON.stringify({ kind: "services" }),
        }),
      ]);
      expect(projection.groups).toEqual([]);
      expect(projection.limitations.map((item) => item.message)).toEqual([
        "Radar couldn't summarize this investigation step. Review it in Activity.",
      ]);
    }
  });
});
