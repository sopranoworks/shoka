import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { router } from './router'
import { ThemeProvider } from './lib/theme'
import { PaletteProvider } from './lib/palette'
import { ToastProvider } from '@shoka/web-core'
import { BannerProvider } from './lib/banner'
import { EditSignalProvider } from './lib/editSignal'
import { AdminProvider } from '@shoka/web-core'
import { AuthGate } from './components/AuthGate'
import './styles/global.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, retry: false, refetchOnWindowFocus: false },
  },
})

// The /ws/ui connection is opened by AuthGate once the user is authenticated (or
// the no-lockout single-operator case): opening it from the login screen would be
// 401'd by the server's session gate once a user exists (B-28 stage 1).

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <ToastProvider>
          <BannerProvider>
            <EditSignalProvider>
              {/* Single-user mode: the sole operator is the administrator, so
                  admin defaults to true. B-28 wires the real authenticated
                  identity here (and the matching server seam). */}
              <AdminProvider>
                <PaletteProvider>
                  <AuthGate>
                    <RouterProvider router={router} />
                  </AuthGate>
                </PaletteProvider>
              </AdminProvider>
            </EditSignalProvider>
          </BannerProvider>
        </ToastProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
