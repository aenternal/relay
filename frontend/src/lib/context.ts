import { createContext, useContext } from 'react';
import { useStore } from 'zustand';
import type { RelayStore } from './store';

export const StoreContext = createContext<RelayStore | null>(null);
export function useRelay() {
  const store = useContext(StoreContext);
  if (!store) throw new Error('Relay store is missing');
  return useStore(store);
}
