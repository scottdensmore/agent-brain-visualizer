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

import jakarta.inject.Inject;
import jakarta.inject.Singleton;
import java.util.List;

/**
 * The "skill / AGENTS.md miner": gathers a source's sessions, mines structural patterns with the
 * pure {@link PatternMiner}, then (when AI is configured) runs a single {@link MinerAdvisorService}
 * pass to phrase that evidence into concrete skills, AGENTS.md rules, and tooling gaps.
 *
 * <p>Degrades gracefully: if AI is not configured, there is no evidence to work with, or the LLM
 * call fails, it still returns the structural evidence with {@link MiningReport#aiGenerated()} false
 * and an explanatory note, rather than erroring.
 */
@Singleton
public class MinerService {

    private final SessionCollector collector;
    private final MinerAdvisorService advisor;
    private final AiConfig aiConfig;

    @Inject
    public MinerService(
        SessionCollector collector,
        MinerAdvisorService advisor,
        AiConfig aiConfig
    ) {
        this.collector = collector;
        this.advisor = advisor;
        this.aiConfig = aiConfig;
    }

    public MiningReport forFlavor(String flavor) {
        SessionCollector.Collected collected = collector.collect(flavor);
        PatternMiner.Patterns p = PatternMiner.mine(collected.sessions());

        boolean hasEvidence =
            !p.toolSequences().isEmpty() ||
            !p.failureFixes().isEmpty() ||
            !p.recommendations().isEmpty();

        if (!hasEvidence) {
            return degraded(
                flavor,
                collected,
                p,
                "Not enough recurring patterns yet — run more sessions (and AI analysis on them) to mine skills."
            );
        }
        if (!aiConfig.isConfigured()) {
            return degraded(
                flavor,
                collected,
                p,
                "Showing structural evidence only. Configure an AI provider to generate skills and AGENTS.md rules."
            );
        }

        MiningProposal proposal;
        try {
            proposal = advisor.propose(buildEvidenceDigest(p));
        } catch (Exception e) {
            System.err.println("Miner advisor failed; returning evidence only: " + e.getMessage());
            return degraded(
                flavor,
                collected,
                p,
                "AI proposal was unavailable (the model did not respond). Showing structural evidence only."
            );
        }

        return new MiningReport(
            flavor,
            collected.totalSessionCount(),
            collected.sessions().size(),
            p.analyzedSessions(),
            true,
            "",
            p.toolSequences(),
            p.failureFixes(),
            p.recommendations(),
            orEmpty(proposal == null ? null : proposal.skills()),
            orEmpty(proposal == null ? null : proposal.agentsRules()),
            orEmpty(proposal == null ? null : proposal.toolingGaps())
        );
    }

    private MiningReport degraded(
        String flavor,
        SessionCollector.Collected collected,
        PatternMiner.Patterns p,
        String note
    ) {
        return new MiningReport(
            flavor,
            collected.totalSessionCount(),
            collected.sessions().size(),
            p.analyzedSessions(),
            false,
            note,
            p.toolSequences(),
            p.failureFixes(),
            p.recommendations(),
            List.of(),
            List.of(),
            List.of()
        );
    }

    /** Hard cap on how much of one ingested value is rendered into the prompt. */
    private static final int MAX_VALUE_CHARS = 200;

    /**
     * Renders one mined value as a single inert evidence item. Tool names and analysis text come
     * from whoever pushed the trajectory, and the advisor's proposals are exported as AGENTS.md
     * rules and skills, so an ingested string must not be able to shape the digest around it: line
     * breaks — which would forge extra evidence lines or a fake section header — collapse to spaces,
     * and the value is length-capped. The value itself is still passed through, just as data. Fence
     * lookalikes are handled once for the whole digest by {@link UntrustedText#fenced}.
     */
    private static String item(String text) {
        return text == null ? "" : UntrustedText.boundedLine(text, MAX_VALUE_CHARS);
    }

    /**
     * Renders the mined evidence into a compact prompt for the advisor, fenced as untrusted data.
     * The fence tag carries a per-call random nonce, so no ingested value can spell the closing
     * marker and promote itself to the instruction side of the prompt.
     */
    private String buildEvidenceDigest(PatternMiner.Patterns p) {
        StringBuilder sb = new StringBuilder();

        sb.append(
            "RECURRING TOOL SEQUENCES (consecutive tool calls, with the number of sessions):\n"
        );
        if (p.toolSequences().isEmpty()) {
            sb.append("- (none)\n");
        } else {
            for (NameCount s : p.toolSequences()) {
                sb
                    .append("- ")
                    .append(item(s.name()))
                    .append(" (")
                    .append(s.count())
                    .append(" sessions)\n");
            }
        }

        sb.append("\nRECURRING FAILURE → FIX PAIRS:\n");
        if (p.failureFixes().isEmpty()) {
            sb.append("- (none)\n");
        } else {
            for (FixPair f : p.failureFixes()) {
                String fix = item(f.fix());
                sb
                    .append("- Error: ")
                    .append(item(f.error()))
                    .append(" | Fix: ")
                    .append(fix.isBlank() ? "(not recorded)" : fix)
                    .append(" (")
                    .append(f.count())
                    .append(" sessions)\n");
            }
        }

        sb.append("\nRECOMMENDATIONS FROM PRIOR ANALYSES:\n");
        if (p.recommendations().isEmpty()) {
            sb.append("- (none)\n");
        } else {
            for (NameCount r : p.recommendations()) {
                sb
                    .append("- ")
                    .append(item(r.name()))
                    .append(" (")
                    .append(r.count())
                    .append(" sessions)\n");
            }
        }

        return UntrustedText.fenced(UntrustedText.EVIDENCE_TAG, sb.toString());
    }

    private static <T> List<T> orEmpty(List<T> list) {
        return list == null ? List.of() : list;
    }
}
