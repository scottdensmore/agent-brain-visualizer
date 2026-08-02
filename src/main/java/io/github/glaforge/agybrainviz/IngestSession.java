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

import io.micronaut.serde.annotation.Serdeable;
import java.util.List;

/**
 * One trajectory pushed by an ingest client, carrying the tool's own transcript verbatim.
 *
 * <p>The client stays deliberately thin: it locates files, reads them, and reports when they last
 * changed. It does not parse them — {@link SourceNormalizer} does, on the server.
 *
 * @param source which agent wrote it ({@code claude-code}, {@code codex}, {@code antigravity-cli}, …)
 * @param id the trajectory's own stable id, derived from the transcript rather than its path, so the
 *     same session pushed from two machines de-duplicates onto one row
 * @param title an optional short label; the server derives one from the transcript when absent
 * @param sourceMtime when the transcript last changed on the client, in epoch milliseconds
 * @param raw the tool's transcript, one JSON object per line
 * @param summary an optional cached AI analysis (the {@code AnalysisResponse} JSON) to store with the
 *     session — e.g. Antigravity's on-disk {@code summary.json}; {@code null} when there is none
 * @param files the files this transcript explicitly attached, carried so the inline preview works
 *     from any machine rather than only the one that ran the agent; {@code null} when the client
 *     sent none (an older client, or {@code --no-upload-files})
 */
@Serdeable
public record IngestSession(
    String source,
    String id,
    String title,
    long sourceMtime,
    String raw,
    String summary,
    List<IngestFile> files
) {
    /** Compact constructor keeping {@code files} non-null, so callers never branch on it. */
    public IngestSession {
        files = files == null ? List.of() : List.copyOf(files);
    }

    /** Backwards-compatible shape for the many call sites (and clients) that push no files. */
    public IngestSession(
        String source,
        String id,
        String title,
        long sourceMtime,
        String raw,
        String summary
    ) {
        this(source, id, title, sourceMtime, raw, summary, List.of());
    }
}
