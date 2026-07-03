import { defineConfig } from "vite"
import react from "@vitejs/plugin-react"
import tsconfigPaths from "vite-tsconfig-paths"

// Static SPA build. Tailwind is picked up automatically via postcss.config.mjs;
// the "@/..." path alias comes from tsconfig via vite-tsconfig-paths.
export default defineConfig({
  plugins: [react(), tsconfigPaths()],
  // Ensure a single React instance (react-query/react-router must share it).
  resolve: {
    dedupe: ["react", "react-dom"],
  },
  server: {
    port: 3000,
    host: true,
    // In dev the SPA calls the API same-origin; forward those to the core so no
    // build-time API URL is needed. Override the target with VITE_DEV_API_TARGET.
    proxy: {
      "/api": { target: process.env.VITE_DEV_API_TARGET || "http://localhost:8001", changeOrigin: true },
      "/ws": { target: process.env.VITE_DEV_API_TARGET || "http://localhost:8001", ws: true, changeOrigin: true },
    },
  },
  preview: {
    port: 3000,
    host: true,
  },
  build: {
    outDir: "dist",
  },
})
