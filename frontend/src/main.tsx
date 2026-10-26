import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { desktopBridge } from './lib/bridge';
import { createAppStore } from './lib/store';
import './styles.css';

const store = createAppStore(desktopBridge);
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <App store={store} />
  </StrictMode>,
);
