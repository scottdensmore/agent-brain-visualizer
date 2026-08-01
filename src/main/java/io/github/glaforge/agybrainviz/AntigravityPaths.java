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

import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.Locale;

/**
 * The Antigravity locations the app still needs after sessions moved to the store: the default flavor
 * and the {@code ~/.gemini} root that sandboxes the inline file-preview endpoint.
 *
 * <p>{@code user.home} is read on every call (not cached) so tests that redirect it are honoured.
 */
final class AntigravityPaths {

    private AntigravityPaths() {}

    /** The flavor used when none is supplied. */
    static final String DEFAULT_FLAVOR = "antigravity-cli";

    /**
     * The {@code ~/.gemini} root, used to sandbox the inline file-preview endpoint. Sessions are read
     * from the store now, so this is the only reason the app still touches the Antigravity layout.
     */
    static Path geminiRoot() {
        return Paths.get(System.getProperty("user.home"), ".gemini");
    }

    /**
     * Whether a path inside the {@code ~/.gemini} sandbox is off limits to the file preview.
     *
     * <p>Living under {@code ~/.gemini} is not the same as being safe to serve. The Gemini CLI keeps
     * its own credentials in that directory — {@code oauth_creds.json}, {@code google_accounts.json},
     * {@code settings.json}, {@code mcp-oauth-tokens.json}, {@code .env} — while the transcripts the
     * preview exists for live one level down, under {@code <source>/brain/}. So the rule is about
     * shape rather than a list of names that a new credential file would immediately outgrow:
     *
     * <ul>
     *   <li>anything whose name starts with a dot, at any depth — {@code .env}, {@code
     *       .credentials/token.json};
     *   <li>any {@code .json} file sitting directly in the sandbox root, which is where the CLI's own
     *       state lives. Session JSON under {@code <source>/brain/} is unaffected.
     * </ul>
     *
     * <p>This is checked independently of anything a transcript claims, and nothing can grant an
     * exception to it. That matters because the ingest API is the way transcripts get here: an
     * earlier attempt at this fix served a file whenever a session's transcript linked it, and was
     * defeated by pushing a session that linked the credential file.
     */
    static boolean isPreviewDenied(Path sandbox, Path candidate) {
        Path relative;
        try {
            relative = sandbox.normalize().relativize(candidate.normalize());
        } catch (IllegalArgumentException e) {
            return true; // not inside the sandbox at all; the caller refuses it anyway
        }
        for (Path part : relative) {
            if (part.toString().startsWith(".")) {
                return true;
            }
        }
        return (
            relative.getNameCount() == 1 &&
            relative.toString().toLowerCase(Locale.ROOT).endsWith(".json")
        );
    }
}
