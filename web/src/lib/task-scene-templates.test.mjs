import assert from "node:assert/strict";
import test from "node:test";
import { instantiateTaskScene, normalizeSceneTargets, taskTemplateNeedsTarget } from "./task-scene-templates.ts";

const scene = (kind) => ({
  input_kind: kind,
  max_targets: 100,
  timeout_seconds: 1800,
  seed_first_intent: false,
  coverage_enabled: true,
});

test("batch domains canonicalize, deduplicate and retain exact hosts", () => {
  assert.deepEqual(
    normalizeSceneTargets(scene("domains"), "Console.Example.test.\nconsole.example.test\napi.example.test"),
    ["console.example.test", "api.example.test"],
  );
  for (const target of [
    "*.example.test",
    "https://example.test/a",
    "user@example.test",
    "192.0.2.1",
    "example.test:443",
  ]) {
    assert.throws(() => normalizeSceneTargets(scene("domains"), target));
  }
  assert.throws(
    () =>
      normalizeSceneTargets(scene("domains"), Array.from({ length: 101 }, (_, i) => `h${i}.example.test`).join("\n")),
    /100/,
  );
});

test("batch IP supports canonical IPv6 but refuses CIDR and ambiguous IPv4", () => {
  assert.deepEqual(normalizeSceneTargets(scene("ips"), "192.0.2.1\n192.0.2.1\n2001:0db8::1\n[2001:db8::1]"), [
    "192.0.2.1",
    "2001:db8::1",
  ]);
  for (const target of ["192.0.2.0/24", "127.1", "0x7f000001", "192.000.2.1", "256.2.3.4", "https://example.test"]) {
    assert.throws(() => normalizeSceneTargets(scene("ips"), target));
  }
});

test("single-host URL preserves entry path/port, refuses credentials and wildcards", () => {
  assert.deepEqual(normalizeSceneTargets(scene("host"), "https://Console.Example.test:8443/api#fragment"), [
    "https://console.example.test:8443/api",
  ]);
  for (const target of ["https://user:secret@example.test", "ftp://example.test", "*.example.test", "a.test\nb.test"]) {
    assert.throws(() => normalizeSceneTargets(scene("host"), target));
  }
});

test("scene application renders both fields literally without changing defaults or original", () => {
  const template = {
    id: 7,
    builtin_key: "batch_domains",
    name: "batch",
    description: "list\n{{target}}",
    goal: "check\n{{target}}",
    scene: scene("domains"),
  };
  const rendered = instantiateTaskScene(template, "A.Example.test\na.example.test");
  assert.equal(rendered.description, "list\na.example.test");
  assert.equal(rendered.goal, "check\na.example.test");
  assert.equal(rendered.scene.timeout_seconds, 1800);
  assert.equal(template.description, "list\n{{target}}");
  assert.throws(() => instantiateTaskScene(template, "{{target}}"));
});

test("copied custom templates keep target substitution without inheriting builtin task defaults", () => {
  const custom = { id: 9, name: "copy", description: "scope {{target}}", goal: "verify {{target}}" };
  assert.equal(taskTemplateNeedsTarget(custom), true);
  const rendered = instantiateTaskScene(custom, "literal $& target");
  assert.equal(rendered.goal, "verify literal $& target");
  assert.equal(rendered.scene, undefined);
  assert.equal(taskTemplateNeedsTarget({ description: "ordinary", goal: "task" }), false);
});
