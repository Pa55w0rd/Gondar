interface ObserverPool {
  observer: IntersectionObserver;
  targets: Map<Element, Set<() => void>>;
}

// A pool exists only while it has subscribers, including for the null viewport.
const pools = new Map<Element | null, Map<string, ObserverPool>>();

export function observeNearViewport(element: Element, root: Element | null, rootMargin: string, visible: () => void) {
  if (typeof IntersectionObserver === "undefined") {
    visible();
    return () => {};
  }
  let margins = pools.get(root);
  if (!margins) {
    margins = new Map();
    pools.set(root, margins);
  }
  const bucket = margins;
  let pool = bucket.get(rootMargin);
  const releaseEmptyPool = (value: ObserverPool) => {
    if (value.targets.size > 0 || bucket.get(rootMargin) !== value) return;
    value.observer.disconnect();
    bucket.delete(rootMargin);
    if (bucket.size === 0) pools.delete(root);
  };
  if (!pool) {
    const targets = new Map<Element, Set<() => void>>();
    let registered: ObserverPool;
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) {
          if (!entry.isIntersecting) continue;
          const callbacks = targets.get(entry.target);
          if (!callbacks) continue;
          targets.delete(entry.target);
          observer.unobserve(entry.target);
          for (const callback of callbacks) callback();
        }
        releaseEmptyPool(registered);
      },
      { root, rootMargin },
    );
    pool = { observer, targets };
    registered = pool;
    bucket.set(rootMargin, registered);
  }
  const targetPool = pool;
  let callbacks = targetPool.targets.get(element);
  if (!callbacks) {
    callbacks = new Set();
    targetPool.targets.set(element, callbacks);
    targetPool.observer.observe(element);
  }
  let active = true;
  const notify = () => {
    if (!active) return;
    active = false;
    visible();
  };
  callbacks.add(notify);
  return () => {
    active = false;
    const current = targetPool.targets.get(element);
    current?.delete(notify);
    if (current?.size === 0) {
      targetPool.targets.delete(element);
      targetPool.observer.unobserve(element);
    }
    releaseEmptyPool(targetPool);
  };
}
