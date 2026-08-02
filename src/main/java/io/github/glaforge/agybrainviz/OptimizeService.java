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

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import io.micronaut.scheduling.TaskExecutors;
import jakarta.inject.Inject;
import jakarta.inject.Named;
import jakarta.inject.Singleton;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Future;
import java.util.concurrent.Semaphore;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * The "prompt lab": eval-driven prompt optimization. Given two analysis instructions, it re-analyzes
 * a small sample of a source's sessions with each and scores the outputs with the deterministic
 * {@link EvalScorer}, so a human can see which prompt variant produces better analyses. The eval is
 * the fitness function; the sample and LLM calls are bounded so the run stays cheap and responsive.
 */
@Singleton
public class OptimizeService {

    private static final Logger LOG = LoggerFactory.getLogger(OptimizeService.class);

    /** The baseline analysis instruction the current pipeline effectively uses (variant A default). */
    public static final String DEFAULT_INSTRUCTION = """
        Analyze the following coding-agent session transcript. Extract, into the structured format:
        a very short title; the key flow of what happened; the agent's notable actions; the issues or
        errors it hit and how it worked around them; concrete, actionable recommendations (e.g. missing
        CLI tools, skills to create, or AGENTS.md advice); and a short (max 3 paragraph) summary.""";

    static final int MAX_SAMPLE = 5;
    private static final int CONCURRENCY = 6;
    private static final int MAX_TRANSCRIPT_CHARS = 12000;

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final SessionCollector collector;
    private final VariantAnalyzerService analyzer;
    private final AiConfig aiConfig;
    private final ExecutorService executor;

    @Inject
    public OptimizeService(
        SessionCollector collector,
        VariantAnalyzerService analyzer,
        AiConfig aiConfig,
        @Named(TaskExecutors.IO) ExecutorService executor
    ) {
        this.collector = collector;
        this.analyzer = analyzer;
        this.aiConfig = aiConfig;
        this.executor = executor;
    }

    public OptimizeReport compare(
        String flavor,
        int sampleSize,
        String instructionA,
        String instructionB
    ) {
        if (!aiConfig.isConfigured()) {
            return OptimizeReport.unavailable("Configure an AI provider to run the prompt lab.");
        }
        String a = blankTo(instructionA, DEFAULT_INSTRUCTION);
        String b = blankTo(instructionB, DEFAULT_INSTRUCTION);
        int n = Math.max(1, Math.min(MAX_SAMPLE, sampleSize));

        List<FleetInsights.Session> sample = collector
            .collect(flavor)
            .sessions()
            .stream()
            .filter(s -> !s.steps().isEmpty())
            .limit(n)
            .toList();
        if (sample.isEmpty()) {
            return OptimizeReport.unavailable("No sessions with a transcript to analyze here.");
        }

        // Submit both variants for every sampled session up front, bounded by the semaphore.
        Semaphore rateLimit = new Semaphore(CONCURRENCY);
        List<Future<EvalCaseResult>> futuresA = new ArrayList<>();
        List<Future<EvalCaseResult>> futuresB = new ArrayList<>();
        for (FleetInsights.Session session : sample) {
            String transcript = transcriptText(session.steps());
            futuresA.add(executor.submit(() -> runVariant(session, a, transcript, rateLimit)));
            futuresB.add(executor.submit(() -> runVariant(session, b, transcript, rateLimit)));
        }

        return new OptimizeReport(
            sample.size(),
            "",
            aggregate(collect(futuresA)),
            aggregate(collect(futuresB))
        );
    }

    /** Re-analyzes one session with an instruction and scores the result, or null on failure. */
    private EvalCaseResult runVariant(
        FleetInsights.Session session,
        String instruction,
        String transcript,
        Semaphore rateLimit
    ) {
        try {
            rateLimit.acquire();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return null;
        }
        try {
            // The transcript was pushed by whoever could reach the ingest API: fence it as untrusted
            // data the model must describe rather than obey.
            AnalysisResponse response = analyzer.analyzeWithInstruction(
                instruction,
                UntrustedText.fencedTranscript(transcript)
            );
            JsonNode analysis = MAPPER.valueToTree(response);
            String id = session.id() == null ? "unknown" : session.id();
            return EvalScorer.score(id, session.steps(), analysis);
        } catch (Exception e) {
            // The message can quote ingested text back, so escape it and use the logger rather than
            // raw stderr, which no appender formats.
            LOG.warn("Prompt-lab analysis failed: {}", LogSafe.escape(e.getMessage()));
            return null;
        } finally {
            rateLimit.release();
        }
    }

    private static List<EvalCaseResult> collect(List<Future<EvalCaseResult>> futures) {
        List<EvalCaseResult> results = new ArrayList<>();
        for (Future<EvalCaseResult> f : futures) {
            try {
                EvalCaseResult r = f.get();
                if (r != null) results.add(r);
            } catch (Exception e) {
                // A single failure must not sink the whole comparison; skip it.
            }
        }
        return results;
    }

    private static VariantResult aggregate(List<EvalCaseResult> results) {
        if (results.isEmpty()) return VariantResult.empty();
        double avg = round1(results.stream().mapToInt(EvalCaseResult::score).average().orElse(0.0));
        Map<String, Integer> passing = new LinkedHashMap<>();
        for (String check : EvalScorer.checkNames()) {
            int count = (int) results.stream().filter(r -> r.passed().contains(check)).count();
            passing.put(check, count);
        }
        List<NameCount> passRates = new ArrayList<>();
        passing.forEach((name, count) -> passRates.add(new NameCount(name, count)));
        return new VariantResult(avg, results.size(), passRates);
    }

    /** A bounded, plain-text rendering of a session's steps for a single-shot analysis call. */
    private String transcriptText(List<JsonNode> steps) {
        StringBuilder sb = new StringBuilder();
        for (JsonNode step : steps) {
            if (sb.length() >= MAX_TRANSCRIPT_CHARS) {
                sb.append("...(truncated)\n");
                break;
            }
            String type = step.path("type").asText("");
            JsonNode toolCalls = step.path("tool_calls");
            if (toolCalls.isArray() && toolCalls.size() > 0) {
                List<String> names = new ArrayList<>();
                for (JsonNode t : toolCalls) names.add(t.path("name").asText("unknown"));
                sb.append("TOOL_CALL: ").append(String.join(", ", names)).append("\n");
            } else if (FleetInsights.isError(step)) {
                String err = step.has("error")
                    ? step.path("error").asText("")
                    : step.path("content").asText("");
                sb.append("ERROR: ").append(cap(err.strip(), 400)).append("\n");
            } else if ("USER_INPUT".equals(type)) {
                String content = step.path("content").asText("").strip();
                if (!content.isBlank()) sb.append("USER: ").append(cap(content, 500)).append("\n");
            } else {
                String content = step.path("content").asText("").strip();
                if (!content.isBlank()) sb.append("AGENT: ").append(cap(content, 500)).append("\n");
            }
        }
        return sb.length() == 0 ? "(empty transcript)" : sb.toString();
    }

    private static String cap(String s, int max) {
        if (s == null) return "";
        return s.length() > max ? s.substring(0, max) : s;
    }

    private static String blankTo(String value, String fallback) {
        return (value == null || value.isBlank()) ? fallback : value;
    }

    private static double round1(double value) {
        return Math.round(value * 10.0) / 10.0;
    }
}
