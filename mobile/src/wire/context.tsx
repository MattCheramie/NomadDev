// WSClientContext exposes the live WSClient instance to every screen
// without re-running useWebSocket per route. App.tsx owns the hook; any
// screen that needs to send envelopes reads useWSClient() and writes
// through that ref.

import { createContext, ReactNode, useContext } from 'react';
import { WSClient } from './client';

const Ctx = createContext<{ current: WSClient | null }>({ current: null });

export function WSClientProvider({
  value,
  children,
}: {
  value: { current: WSClient | null };
  children: ReactNode;
}) {
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useWSClient(): WSClient | null {
  return useContext(Ctx).current;
}
