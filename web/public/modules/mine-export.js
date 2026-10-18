/*
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
export function slugify(name) {
  const s = (name || "")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return s || "skill";
}

// Double-quote a YAML scalar so free-text (with colons, #, etc.) stays valid front-matter.
function yamlString(value) {
  return `"${String(value || "")
    .replace(/\\/g, "\\\\")
    .replace(/"/g, '\\"')}"`;
}

/** A single skill as a SKILL.md document: YAML front-matter (name + description) then the body. */
export function skillMarkdown(skill) {
  const s = skill || {};
  const name = (s.name || "skill").trim();
  const desc = (s.whenToUse || "").trim();
  const body = (s.body || "").trim();
  return (
    `---\nname: ${yamlString(name)}\ndescription: ${yamlString(
      desc
    )}\n---\n\n` + `${body}\n`
  );
}

/** All proposed rules + tooling gaps as one AGENTS.md, with a provenance header. */
export function agentsMarkdown(report) {
  const r = report || {};
  const rules = r.agentsRules || [];
  const gaps = r.toolingGaps || [];
  const label = r.flavor || "sessions";

  const lines = ["# AGENTS.md", ""];
  lines.push(
    `<!-- Mined from ${r.sessionCount || 0} ${label} sessions ` +
      `(${
        r.analyzedSessions || 0
      } AI-analyzed) by Agent Brain Visualizer. Review before committing. -->`,
    ""
  );

  if (rules.length > 0) {
    lines.push("## Guidelines", "");
    for (const rule of rules) {
      const text = (rule.rule || "").trim();
      const why = (rule.rationale || "").trim();
      lines.push(why ? `- **${text}** — ${why}` : `- **${text}**`);
    }
    lines.push("");
  }

  if (gaps.length > 0) {
    lines.push("## Tooling gaps", "");
    for (const gap of gaps) lines.push(`- ${String(gap).trim()}`);
    lines.push("");
  }

  return lines.join("\n");
}

/** Triggers a browser download of `text` as `filename` (markdown by default). */
export function downloadText(
  filename,
  text,
  mime = "text/markdown;charset=utf-8"
) {
  const blob = new Blob([text], { type: mime });
  const url = URL.createObjectURL(blob);
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  URL.revokeObjectURL(url);
}
