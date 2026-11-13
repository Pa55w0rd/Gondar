import assert from "node:assert/strict";
import test from "node:test";
import { observeNearViewport } from "./observe-near-viewport.ts";

test("大量离屏消息共用观察器，仅进入视口者触发一次，卸载全部释放", (t) => {
  const original = globalThis.IntersectionObserver;
  const instances = [];
  const cleanups = [];
  class Observer {
    constructor(callback, options) {
      this.callback = callback;
      this.options = options;
      this.targets = new Set();
      this.disconnected = false;
      instances.push(this);
    }
    observe(target) {
      this.targets.add(target);
    }
    unobserve(target) {
      this.targets.delete(target);
    }
    disconnect() {
      this.targets.clear();
      this.disconnected = true;
    }
    emit(target, isIntersecting) {
      this.callback([{ target, isIntersecting }]);
    }
  }
  globalThis.IntersectionObserver = Observer;
  t.after(() => {
    cleanups.forEach((stop) => stop());
    globalThis.IntersectionObserver = original;
  });
  const root = {};
  const elements = Array.from({ length: 1000 }, () => ({}));
  let details = 0;
  for (const element of elements) cleanups.push(observeNearViewport(element, root, "400px", () => details++));
  assert.equal(instances.length, 1);
  assert.equal(instances[0].targets.size, 1000);
  for (const element of elements) instances[0].emit(element, false);
  assert.equal(details, 0);
  instances[0].emit(elements[0], true);
  instances[0].emit(elements[0], true);
  assert.equal(details, 1);
  assert.equal(instances[0].targets.size, 999);
  cleanups.forEach((stop) => stop());
  assert.equal(instances[0].targets.size, 0);
  assert.equal(instances[0].disconnected, true);
  const stop = observeNearViewport({}, root, "400px", () => details++);
  cleanups.push(stop);
  assert.equal(instances.length, 2, "最后一个订阅退出后必须重建观察器");
  const newObserver = instances[1];
  newObserver.emit([...newObserver.targets][0], true);
  assert.equal(newObserver.disconnected, true);
  cleanups.push(observeNearViewport({}, root, "400px", () => {}));
  assert.equal(instances.length, 3, "自动进入视口后也必须释放池");
});

test("不同滚动根和边距隔离，缺少观察器时保持立即加载回退", (t) => {
  const original = globalThis.IntersectionObserver;
  let created = 0;
  class Observer {
    constructor() {
      created++;
    }
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.IntersectionObserver = Observer;
  const root = {};
  const cleanups = [
    observeNearViewport({}, root, "400px", () => {}),
    observeNearViewport({}, root, "800px", () => {}),
    observeNearViewport({}, null, "400px", () => {}),
  ];
  t.after(() => {
    cleanups.forEach((stop) => stop());
    globalThis.IntersectionObserver = original;
  });
  assert.equal(created, 3);
  cleanups.forEach((stop) => stop());
  delete globalThis.IntersectionObserver;
  let visible = 0;
  observeNearViewport({}, null, "400px", () => visible++)();
  assert.equal(visible, 1);
});

test("同一元素独立订阅，退出一个订阅不影响另一个，旧回调不再触发", (t) => {
  const original = globalThis.IntersectionObserver;
  let observer;
  class Observer {
    constructor(callback) {
      this.callback = callback;
      observer = this;
    }
    observe() {}
    unobserve() {}
    disconnect() {}
  }
  globalThis.IntersectionObserver = Observer;
  t.after(() => {
    globalThis.IntersectionObserver = original;
  });
  const element = {};
  let calls = 0;
  const callback = () => calls++;
  const stopFirst = observeNearViewport(element, null, "400px", callback);
  const stopSecond = observeNearViewport(element, null, "400px", callback);
  stopFirst();
  observer.callback([{ target: element, isIntersecting: true }]);
  observer.callback([{ target: element, isIntersecting: true }]);
  assert.equal(calls, 1);
  stopSecond();
});
