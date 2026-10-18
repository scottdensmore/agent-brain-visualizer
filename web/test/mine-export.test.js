import { describe, expect, it } from "vitest";
import {
  slugify,
  skillMarkdown,
  agentsMarkdown,
} from "../public/modules/mine-export.js";

describe("slugify", () => {
  it("kebab-cases and strips unsafe characters", () => {
    expect(slugify("Read, Edit & Verify!")).toBe("read-edit-verify");
    expect(slugify("  spaced  out  ")).toBe("spaced-out");
  });

  it("falls back to 'skill' for empty input", () => {
    expect(slugify("")).toBe("skill");
    expect(slugify("***")).toBe("skill");
  });
});

describe("skillMarkdown", () => {
  it("renders YAML front-matter and body", () => {
    const md = skillMarkdown({
      name: "edit-and-verify",
      whenToUse: "When editing then checking a build",
      body: "1. Read\n2. Edit\n3. Bash",
    });
    expect(md).toContain("---\n");
    expect(md).toContain('name: "edit-and-verify"');
    expect(md).toContain('description: "When editing then checking a build"');
    expect(md).toContain("1. Read\n2. Edit\n3. Bash");
    // Front-matter closes before the body.
    expect(md.indexOf("---\n\n")).toBeGreaterThan(0);
  });

  it("escapes quotes so free-text descriptions stay valid YAML", () => {
    const md = skillMarkdown({ name: "x", whenToUse: 'run "gradle build"', body: "do it" });
    expect(md).toContain('description: "run \\"gradle build\\""');
  });
});

describe("agentsMarkdown", () => {
  const REPORT = {
    flavor: "antigravity-cli",
    sessionCount: 12,
    analyzedSessions: 5,
    agentsRules: [
      { rule: "Build with JDK 25 via mise", rationale: "JDK 21 can't load the plugins" },
      { rule: "Squash-merge only", rationale: "" },
    ],
    toolingGaps: ["A one-shot build-and-test command"],
  };

  it("renders a provenance header, guidelines, and tooling gaps", () => {
    const md = agentsMarkdown(REPORT);
    expect(md).toContain("# AGENTS.md");
    expect(md).toContain("Mined from 12 antigravity-cli sessions (5 AI-analyzed)");
    expect(md).toContain("## Guidelines");
    expect(md).toContain("- **Build with JDK 25 via mise** — JDK 21 can't load the plugins");
    // A rule without a rationale omits the em dash.
    expect(md).toContain("- **Squash-merge only**\n");
    expect(md).toContain("## Tooling gaps");
    expect(md).toContain("- A one-shot build-and-test command");
  });

  it("omits empty sections", () => {
    const md = agentsMarkdown({ flavor: "codex", agentsRules: [], toolingGaps: [] });
    expect(md).toContain("# AGENTS.md");
    expect(md).not.toContain("## Guidelines");
    expect(md).not.toContain("## Tooling gaps");
  });
});
