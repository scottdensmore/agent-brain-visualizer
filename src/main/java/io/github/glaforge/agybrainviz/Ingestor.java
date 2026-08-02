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

import com.fasterxml.jackson.core.JacksonException;
import com.fasterxml.jackson.databind.ObjectMapper;
import jakarta.inject.Inject;
import jakarta.inject.Singleton;
import java.util.List;
import java.util.Optional;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Normalizes pushed trajectories and stores them.
 *
 * <p>The content hash is computed here, from the transcript the server actually received, rather than
 * trusted from the client. A client that hashed wrongly would otherwise be able to convince the store
 * that a changed trajectory was unchanged, and the row would never be refreshed. Getting it wrong now
 * costs only bandwidth: the client's manifest comparison misses, it re-pushes, and the upsert no-ops.
 */
@Singleton
public class Ingestor {

    private static final Logger LOG = LoggerFactory.getLogger(Ingestor.class);

    private static final ObjectMapper MAPPER = new ObjectMapper();

    /**
     * Largest cached analysis this server will store for one session.
     *
     * <p>A pushed summary is the one piece of client input that outlives its request: it lands in the
     * {@code summaries} table, and every cross-session endpoint ({@code /api/insights}, {@code
     * /api/eval}, {@code /api/mine}, {@code /api/optimize}) later parses up to 150 of them into
     * object trees at once. Without a bound, a handful of oversized documents do not merely make one
     * request expensive — they make every subsequent read of those endpoints fail, until the rows are
     * deleted by hand.
     *
     * <p>The ceiling is set above anything the server itself produces: {@link
     * AnalysisOrchestrator#normalize} bounds a model's analysis to roughly 120 KB of text in the
     * worst case, so 256 KiB leaves generous headroom while keeping the aggregate of 150 summaries
     * to a size the heap can hold.
     */
    static final int MAX_SUMMARY_CHARS = 256 * 1024;

    private final List<SourceNormalizer> normalizers;
    private final SessionRepository sessions;
    private final SummaryRepository summaries;

    @Inject
    public Ingestor(
        List<SourceNormalizer> normalizers,
        SessionRepository sessions,
        SummaryRepository summaries
    ) {
        this.normalizers = normalizers;
        this.sessions = sessions;
        this.summaries = summaries;
    }

    /** Stores a batch, counting each trajectory as ingested, skipped (unchanged), or failed. */
    public IngestResult ingest(List<IngestSession> batch) {
        int ingested = 0;
        int skipped = 0;
        int failed = 0;
        for (IngestSession pushed : batch == null ? List.<IngestSession>of() : batch) {
            switch (store(pushed)) {
                case INGESTED -> ingested++;
                case SKIPPED -> skipped++;
                case FAILED -> failed++;
            }
        }
        return new IngestResult(ingested, skipped, failed);
    }

    /**
     * Stores a batch of summaries pushed on their own (for sessions already in the store), counting
     * each as ingested, skipped (unchanged), or failed. Used for a summary that appeared after its
     * transcript was already ingested.
     */
    public IngestResult ingestSummaries(List<IngestSummary> batch) {
        int ingested = 0;
        int skipped = 0;
        int failed = 0;
        for (IngestSummary pushed : batch == null ? List.<IngestSummary>of() : batch) {
            switch (storeSummary(pushed)) {
                case INGESTED -> ingested++;
                case SKIPPED -> skipped++;
                case FAILED -> failed++;
            }
        }
        return new IngestResult(ingested, skipped, failed);
    }

    private Outcome storeSummary(IngestSummary pushed) {
        if (
            pushed == null ||
            isBlank(pushed.id()) ||
            isBlank(pushed.source()) ||
            isBlank(pushed.summary())
        ) {
            LOG.warn("Rejecting a pushed summary with no source, id, or body");
            return Outcome.FAILED;
        }
        // Only accept summaries for sources the server knows, matching the trajectory push.
        if (normalizerFor(pushed.source()).isEmpty()) {
            LOG.warn(
                "Rejecting summary for {}: unknown source '{}'",
                LogSafe.escape(pushed.id()),
                LogSafe.escape(pushed.source())
            );
            return Outcome.FAILED;
        }
        // Reject a non-JSON body up front (it can't go in the jsonb column) as this one item's
        // failure. Doing the check here, rather than catching the store's exception, keeps a genuine
        // store outage propagating out of the batch — a 503, exactly as a pushed trajectory behaves —
        // instead of being masked as a per-item failure inside a 200 response.
        // Oversized bodies are rejected the same way, and for the same reason: this row is read back
        // by every cross-session endpoint, so accepting it would degrade far more than this request.
        if (pushed.summary().length() > MAX_SUMMARY_CHARS) {
            LOG.warn(
                "Rejecting summary for {}: body is {} chars, over the {} limit",
                LogSafe.escape(pushed.id()),
                pushed.summary().length(),
                MAX_SUMMARY_CHARS
            );
            return Outcome.FAILED;
        }
        if (!isValidJson(pushed.summary())) {
            LOG.warn(
                "Rejecting summary for {}: body is not valid JSON",
                LogSafe.escape(pushed.id())
            );
            return Outcome.FAILED;
        }
        boolean written = summaries.upsert(pushed.source(), pushed.id(), pushed.summary(), null);
        return written ? Outcome.INGESTED : Outcome.SKIPPED;
    }

    private static boolean isValidJson(String value) {
        try {
            MAPPER.readTree(value);
            return true;
        } catch (JacksonException e) {
            return false;
        }
    }

    private enum Outcome {
        INGESTED,
        SKIPPED,
        FAILED,
    }

    private Outcome store(IngestSession pushed) {
        if (pushed == null || isBlank(pushed.id()) || isBlank(pushed.source())) {
            LOG.warn("Rejecting a pushed trajectory with no source or id");
            return Outcome.FAILED;
        }
        Optional<SourceNormalizer> normalizer = normalizerFor(pushed.source());
        if (normalizer.isEmpty()) {
            LOG.warn(
                "Rejecting trajectory {}: unknown source '{}'",
                LogSafe.escape(pushed.id()),
                LogSafe.escape(pushed.source())
            );
            return Outcome.FAILED;
        }

        String raw = pushed.raw() == null ? "" : pushed.raw();
        List<String> lines = raw.lines().toList();
        String stepsJson = normalizer.get().toStepsJson(lines);
        String title = titleFor(pushed, normalizer.get(), lines);

        // A pushed mtime of 0 (a client that could not stat the file) would sort the session to the
        // epoch; treat it as "now" so it still appears at the top of a newest-first listing.
        long mtime = pushed.sourceMtime() > 0 ? pushed.sourceMtime() : System.currentTimeMillis();

        boolean written = sessions.upsert(
            new SessionRepository.Session(
                pushed.source(),
                pushed.id(),
                title,
                mtime,
                stepsJson,
                sha256(raw)
            )
        );
        // Store a pushed cached analysis (e.g. Antigravity's on-disk summary.json) if present, so a
        // machine that already generated one doesn't force every viewer to recompute it. A summary
        // that won't store (e.g. it isn't valid jsonb) must not sink the session it rode in with, nor
        // the rest of the batch — the transcript is what matters, so log it and move on.
        if (!isBlank(pushed.summary())) {
            try {
                if (pushed.summary().length() > MAX_SUMMARY_CHARS) {
                    // Same bound as the dedicated summaries endpoint, but a rejection here only drops
                    // the cached analysis: the transcript is what matters, and the session it rode in
                    // with has already been stored.
                    LOG.warn(
                        "Stored trajectory {} but dropped its pushed summary: {} chars, over the {} limit",
                        LogSafe.escape(pushed.id()),
                        pushed.summary().length(),
                        MAX_SUMMARY_CHARS
                    );
                } else {
                    summaries.upsert(pushed.source(), pushed.id(), pushed.summary(), title);
                }
            } catch (RuntimeException e) {
                LOG.warn(
                    "Stored trajectory {} but could not store its pushed summary: {}",
                    LogSafe.escape(pushed.id()),
                    // The store's message quotes the pushed id back, so it carries the same taint.
                    LogSafe.escape(e.getMessage())
                );
            }
        }
        return written ? Outcome.INGESTED : Outcome.SKIPPED;
    }

    private Optional<SourceNormalizer> normalizerFor(String source) {
        return normalizers.stream().filter(n -> n.handles(source)).findFirst();
    }

    // The client's title wins, then whatever the transcript reveals, then a stable last resort — the
    // session list must never show a blank row.
    private String titleFor(IngestSession pushed, SourceNormalizer normalizer, List<String> lines) {
        if (!isBlank(pushed.title())) return pushed.title().trim();
        return normalizer
            .deriveTitle(lines)
            .filter(t -> !t.isBlank())
            .orElseGet(() -> pushed.source() + " session " + shortId(pushed.id()));
    }

    private static String shortId(String id) {
        return id.length() > 8 ? id.substring(0, 8) : id;
    }

    /** Hex SHA-256 of the transcript's UTF-8 bytes. Clients hash the file the same way. */
    static String sha256(String raw) {
        return Hashing.sha256Hex(raw);
    }

    private static boolean isBlank(String value) {
        return value == null || value.isBlank();
    }
}
