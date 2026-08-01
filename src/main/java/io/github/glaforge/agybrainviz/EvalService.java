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
import java.util.Comparator;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Future;
import java.util.concurrent.Semaphore;

/**
 * The analysis-quality eval harness: gathers a source's sessions via {@link SessionCollector}, grades
 * every session that already has a cached AI analysis with the deterministic {@link EvalScorer}, and
 * rolls the results into an {@link EvalReport} scoreboard.
 *
 * <p>The deterministic pass makes no LLM calls, so it is fast and reproducible; the report is
 * labelled with the active model so runs can be compared after a model/prompt change. When
 * {@code judge} is requested, an optional {@link AnalysisJudgeService} LLM pass additionally rates a
 * capped, parallelised sample of the analyses on a 1-5 rubric — degrading gracefully (rubric omitted
 * with an explanatory note) when AI is not configured or the model does not respond.
 */
@Singleton
public class EvalService {

    private static final int WORST_CASES = 10;
    /** Cap the (LLM) judged sample so the opt-in judge stays responsive on large histories. */
    static final int JUDGE_MAX_SESSIONS = 12;
    private static final int JUDGE_CONCURRENCY = 8;
    private static final int MAX_DIGEST_CHARS = 6000;
    private static final int MAX_ANALYSIS_CHARS = 6000;

    /**
     * The judge is a panel: each session is rated once per lens and the verdicts are averaged. Since
     * the model runs at temperature 0, distinct lenses (not naive re-sampling) are what yield genuine
     * diversity, so a single harsh or lenient framing can't dominate the score.
     */
    static final List<String> JUDGE_LENSES = List.of(
        "a strict, skeptical reviewer who demands explicit evidence for every claim",
        "a fair, balanced reviewer weighing the analysis's strengths and weaknesses",
        "a pragmatic engineer judging whether the analysis would actually help a teammate"
    );

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private final SessionCollector collector;
    private final AiConfig aiConfig;
    private final AnalysisJudgeService judgeService;
    private final ExecutorService executor;

    @Inject
    public EvalService(
        SessionCollector collector,
        AiConfig aiConfig,
        AnalysisJudgeService judgeService,
        @Named(TaskExecutors.IO) ExecutorService executor
    ) {
        this.collector = collector;
        this.aiConfig = aiConfig;
        this.judgeService = judgeService;
        this.executor = executor;
    }

    /** One evaluated session with everything the judge needs, kept alongside its graded result. */
    private record Judgeable(String id, String title, List<JsonNode> steps, JsonNode analysis) {}

    public EvalReport forFlavor(String flavor, boolean judge) {
        SessionCollector.Collected collected = collector.collect(flavor);

        List<EvalCaseResult> results = new ArrayList<>();
        List<Judgeable> judgeables = new ArrayList<>();
        for (FleetInsights.Session session : collected.sessions()) {
            JsonNode analysis = session.cachedSummary();
            if (analysis == null || analysis.isMissingNode() || analysis.isNull()) continue;
            String id = session.id() == null ? "unknown" : session.id();
            EvalCaseResult result = EvalScorer.score(id, session.steps(), analysis);
            results.add(result);
            judgeables.add(new Judgeable(id, result.title(), session.steps(), analysis));
        }

        double avgScore = results.isEmpty()
            ? 0.0
            : round1(results.stream().mapToInt(EvalCaseResult::score).average().orElse(0.0));

        List<NameCount> passRates = new ArrayList<>();
        for (String check : EvalScorer.checkNames()) {
            int passing = (int) results.stream().filter(r -> r.passed().contains(check)).count();
            passRates.add(new NameCount(check, passing));
        }

        List<EvalCaseResult> worst = results
            .stream()
            .sorted(Comparator.comparingInt(EvalCaseResult::score))
            .limit(WORST_CASES)
            .toList();

        return new EvalReport(
            flavor,
            collected.totalSessionCount(),
            collected.sessions().size(),
            results.size(),
            avgScore,
            modelLabel(),
            passRates,
            worst,
            runJudge(judge, judgeables)
        );
    }

    private JudgeSummary runJudge(boolean requested, List<Judgeable> judgeables) {
        if (!requested) {
            return JudgeSummary.notRun(
                "Run the LLM judge to add faithfulness / actionability / clarity scores."
            );
        }
        if (judgeables.isEmpty()) {
            return JudgeSummary.notRun("No analyzed sessions to judge yet.");
        }
        if (!aiConfig.isConfigured()) {
            return JudgeSummary.notRun("Configure an AI provider to run the LLM judge.");
        }

        List<Judgeable> sample = judgeables.subList(
            0,
            Math.min(JUDGE_MAX_SESSIONS, judgeables.size())
        );

        // Submit every (session, lens) task up front for maximum parallelism under the semaphore,
        // then gather each session's panel and ensemble it into one case.
        Semaphore rateLimit = new Semaphore(JUDGE_CONCURRENCY);
        List<List<Future<JudgeScore>>> panels = new ArrayList<>();
        for (Judgeable j : sample) {
            List<Future<JudgeScore>> panel = new ArrayList<>();
            for (String lens : JUDGE_LENSES) {
                panel.add(executor.submit(() -> judgeWithLens(j, lens, rateLimit)));
            }
            panels.add(panel);
        }

        List<JudgedCase> cases = new ArrayList<>();
        for (int i = 0; i < sample.size(); i++) {
            List<JudgeScore> verdicts = new ArrayList<>();
            for (Future<JudgeScore> f : panels.get(i)) {
                try {
                    JudgeScore verdict = f.get();
                    if (verdict != null) verdicts.add(verdict);
                } catch (Exception e) {
                    // A single lens failing must not sink the session's case; skip it.
                }
            }
            if (!verdicts.isEmpty()) cases.add(ensemble(sample.get(i), verdicts));
        }

        if (cases.isEmpty()) {
            return JudgeSummary.notRun(
                "The LLM judge was unavailable (the model did not respond). Showing deterministic checks only."
            );
        }

        return new JudgeSummary(
            true,
            "",
            cases.size(),
            round1(cases.stream().mapToDouble(JudgedCase::faithfulness).average().orElse(0.0)),
            round1(cases.stream().mapToDouble(JudgedCase::actionability).average().orElse(0.0)),
            round1(cases.stream().mapToDouble(JudgedCase::clarity).average().orElse(0.0)),
            cases
        );
    }

    /** One panelist's clamped verdict for a session, or null if the model call failed. */
    private JudgeScore judgeWithLens(Judgeable j, String lens, Semaphore rateLimit) {
        try {
            rateLimit.acquire();
        } catch (InterruptedException e) {
            Thread.currentThread().interrupt();
            return null;
        }
        try {
            return clamp(
                judgeService.judge(
                    // Both the digest and the analysis derive from a transcript pushed by whoever
                    // could reach the ingest API: fence them as data the judge must rate, not obey.
                    UntrustedText.fencedTranscript(buildDigest(j.steps())),
                    UntrustedText.fencedAnalysis(cap(toJson(j.analysis()), MAX_ANALYSIS_CHARS)),
                    lens
                )
            );
        } catch (Exception e) {
            System.err.println("Judge lens failed for " + j.id() + ": " + e.getMessage());
            return null;
        } finally {
            rateLimit.release();
        }
    }

    /** Averages a session's panel verdicts (per dimension) into one case, keeping the panel spread. */
    private static JudgedCase ensemble(Judgeable j, List<JudgeScore> verdicts) {
        String comment = verdicts
            .stream()
            .map(JudgeScore::comment)
            .filter(c -> c != null && !c.isBlank())
            .findFirst()
            .orElse("");
        // Each panelist's overall opinion (mean of its three dimensions); the min/max shows how much
        // the lenses disagreed for this session.
        double[] overalls = verdicts
            .stream()
            .mapToDouble(v -> (v.faithfulness() + v.actionability() + v.clarity()) / 3.0)
            .toArray();
        double panelMin = round1(java.util.Arrays.stream(overalls).min().orElse(0.0));
        double panelMax = round1(java.util.Arrays.stream(overalls).max().orElse(0.0));
        return new JudgedCase(
            j.id(),
            j.title(),
            round1(verdicts.stream().mapToInt(JudgeScore::faithfulness).average().orElse(0.0)),
            round1(verdicts.stream().mapToInt(JudgeScore::actionability).average().orElse(0.0)),
            round1(verdicts.stream().mapToInt(JudgeScore::clarity).average().orElse(0.0)),
            verdicts.size(),
            panelMin,
            panelMax,
            comment
        );
    }

    /** A compact "what happened" digest — user asks, tool calls, and errors — for the judge. */
    private String buildDigest(List<JsonNode> steps) {
        StringBuilder sb = new StringBuilder();
        for (JsonNode step : steps) {
            if (sb.length() >= MAX_DIGEST_CHARS) {
                sb.append("...(truncated)\n");
                break;
            }
            JsonNode toolCalls = step.path("tool_calls");
            String type = step.path("type").asText("");
            if (toolCalls.isArray() && toolCalls.size() > 0) {
                List<String> names = new ArrayList<>();
                for (JsonNode t : toolCalls) names.add(t.path("name").asText("unknown"));
                sb.append("TOOL: ").append(String.join(", ", names)).append("\n");
            } else if (FleetInsights.isError(step)) {
                String err = step.has("error")
                    ? step.path("error").asText("")
                    : step.path("content").asText("");
                sb.append("ERROR: ").append(cap(err.strip(), 200)).append("\n");
            } else if ("USER_INPUT".equals(type)) {
                String content = step.path("content").asText("").strip();
                if (!content.isBlank()) sb.append("USER: ").append(cap(content, 300)).append("\n");
            }
        }
        return sb.length() == 0 ? "(no significant steps)" : sb.toString();
    }

    private static JudgeScore clamp(JudgeScore s) {
        return new JudgeScore(
            clamp(s.faithfulness()),
            clamp(s.actionability()),
            clamp(s.clarity()),
            s.comment()
        );
    }

    private static int clamp(int v) {
        return Math.max(1, Math.min(5, v));
    }

    private static String toJson(JsonNode node) {
        try {
            return MAPPER.writeValueAsString(node);
        } catch (Exception e) {
            return String.valueOf(node);
        }
    }

    private static String cap(String s, int max) {
        if (s == null) return "";
        return s.length() > max ? s.substring(0, max) : s;
    }

    private String modelLabel() {
        return aiConfig.provider() == AiConfig.Provider.OLLAMA
            ? "ollama · " + aiConfig.ollamaModel()
            : "gemini · " + aiConfig.geminiModel();
    }

    private static double round1(double value) {
        return Math.round(value * 10.0) / 10.0;
    }
}
