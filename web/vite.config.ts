import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// https://vitejs.dev/config/
export default defineConfig({
  plugins: [react()],
  build: {
    // Build directly into the directory the Go server embeds (server/embed.go).
    outDir: '../server/dist',
    emptyOutDir: true,
  },
})
