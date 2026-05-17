// useVisibility wires document.visibilitychange (web) to a callback. On
// native, the document API is absent and the hook is a no-op.

import { useEffect } from 'react';

export function useVisibility(onVisible: () => void): void {
  useEffect(() => {
    if (typeof document === 'undefined') return;
    const handler = () => {
      if (document.visibilityState === 'visible') onVisible();
    };
    document.addEventListener('visibilitychange', handler);
    return () => document.removeEventListener('visibilitychange', handler);
  }, [onVisible]);
}
