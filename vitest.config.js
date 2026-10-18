import { defineConfig } from "vitest/config";

// The frontend is plain ES-module JavaScript that runs in the browser. We test it under jsdom.
// The timezone is pinned to UTC via the `test` npm script (TZ=UTC) so that any locale/time
// assertions are deterministic across machines; this fallback covers direct `vitest` invocations.
process.env.TZ = process.env.TZ || "UTC";

export default defineConfig({
  test: {
    environment: "jsdom",
    include: ["web/test/**/*.test.js"],
    setupFiles: ["web/test/setup.js"],
  },
});
