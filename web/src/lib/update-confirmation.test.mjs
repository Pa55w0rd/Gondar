import { confirmedUpdate, fetchUpdateJSON, UpdateHTTPError, waitForUpdate } from "./update-confirmation.ts";
import assert from "node:assert/strict";
import { once } from "node:events";
import http from "node:http";
import test from "node:test";

const expected = {
  request_id: "request-fixture",
  operation_id: "operation-fixture",
  target_version: "v1.2.0",
  repository: "fixture/gondar",
};
function observation(overrides = {}) {
  return {
    operation: {
      operation_id: expected.operation_id,
      request_id: expected.request_id,
      repository: expected.repository,
      target_version: "1.2.0",
      from_version: "1.1.0",
      accepted_boot_id: "old-process",
      binary_sha256: "signed-binary-digest",
      phase: "completed",
      updated_at: 1,
      asset: "fixture.zip",
    },
    runtime: { version: "1.2.0", boot_id: "new-process", binary_sha256: "signed-binary-digest" },
    ...overrides,
  };
}
async function serverFor(t, handler) {
  const server = http.createServer(handler);
  server.listen(0, "127.0.0.1");
  await once(server, "listening");
  t.after(() => {
    server.closeAllConnections();
    server.close();
  });
  return `http://127.0.0.1:${server.address().port}`;
}
function json(response, status, body) {
  response.writeHead(status, { "Content-Type": "application/json" });
  response.end(JSON.stringify(body));
}

test("POST受理后不接收任何staged或SSE，仍经真实HTTP确认精确目标", async (t) => {
  let posts = 0,
    polls = 0;
  const ready = observation();
  const states = [
    observation({
      operation: { ...ready.operation, phase: "accepted", binary_sha256: undefined },
      runtime: { ...ready.runtime, version: "1.1.0", boot_id: "old-process" },
    }),
    observation({
      operation: { ...ready.operation, phase: "staged" },
      runtime: { ...ready.runtime, version: "1.1.0", boot_id: "old-process" },
    }),
    observation({ runtime: { ...ready.runtime, version: "9.0.0" } }),
    observation({ runtime: { ...ready.runtime, boot_id: "old-process" } }),
    observation({ runtime: { ...ready.runtime, binary_sha256: "wrong-binary" } }),
    ready,
  ];
  const base = await serverFor(t, (request, response) => {
    assert.equal(request.headers.authorization, "Bearer fixture-auth");
    if (request.method === "POST") {
      posts++;
      json(response, 202, { ok: true, operation_id: expected.operation_id, target: "1.2.0" });
    } else {
      assert.equal(new URL(request.url, base).searchParams.get("request_id"), expected.request_id);
      json(response, 200, states[Math.min(polls++, states.length - 1)]);
    }
  });
  await fetchUpdateJSON(`${base}/apply`, { method: "POST", body: "{}" }, "fixture-auth");
  const result = await waitForUpdate(
    expected,
    new AbortController().signal,
    (_, signal) => fetchUpdateJSON(`${base}/status?request_id=${expected.request_id}`, { signal }, "fixture-auth"),
    {
      delay: async () => {
        /* skip poll delay in this ordered HTTP fixture */
      },
    },
  );
  assert.deepEqual(result, { kind: "confirmed", version: "1.2.0" });
  assert.equal(posts, 1);
  assert.equal(polls, 6);
});

test("受理响应断开后按request_id查询，不重复POST安装", async (t) => {
  let posts = 0;
  const base = await serverFor(t, (request, response) => {
    if (request.method === "POST") {
      posts++;
      response.destroy();
    } else json(response, 200, observation());
  });
  await assert.rejects(fetchUpdateJSON(`${base}/apply`, { method: "POST" }, null));
  const result = await waitForUpdate(
    { ...expected, operation_id: undefined },
    new AbortController().signal,
    (_, signal) => fetchUpdateJSON(`${base}/status?request_id=${expected.request_id}`, { signal }, null),
  );
  assert.equal(result.kind, "confirmed");
  assert.equal(posts, 1);
});

test("401停止确认并要求登录，不判服务器安装失败或成功", async (t) => {
  let calls = 0;
  const base = await serverFor(t, (_, response) => {
    calls++;
    json(response, 401, { error: "未授权" });
  });
  const result = await waitForUpdate(expected, new AbortController().signal, (_, signal) =>
    fetchUpdateJSON(base, { signal }, null),
  );
  assert.deepEqual(result, { kind: "auth" });
  assert.equal(calls, 1);
});

test("卸载取消真实在途HTTP，旧观察没有后续状态回调", async (t) => {
  let markRequested;
  const requested = new Promise((resolve) => {
    markRequested = resolve;
  });
  let closed;
  const connectionClosed = new Promise((resolve) => {
    closed = resolve;
  });
  const base = await serverFor(t, (request) => {
    request.on("close", closed);
    markRequested();
  });
  const controller = new AbortController();
  let callbacks = 0;
  const pending = waitForUpdate(expected, controller.signal, (_, signal) => fetchUpdateJSON(base, { signal }, null), {
    onStatus: () => callbacks++,
  });
  await requested;
  controller.abort();
  assert.deepEqual(await pending, { kind: "aborted" });
  await connectionClosed;
  assert.equal(callbacks, 0);
});

test("错版本、错操作、旧boot、错摘要均不能以版本变化冒充成功", () => {
  for (const mutate of [
    (s) => {
      s.runtime.version = "9.0.0";
    },
    (s) => {
      s.operation.operation_id = "another-operation";
    },
    (s) => {
      s.operation.request_id = "another-request";
    },
    (s) => {
      s.operation.repository = "another/repo";
    },
    (s) => {
      s.runtime.boot_id = s.operation.accepted_boot_id;
    },
    (s) => {
      s.runtime.binary_sha256 = "other-binary";
    },
    (s) => {
      s.operation.phase = "activating";
    },
  ]) {
    const value = observation();
    mutate(value);
    assert.equal(confirmedUpdate(value, expected), false);
  }
});

test("旧服务只支持精确version时明确能力降级，不标确认成功", async () => {
  const result = await waitForUpdate(
    expected,
    new AbortController().signal,
    async () => {
      throw new UpdateHTTPError(404, "no endpoint");
    },
    { legacyVersion: async () => ({ version: "1.2.0" }) },
  );
  assert.equal(result.kind, "legacy");
});

test("超时保留未确认语义，普通网络错误不变成安装失败", async () => {
  let clock = 0;
  const result = await waitForUpdate(
    expected,
    new AbortController().signal,
    async () => {
      throw new TypeError("connection reset");
    },
    {
      now: () => clock,
      timeoutMs: 5,
      delay: async () => {
        clock++;
      },
    },
  );
  assert.deepEqual(result, { kind: "timeout" });
});

test("已认证的失败与自动回滚保留实际终态", async () => {
  for (const phase of ["failed", "rolled_back"]) {
    const value = observation();
    value.operation.phase = phase;
    value.operation.error = "fixture failure";
    const result = await waitForUpdate(expected, new AbortController().signal, async () => value);
    assert.deepEqual(result, { kind: phase, message: "fixture failure" });
  }
});
