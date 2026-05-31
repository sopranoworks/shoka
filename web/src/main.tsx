import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { router } from './router'
import { ThemeProvider } from './lib/theme'
import { PaletteProvider } from './lib/palette'
import { wsClient } from './lib/wsClient'
import './styles/global.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, retry: false, refetchOnWindowFocus: false },
  },
})

// Open the /ws/ui connection eagerly so the first query has a warm socket.
// Session 1 uses it for request/response only; NOTIFY auto-refresh is session 2.
wsClient().connect()

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
