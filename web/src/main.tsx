import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { RouterProvider } from '@tanstack/react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { router } from './router'
import {
  ThemeProvider,
  PaletteProvider,
  BannerProvider,
  EditSignalProvider,
  ShellProvider,
  ToastProvider,
  AdminProvider,
} from '@shoka/web-core'
import { AuthGate } from '@shoka/web-core'
import { shokaShellConfig } from './shokaShellConfig'
import './styles/global.css'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: { staleTime: Infinity, retry: false, refetchOnWindowFocus: false },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <ThemeProvider>
        <ToastProvider>
          <BannerProvider>
            <EditSignalProvider>
              <AdminProvider>
                <PaletteProvider>
                  <ShellProvider value={shokaShellConfig}>
                    <AuthGate appName="Shoka">
                      <RouterProvider router={router} />
                    </AuthGate>
                  </ShellProvider>
                </PaletteProvider>
              </AdminProvider>
            </EditSignalProvider>
          </BannerProvider>
        </ToastProvider>
      </ThemeProvider>
    </QueryClientProvider>
  </StrictMode>,
)
