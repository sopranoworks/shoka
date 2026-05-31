import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { router } from './router'
import { ThemeProvider } from './lib/theme'
import { PaletteProvider } from './lib/palette'
import { startWsClient } from './lib/ws'
import './styles/global.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, retry: false, refetchOnWindowFocus: false },
  },
})

// Feasibility check §2.3.4: start the long-lived WS client OUTSIDE the React
// tree, handing it the QueryClient so push events drive invalidateQueries.
// Points at a dead port on purpose; it backoff-reconnects forever, harmlessly.
startWsClient({
  url: 'ws://localhost:9999/mock',
  queryClient,
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <PaletteProvider>
          <RouterProvider router={router} />
        </PaletteProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
