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

import com.fasterxml.jackson.databind.ObjectMapper;
import io.micronaut.scheduling.TaskExecutors;
import jakarta.annotation.PreDestroy;
import jakarta.inject.Inject;
import jakarta.inject.Named;
import jakarta.inject.Singleton;
import java.io.IOException;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.Semaphore;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.function.ToIntFunction;
import java.util.function.UnaryOperator;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Runs the full "summarize a conversation" pipeline off the HTTP layer: chunking the transcript,
 * fanning the chunks out to the LLM in parallel, consolidating the partial analyses (recursively,
 * with a deterministic local-merge fallback), reporting progress, and caching the final result.
 */
@Singleton
public class AnalysisOrchestrator {

    private static final Logger LOG = LoggerFactory.getLogger(AnalysisOrchestrator.class);
    private static final ObjectMapper MAPPER = new ObjectMapper();

    private static final int MAX_TOKENS_PER_CHUNK = 100_000;

    /** Hard caps on what one model reply may write into the cached, publicly served analysis. */
    static final int MAX_TITLE_CHARS = 120;
    static final int MAX_SUMMARY_CHARS = 8_000;
    static final int MAX_ITEM_CHARS = 600;
    static final int MAX_ITEMS = 40;
    static final int MAX_RECOMMENDATIONS = 30;

    public record ProgressState(int progress, String phase) {}

    private final Map<String, ProgressState> progressMap = new ConcurrentHashMap<>();
    private final Map<String, Object> runningTasks = new ConcurrentHashMap<>();

    /**
     * Dedicated single-thread scheduler that animates the progress bar during the long final LLM
     * consolidation, so no IO-pool thread is parked just to sleep between ticks.
     */
    private final ScheduledExecutorService progressScheduler =
        Executors.newSingleThreadScheduledExecutor(runnable -> {
            Thread thread = new Thread(runnable, "analysis-progress-ticker");
            thread.setDaemon(true);
            return thread;
        });

    private final AnalyzerService analyzerService;
    private final ExecutorService executor;
    private final TokenCounter tokenCounter;
    private final SessionRepository sessions;
    private final SummaryRepository summaries;

    @Inject
    public AnalysisOrchestrator(
        AnalyzerService analyzerService,
        @Named(TaskExecutors.IO) ExecutorService executor,
        TokenCounter tokenCounter,
        SessionRepository sessions,
        SummaryRepository summaries
    ) {
        this.analyzerService = analyzerService;
        this.executor = executor;
        this.tokenCounter = tokenCounter;
        this.sessions = sessions;
        this.summaries = summaries;
    }

    @PreDestroy
    void shutdown() {
        progressScheduler.shutdownNow();
    }

    /**
     * In-flight work is keyed by source and id together — two sources can legitimately hold a
     * session with the same id, and they must not share a progress bar or an "already running" slot.
     */
    private static String taskKey(String flavorName, String id) {
        return flavorName + "/" + id;
    }

    /** Current progress of an in-flight analysis, or {@code null} when none is running. */
    public ProgressState progress(String flavorName, String id) {
        return progressMap.get(taskKey(flavorName, id));
    }

    public String summarize(String flavorName, String id, boolean forceRecompute)
        throws IOException {
        String key = taskKey(flavorName, id);
        if (runningTasks.putIfAbsent(key, new Object()) != null) {
            return "{\"summary\": \"Analysis is already running in the background for this conversation. Please wait a moment and refresh.\"}";
        }

        try {
            if (!sessions.exists(flavorName, id)) {
                return "{\"summary\": \"No transcript found.\"}";
            }

            if (!forceRecompute) {
                Optional<String> cached = summaries.find(flavorName, id);
                if (cached.isPresent()) {
                    return cached.get();
                }
            } else {
                summaries.delete(flavorName, id);
            }

            try {
                return analyzeAndConsolidate(flavorName, id, key);
            } catch (Exception e) {
                LOG.error("Exception caught during analysis", e);
                try {
                    return MAPPER.writeValueAsString(
                        Map.of("summary", "Error generating summary: " + e.getMessage())
                    );
                } catch (Exception ex) {
                    return "{\"summary\": \"Error generating summary: Unknown error\"}";
                }
            } finally {
                progressMap.remove(key);
            }
        } finally {
            runningTasks.remove(key);
        }
    }

    private String analyzeAndConsolidate(String flavorName, String id, String key)
        throws Exception {
        List<List<String>> sequences = AnalysisSequences.fromStepsJson(
            flavorName,
            sessions.steps(flavorName, id)
        );

        ToIntFunction<String> tokenFn = tokenCounter::estimate;

        progressMap.put(key, new ProgressState(5, "Estimating Tokens & Chunking...")); // Phase 1: Estimating

        List<String> combinedLines = new ArrayList<>();
        for (List<String> seq : sequences) {
            combinedLines.addAll(seq);
        }
        List<List<String>> optimalChunks = new ArrayList<>();
        TranscriptParser.splitIntoSafeChunks(
            combinedLines,
            tokenFn,
            MAX_TOKENS_PER_CHUNK,
            optimalChunks
        );

        LOG.info("Total optimal chunks to process in parallel: {}", optimalChunks.size());

        progressMap.put(key, new ProgressState(0, "Starting chunk processing...")); // start at 0%

        List<AnalysisResponse> seqResponses = analyzeChunksInParallel(key, optimalChunks);

        AnalysisResponse responseObj;
        boolean consolidationFellBack = false;

        if (seqResponses.isEmpty()) {
            // Distinguish "nothing to analyze" from "the model failed for every chunk".
            String msg = optimalChunks.isEmpty()
                ? "No transcript lines found."
                : "Analysis could not be generated: the model did not return a result for any part of this conversation. Please try again.";
            return MAPPER.writeValueAsString(Map.of("summary", msg));
        } else if (seqResponses.size() == 1) {
            responseObj = seqResponses.get(0);
        } else {
            progressMap.put(key, new ProgressState(90, "Consolidating final analysis..."));

            // Keep the progress bar moving smoothly during the long final LLM consolidation
            ScheduledFuture<?> fakeProgress = startConsolidationTicker(key);
            try {
                responseObj = recursivelyConsolidate(seqResponses, tokenFn, MAPPER, 500_000);
            } catch (Exception e) {
                // LLM consolidation failed (e.g. API timeout). Rather than discard all the
                // per-chunk work, merge the partial analyses locally so the user still gets
                // a usable summary.
                LOG.warn("Consolidation failed; using local merge fallback", e);
                responseObj = localMerge(seqResponses);
                consolidationFellBack = true;
            } finally {
                fakeProgress.cancel(false);
            }
        }

        progressMap.put(key, new ProgressState(100, "Done"));

        // The model just read an attacker-controllable transcript, so bound what its reply can write
        // into the cache that every viewer (and later the miner) reads.
        responseObj = normalize(responseObj);

        String jsonResponse = MAPPER.writeValueAsString(responseObj);

        // Don't persist a degraded local-merge fallback: the consolidation failure was
        // likely transient, so a later (non-forced) load should retry the LLM rather than
        // serve the cruder summary.
        if (consolidationFellBack) {
            return jsonResponse;
        }

        String title = responseObj.shortTitle();
        // Caching is best-effort: a store hiccup must not discard a summary we just computed.
        // Log it, though — a persistently failing cache silently re-spends tokens every load.
        try {
            summaries.upsert(flavorName, id, jsonResponse, title);
        } catch (Exception e) {
            LOG.warn("Could not cache the summary for {}", id, e);
        }
        return jsonResponse;
    }

    private List<AnalysisResponse> analyzeChunksInParallel(
        String key,
        List<List<String>> optimalChunks
    ) throws Exception {
        List<Future<AnalysisResponse>> futures = new ArrayList<>();
        AtomicInteger completed = new AtomicInteger(0);

        // Limit to 20 concurrent LLM requests, as we have drastically reduced chunk count
        Semaphore rateLimitSemaphore = new Semaphore(20);

        for (List<String> chunkLines : optimalChunks) {
            futures.add(
                executor.submit(() -> {
                    try {
                        rateLimitSemaphore.acquire();
                    } catch (InterruptedException e) {
                        Thread.currentThread().interrupt();
                        return null;
                    }

                    try {
                        // The transcript was pushed by whoever could reach the ingest API: fence it
                        // as untrusted data the model must describe rather than obey.
                        String chunk = UntrustedText.fencedTranscript(
                            String.join("\n", chunkLines)
                        );
                        AnalysisResponse seqResponse = null;
                        try {
                            seqResponse = analyzerService.analyze(chunk);
                        } catch (Exception e) {
                            // Ignore unparseable chunk
                            LOG.warn("Failed to parse chunk from LLM", e);
                        }

                        int comp = completed.incrementAndGet();
                        // Scale parallel processing to 90% of the total progress
                        int pct = (int) Math.round((comp * 90.0) / optimalChunks.size());
                        String phaseMsg =
                            "Processing chunk " + comp + " of " + optimalChunks.size() + "...";

                        // Prevent progress moving backwards if futures finish out of order
                        progressMap.compute(
                            key,
                            (k, v) -> {
                                if (v == null || pct > v.progress()) {
                                    return new ProgressState(pct, phaseMsg);
                                }
                                return v;
                            }
                        );

                        return seqResponse;
                    } finally {
                        rateLimitSemaphore.release();
                    }
                })
            );
        }

        List<AnalysisResponse> seqResponses = new ArrayList<>();
        for (Future<AnalysisResponse> f : futures) {
            AnalysisResponse r = f.get();
            if (r != null) seqResponses.add(r);
        }
        return seqResponses;
    }

    /**
     * Nudges the progress bar one percent every two seconds, from 90 up to at most 99, while the
     * final consolidation call is in flight. The returned future must be cancelled once
     * consolidation finishes (or fails).
     */
    private ScheduledFuture<?> startConsolidationTicker(String key) {
        AtomicInteger fakePct = new AtomicInteger(90);
        return progressScheduler.scheduleAtFixedRate(
            () -> {
                int next = fakePct.get() + 1;
                if (next > 99) {
                    return;
                }
                progressMap.put(key, new ProgressState(next, "Consolidating final analysis..."));
                fakePct.set(next);
            },
            2000,
            2000,
            TimeUnit.MILLISECONDS
        );
    }

    private AnalysisResponse recursivelyConsolidate(
        List<AnalysisResponse> responses,
        ToIntFunction<String> tokenFn,
        ObjectMapper mapper,
        int maxTokens
    ) throws Exception {
        String json = mapper.writeValueAsString(responses);
        boolean withinBudget;
        try {
            withinBudget = tokenFn.applyAsInt(json) <= maxTokens;
        } catch (Exception e) {
            // Only token estimation is best-effort here; fall back to a char-length heuristic so a
            // failed estimate doesn't get mistaken for a failed consolidation.
            withinBudget = (json.length() / 4) <= maxTokens;
        }
        // A consolidation failure below propagates to the caller (which falls back to a local merge).
        if (withinBudget || responses.size() <= 1) {
            // These analyses were produced from ingested transcripts, so they carry the same taint.
            return analyzerService.consolidateAnalysis(UntrustedText.fencedAnalysis(json));
        }

        int mid = responses.size() / 2;
        AnalysisResponse r1 = recursivelyConsolidate(
            responses.subList(0, mid),
            tokenFn,
            mapper,
            maxTokens
        );
        AnalysisResponse r2 = recursivelyConsolidate(
            responses.subList(mid, responses.size()),
            tokenFn,
            mapper,
            maxTokens
        );
        return analyzerService.consolidateAnalysis(
            UntrustedText.fencedAnalysis(mapper.writeValueAsString(List.of(r1, r2)))
        );
    }

    /**
     * Deterministically merges per-chunk analyses without calling the LLM. Used as a fallback when
     * LLM consolidation fails, so the user still gets a usable (if less polished) summary instead of
     * an error.
     */
    private AnalysisResponse localMerge(List<AnalysisResponse> responses) {
        String shortTitle = "Session analysis";
        StringBuilder summary = new StringBuilder();
        List<String> flow = new ArrayList<>();
        List<AgentAction> actions = new ArrayList<>();
        List<Issue> issues = new ArrayList<>();
        List<String> recommendations = new ArrayList<>();

        for (AnalysisResponse r : responses) {
            if (r == null) continue;
            if (
                "Session analysis".equals(shortTitle) &&
                r.shortTitle() != null &&
                !r.shortTitle().isBlank()
            ) {
                shortTitle = r.shortTitle();
            }
            if (r.summary() != null && !r.summary().isBlank()) {
                if (summary.length() > 0) summary.append(" ");
                summary.append(r.summary().trim());
            }
            if (r.flow() != null) {
                for (String f : r.flow()) if (f != null && !flow.contains(f)) flow.add(f);
            }
            if (r.agentActions() != null) actions.addAll(r.agentActions());
            if (r.issues() != null) issues.addAll(r.issues());
            if (r.recommendations() != null) {
                for (String rec : r.recommendations()) {
                    if (rec != null && !recommendations.contains(rec)) recommendations.add(rec);
                }
            }
        }

        String summaryText = summary.length() > 4000
            ? summary.substring(0, 4000) + "..."
            : summary.toString();
        summaryText =
            "(Combined from " +
            responses.size() +
            " partial analyses; full consolidation was unavailable.) " +
            summaryText;

        return new AnalysisResponse(
            shortTitle,
            cap(flow, 40),
            cap(actions, 40),
            cap(issues, 40),
            cap(recommendations, 30),
            summaryText
        );
    }

    private static <T> List<T> cap(List<T> list, int max) {
        return list.size() > max ? new ArrayList<>(list.subList(0, max)) : list;
    }

    /**
     * Bounds and cleans a model reply before it is serialized, cached, and served. Only the shape is
     * normalized — control characters, unbounded lengths, and runaway list sizes — so the wording the
     * model chose is preserved. The list caps match the ones {@link #localMerge} already applies.
     */
    private static AnalysisResponse normalize(AnalysisResponse response) {
        if (response == null) return null;
        return new AnalysisResponse(
            UntrustedText.boundedLine(response.shortTitle(), MAX_TITLE_CHARS),
            normalizeEach(response.flow(), MAX_ITEMS, AnalysisOrchestrator::boundedItem),
            normalizeEach(
                response.agentActions(),
                MAX_ITEMS,
                action ->
                    action == null
                        ? null
                        : new AgentAction(
                            boundedItem(action.action()),
                            boundedItem(action.description())
                        )
            ),
            normalizeEach(
                response.issues(),
                MAX_ITEMS,
                issue ->
                    issue == null
                        ? null
                        : new Issue(boundedItem(issue.error()), boundedItem(issue.circumvention()))
            ),
            normalizeEach(
                response.recommendations(),
                MAX_RECOMMENDATIONS,
                AnalysisOrchestrator::boundedItem
            ),
            UntrustedText.boundedText(response.summary(), MAX_SUMMARY_CHARS)
        );
    }

    private static String boundedItem(String value) {
        return UntrustedText.boundedLine(value, MAX_ITEM_CHARS);
    }

    /** Applies {@code normalizer} to at most {@code max} items; a null list stays null. */
    private static <T> List<T> normalizeEach(List<T> list, int max, UnaryOperator<T> normalizer) {
        if (list == null) return null;
        List<T> normalized = new ArrayList<>(Math.min(list.size(), max));
        for (T item : list) {
            if (normalized.size() >= max) break;
            normalized.add(normalizer.apply(item));
        }
        return normalized;
    }
}
