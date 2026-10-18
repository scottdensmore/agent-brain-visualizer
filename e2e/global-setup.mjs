// Playwright global setup: write the fixture config file, stage the session payloads that
// seed.setup.mjs will push to the store once the server is up, and ensure the Go server binary
// (build/agy-brain-viz) is compiled.

import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { seedFixtures } from "./fixtures.mjs";

const projectRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

export default function globalSetup() {
  const home = path.join(projectRoot, "build", "e2e-home");
  const { sessions } = seedFixtures(home);
  // The seed project (which runs once the server is up) reads these and pushes them to /api/ingest.
  fs.writeFileSync(path.join(projectRoot, "build", "e2e-fixtures.json"), JSON.stringify(sessions));

  const binName = process.platform === "win32" ? "agy-brain-viz.exe" : "agy-brain-viz";
  const binPath = path.join(projectRoot, "build", binName);
  if (!fs.existsSync(binPath)) {
    try {
      execFileSync("go", ["build", "-o", binPath, "."], {
        cwd: projectRoot,
        stdio: "inherit",
      });
    } catch (err) {
      throw new Error(
        `No server binary found at ${binPath} and automatic 'go build' failed.\n` +
          `Build it first with:  go build -o build/${binName} .\n` +
          `(${err.message})`
      );
    }
  }
}
