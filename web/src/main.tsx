import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { router } from './router'
import { ThemeProvider } from './lib/theme'
import { PaletteProvider } from './lib/palette'
import { ToastProvider } from './lib/toast'
import { BannerProvider } from './lib/banner'
import { wsClient } from './lib/wsClient'
import './styles/global.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, retry: false, refetchOnWindowFocus: false },
  },
})

// Open the /ws/ui connection eagerly so the first query has a warm socket.
// The client reconnects with backoff on close; NOTIFY frames are routed into
// the cache + banners/toasts by the NotifyBridge mounted in the Shell.
wsClient().connect()

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <ToastProvider>
          <BannerProvider>
            <PaletteProvider>
              <RouterProvider router={router} />
            </PaletteProvider>
          </BannerProvider>
        </ToastProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
