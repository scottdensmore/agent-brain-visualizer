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

import io.micronaut.http.HttpResponse;
import io.micronaut.http.HttpStatus;
import io.micronaut.http.annotation.Controller;
import io.micronaut.http.annotation.Get;
import io.micronaut.http.annotation.PathVariable;
import io.micronaut.http.annotation.QueryValue;
import io.micronaut.scheduling.TaskExecutors;
import io.micronaut.scheduling.annotation.ExecuteOn;
import jakarta.inject.Inject;
import java.io.IOException;
import java.nio.file.Files;
import java.nio.file.NoSuchFileException;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.List;
import java.util.Map;
import java.util.Optional;

/** Serves the session list and transcripts from the shared store, keyed by {@code flavor}/source. */
@Controller("/api/brain")
public class BrainController {

    private final SessionRepository sessions;

    @Inject
    public BrainController(SessionRepository sessions) {
        this.sessions = sessions;
    }

    /** Default page size when the client asks for none — enough to fill the sidebar in one load. */
    static final int DEFAULT_LIMIT = 200;
    /** Hard cap so a client can never make the store return an unbounded list in one response. */
    static final int MAX_LIMIT = 1000;

    /**
     * One page of a source's sessions, newest first. The list is paged so a large store never has to
     * be returned (or rendered) all at once; the client loads more on demand and uses {@code total} to
     * know when it has them all.
     */
    @ExecuteOn(TaskExecutors.IO)
    @Get("/conversations")
    public ConversationPage listConversations(
        @QueryValue Optional<String> flavor,
        @QueryValue Optional<Integer> limit,
        @QueryValue Optional<Integer> offset
    ) {
        String source = flavor.orElse(AntigravityPaths.DEFAULT_FLAVOR);
        int pageLimit = clamp(limit.orElse(DEFAULT_LIMIT), 1, MAX_LIMIT);
        int pageOffset = Math.max(0, offset.orElse(0));
        List<Map<String, String>> items = sessions.listConversations(source, pageLimit, pageOffset);
        return new ConversationPage(items, sessions.countBySource(source), pageLimit, pageOffset);
    }

    private static int clamp(int value, int min, int max) {
        return Math.max(min, Math.min(max, value));
    }

    @ExecuteOn(TaskExecutors.IO)
    @Get(value = "/conversations/{id}/transcript", produces = "application/json")
    public String getTranscript(@PathVariable String id, @QueryValue Optional<String> flavor) {
        String steps = sessions.steps(flavor.orElse(AntigravityPaths.DEFAULT_FLAVOR), id);
        return steps != null ? steps : "[]";
    }

    /**
     * Serves a referenced file's contents for the inline preview.
     *
     * <p>This one endpoint is inherently machine-local: Antigravity transcripts link to files under
     * the user's {@code ~/.gemini}, which live on the machine that ran the agent, not in the store. On
     * a different machine the file simply isn't there, and the preview returns not-found — the one
     * honest exception to reads coming from the store.
     */
    @ExecuteOn(TaskExecutors.IO)
    @Get(value = "/file", produces = "text/plain")
    public HttpResponse<String> getFileContent(@QueryValue String path) {
        try {
            Path filePath = Paths.get(path).normalize();
            Path geminiDir = AntigravityPaths.geminiRoot().normalize();
            if (!filePath.startsWith(geminiDir)) {
                return HttpResponse.unauthorized();
            }
            // Being inside the sandbox is not enough: the Gemini CLI keeps its own credentials in
            // this directory. Checked before the file is touched, so a refusal is not an existence
            // oracle for the rest of ~/.gemini.
            if (AntigravityPaths.isPreviewDenied(geminiDir, filePath)) {
                return deniedForPreview();
            }
            if (!Files.exists(filePath) || Files.isDirectory(filePath)) {
                return HttpResponse.notFound("File not available on this machine.");
            }
            // A symlink under ~/.gemini can point anywhere on disk; re-check the sandbox against the
            // fully resolved path so a planted link can't read files outside it — or, via the deny
            // check, reach a credential file that the requested spelling hid.
            Path realFile = filePath.toRealPath();
            Path realGeminiDir = geminiDir.toRealPath();
            if (!realFile.startsWith(realGeminiDir)) {
                return HttpResponse.unauthorized();
            }
            if (AntigravityPaths.isPreviewDenied(realGeminiDir, realFile)) {
                return deniedForPreview();
            }
            return HttpResponse.ok(Files.readString(realFile));
        } catch (NoSuchFileException e) {
            return HttpResponse.notFound("File not available on this machine.");
        } catch (IOException e) {
            return HttpResponse.serverError("Error reading file: " + e.getMessage());
        }
    }

    /**
     * Forbidden rather than unauthorized: no credential makes these files previewable, and the
     * frontend's {@code apiFetch} prompts for an API token on a 401, which would send the user
     * hunting for a token that cannot help.
     */
    private static HttpResponse<String> deniedForPreview() {
        return HttpResponse
            .<String>status(HttpStatus.FORBIDDEN)
            .body("This file is not available for preview.");
    }
}
