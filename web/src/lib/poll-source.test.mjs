import assert from "node:assert/strict";
import test from "node:test";
import { startPollSource } from "./poll-source.ts";

const turn = () => new Promise((resolve) => setImmediate(resolve));
const deferred = () => {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => {
    resolve = yes;
    reject = no;
  });
  return { promise, resolve, reject };
};
function environment() {
  let visible = true;
  const listeners = new Set();
  const timers = new Set();
  return {
    visible: () => visible,
    listen: (changed) => {
      listeners.add(changed);
      return () => listeners.delete(changed);
    },
    after: (run) => {
      timers.add(run);
      return () => timers.delete(run);
    },
    show(value) {
      visible = value;
      for (const changed of listeners) changed();
    },
    tick() {
      for (const run of [...timers]) {
        timers.delete(run);
        run();
      }
    },
    listeners,
    timers,
  };
}

test("慢来源不重入，多次刷新合并为一次后续请求", async () => {
  const env = environment();
  const first = deferred();
  const second = deferred();
  let calls = 0;
  const received = [];
  const source = startPollSource(
    () => (++calls === 1 ? first.promise : second.promise),
    (v) => received.push(v),
    5,
    env,
  );
  await turn();
  source.refresh();
  source.refresh();
  source.refresh();
  env.tick();
  assert.equal(calls, 1);
  assert.equal(env.timers.size, 0);
  first.resolve("first");
  await turn();
  assert.equal(calls, 2);
  assert.deepEqual(received, ["first"]);
  second.resolve("second");
  await turn();
  assert.deepEqual(received, ["first", "second"]);
  assert.equal(env.timers.size, 1);
  source.stop();
  assert.equal(env.timers.size, 0);
  assert.equal(env.listeners.size, 0);
});

test("隐藏页面不启动轮询，返回可见页只补一次", async () => {
  const env = environment();
  env.show(false);
  let calls = 0;
  const first = deferred();
  const received = [];
  const source = startPollSource(
    () => {
      calls++;
      return first.promise;
    },
    (v) => received.push(v),
    5,
    env,
  );
  await turn();
  assert.equal(calls, 0);
  assert.equal(env.timers.size, 0);
  env.show(true);
  await turn();
  assert.equal(calls, 1);
  env.show(false);
  first.resolve("in-flight result");
  await turn();
  env.tick();
  assert.equal(calls, 1);
  assert.deepEqual(received, ["in-flight result"]);
  env.show(true);
  await turn();
  assert.equal(calls, 2);
  source.stop();
});

test("一个来源失败或一直等待不阻塞其它来源，卸载后不再提交结果", async () => {
  const env = environment();
  const slow = deferred();
  const late = [];
  const healthy = [];
  let failures = 0;
  const sources = [
    startPollSource(
      () => slow.promise,
      (v) => late.push(v),
      5,
      env,
    ),
    startPollSource(
      () => Promise.reject(new Error(String(++failures))),
      () => assert.fail("错误结果被提交"),
      5,
      env,
    ),
    startPollSource(
      () => Promise.resolve("healthy"),
      (v) => healthy.push(v),
      5,
      env,
    ),
  ];
  await turn();
  assert.deepEqual(healthy, ["healthy"]);
  assert.equal(failures, 1);
  env.tick();
  await turn();
  assert.deepEqual(healthy, ["healthy", "healthy"]);
  assert.equal(failures, 2);
  sources.forEach((source) => source.stop());
  slow.resolve("late");
  await turn();
  env.tick();
  assert.deepEqual(late, []);
  assert.equal(env.timers.size, 0);
  assert.equal(env.listeners.size, 0);
});
