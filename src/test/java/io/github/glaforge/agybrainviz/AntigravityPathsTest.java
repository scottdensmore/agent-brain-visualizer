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
package io.github.glaforge.agybrainviz;

import static org.junit.jupiter.api.Assertions.assertEquals;
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.List;
import org.junit.jupiter.api.Test;

/** Unit tests for the small Antigravity layout helper that survives the move to the store. */
class AntigravityPathsTest {

    @Test
    void defaultFlavorIsTheAntigravityCli() {
        assertEquals("antigravity-cli", AntigravityPaths.DEFAULT_FLAVOR);
    }

    @Test
    void geminiRootIsDotGeminiUnderTheCurrentHome() {
        Path root = AntigravityPaths.geminiRoot();
        assertTrue(root.endsWith(".gemini"));
        assertTrue(root.startsWith(Paths.get(System.getProperty("user.home"))));
    }

    private static final Path SANDBOX = Paths.get("/home/u/.gemini");

    private static boolean denied(String relative) {
        return AntigravityPaths.isPreviewDenied(SANDBOX, SANDBOX.resolve(relative));
    }

    /** The CLI's own state lives at the sandbox root; a new credential file must be denied too. */
    @Test
    void deniesCredentialShapedPathsInTheSandbox() {
        for (String relative : List.of(
            "oauth_creds.json",
            "google_accounts.json",
            "settings.json",
            "mcp-oauth-tokens.json",
            "OAUTH_CREDS.JSON",
            "anything-at-all.json",
            ".env",
            ".hidden",
            ".credentials/token.json",
            "antigravity-cli/.env",
            "antigravity-cli/brain/.secret/notes.txt"
        )) {
            assertTrue(denied(relative), relative + " must not be previewable");
        }
    }

    /** Session data — the thing the preview exists for — stays readable, JSON included. */
    @Test
    void allowsSessionDataUnderTheFlavorSubtrees() {
        for (String relative : List.of(
            "config.txt",
            "notes.md",
            "antigravity-cli/brain/sess-1/state.json",
            "antigravity-cli/brain/sess-1/transcript.jsonl",
            "codex/brain/rollout.json"
        )) {
            assertFalse(denied(relative), relative + " should still be previewable");
        }
    }

    @Test
    void deniesAPathThatIsNotInsideTheSandboxAtAll() {
        assertTrue(
            AntigravityPaths.isPreviewDenied(SANDBOX, Paths.get("/etc/passwd")),
            "a path outside the sandbox has no business being previewable"
        );
    }

    @Test
    void treatsTheDotJsonRuleAsRootOnly() {
        // The rule is about the CLI's own state directory, not about JSON in general.
        assertTrue(denied("creds.json"));
        assertFalse(denied("sub/creds.json"));
    }
}
