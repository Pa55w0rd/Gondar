import { missingInstallPlan, requireResolution, usableResolution, canUpdate, toolAvailable, displayToolVersion } from "./host-tool-versions.ts";
import assert from "node:assert/strict";
import test from "node:test";

const now = Date.parse("2026-11-06T12:00:00Z");
const version = (overrides = {}) => ({
  installable: true,
  resolution_id: "server-created-fixed-plan",
  expires_at: "2026-11-06T12:10:00Z",
  ...overrides,
});
const tool = (id, kind, available = false) => ({ id, kind, available, installable: kind !== "manual" });

test("only a live server resolution enables latest installation", () => {
  assert.equal(usableResolution(version(), now), "server-created-fixed-plan");
  for (const change of [
    { installable: false },
    { resolution_id: "" },
    { expires_at: "invalid" },
    { expires_at: "2026-11-06T12:00:00Z" },
  ])
    assert.equal(usableResolution(version(change), now), undefined);
});

test("batch never falls back from an untrusted remote tool to an unpinned install", () => {
  const tools = [
    tool("curl", "apt"),
    tool("nuclei", "github"),
    tool("httpx", "github"),
    tool("semgrep", "pip"),
    tool("manual", "manual"),
    tool("present", "github", true),
  ];
  const plan = missingInstallPlan(tools, {
    nuclei: version({ expires_at: "2099-01-01T00:00:00Z" }),
    httpx: version({ installable: false, locked_available: true }),
    semgrep: version({ installable: false, locked_available: false }),
  });
  assert.deepEqual(
    plan.tools.map((item) => item.id),
    ["curl", "nuclei"],
  );
  assert.deepEqual(plan.resolutions, { nuclei: "server-created-fixed-plan" });
});

test("expired plans cannot use the removed lock fallback", () => {
  const tools = [tool("nuclei", "github"), tool("httpx", "github")];
  const plan = missingInstallPlan(tools, {
    nuclei: version({ expires_at: "2000-01-01T00:00:00Z", locked_available: false }),
    httpx: version({ expires_at: "2000-01-01T00:00:00Z", locked_available: true }),
  });
  assert.deepEqual(
    plan.tools.map((item) => item.id),
    [],
  );
  assert.deepEqual(plan.resolutions, {});
});

test("a plan expiring between render and click cannot turn a detected install into a lock install", () => {
  const planned = version({ expires_at: "2026-11-06T12:00:01Z", locked_available: true });
  assert.equal(usableResolution(planned, now), "server-created-fixed-plan");
  assert.throws(() => requireResolution(planned, now + 2000), /重新检测/);
  assert.equal(requireResolution(planned, now), "server-created-fixed-plan");
});


test("installed tools never enter missing installs; update needs explicit newer status", () => {
 const installed=tool("nmap","apt",true);
 assert.equal(canUpdate(installed,version({state:"current",update_available:false})),false);
 assert.equal(canUpdate(installed,version({update_available:true,expires_at:"2099-01-01T00:00:00Z"})),true);
 assert.equal(canUpdate(installed,undefined),false);
 assert.deepEqual(missingInstallPlan([installed],{nmap:version()}).tools,[]);
 const detected=tool("pymongo","pip",false);
 assert.equal(toolAvailable(detected,{available:true}),true);
 assert.deepEqual(missingInstallPlan([detected],{pymongo:{available:true}}).tools,[]);
 assert.equal(displayToolVersion("a".repeat(40)),"提交 "+"a".repeat(12));
});
