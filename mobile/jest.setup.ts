// jest setup — shims and globals shared by every test file.

// localStorage isn't provided in the default jsdom env we use for non-RN
// component tests; @react-native-async-storage/async-storage looks for it
// on the web. Provide a trivial in-memory polyfill so storage tests are
// deterministic.
if (typeof window !== 'undefined' && !('localStorage' in window)) {
  const store = new Map<string, string>();
  Object.defineProperty(window, 'localStorage', {
    value: {
      getItem: (k: string) => (store.has(k) ? store.get(k)! : null),
      setItem: (k: string, v: string) => { store.set(k, String(v)); },
      removeItem: (k: string) => { store.delete(k); },
      clear: () => { store.clear(); },
      key: (i: number) => Array.from(store.keys())[i] ?? null,
      get length() { return store.size; },
    },
    writable: true,
  });
}
