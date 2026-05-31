import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// This prototype uses CODE-BASED routing (src/router.tsx), not TanStack
// Router's file-based route tree, so the @tanstack/router-plugin is intentionally
// NOT used (it would demand a src/routes/__root.tsx). Code-based routing keeps
// the whole route tree in one typed file with zero codegen.
//
// base: './' makes the production bundle's asset paths relative, so a Go server
// can embed and serve dist/ under any URL prefix (feasibility check §2.3.1).
export default defineConfig({
  base: './',
  plugins: [react()],
})
