import { defineConfig } from "vite";

export default defineConfig({
  base: "./",
  server: {
    port: 5555,
    strictPort: true,
    watch: { ignored: ["**/.mygo/**", "**/build/**"] },
  },
  build: { outDir: "dist" },
});
