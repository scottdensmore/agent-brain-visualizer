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
import static org.junit.jupiter.api.Assertions.assertNotEquals;
import static org.junit.jupiter.api.Assertions.assertTrue;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;
import java.io.ByteArrayOutputStream;
import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.List;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;

/** Tests that pushed, tool-native transcripts are normalized and de-duplicated on the way in. */
class IngestorTest extends PostgresTest {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    private Ingestor ingestor;

    private SummaryRepository summaries;
    private SessionFileRepository sessionFiles;

    @BeforeEach
    void setUp() throws SQLException {
        SessionRepository sessions = new SessionRepository(dataSource());
        summaries = new SummaryRepository(dataSource());
        sessionFiles = new SessionFileRepository(dataSource());
        ingestor =
            new Ingestor(
                List.of(
                    new AntigravityNormalizer(),
                    new CodexNormalizer(),
                    new ClaudeCodeNormalizer()
                ),
                sessions,
                summaries,
                sessionFiles
            );
        truncate("sessions");
        truncate("summaries");
        truncate("session_files");
    }

    private static final String ANTIGRAVITY_RAW =
        "{\"type\":\"USER_INPUT\",\"content\":\"<USER_REQUEST>\\nfix the build\\n</USER_REQUEST>\",\"created_at\":\"2026-06-19T10:00:00Z\"}\n" +
        "{\"type\":\"MESSAGE\",\"source\":\"MODEL\",\"content\":\"done\",\"created_at\":\"2026-06-19T10:01:00Z\"}\n";

    private static final String CLAUDE_RAW =
        "{\"type\":\"user\",\"message\":{\"content\":\"add a test\"},\"timestamp\":\"2026-06-19T10:00:00Z\"}\n" +
        "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]},\"timestamp\":\"2026-06-19T10:01:00Z\"}\n";

    private IngestSession push(String source, String id, String raw) {
        return new IngestSession(source, id, null, 1_700_000_000_000L, raw, null);
    }

    @Test
    void storesAPushedCachedSummaryAlongsideTheSession() {
        ingestor.ingest(
            List.of(
                new IngestSession(
                    "antigravity-cli",
                    "s1",
                    null,
                    1L,
                    ANTIGRAVITY_RAW,
                    "{\"shortTitle\":\"t\",\"summary\":\"cached\"}"
                )
            )
        );
        assertTrue(summaries.find("antigravity-cli", "s1").orElse("").contains("cached"));
    }

    @Test
    void aPushWithoutASummaryLeavesNoCachedAnalysis() {
        ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)));
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty());
    }

    @Test
    void ingestsASummaryOnItsOwnForAnAlreadyStoredSession() {
        // The post-hoc case: the transcript is already in the store, and a summary shows up later.
        ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)));
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty());

        IngestResult result = ingestor.ingestSummaries(
            List.of(new IngestSummary("antigravity-cli", "s1", "{\"summary\":\"late\"}"))
        );

        assertEquals(new IngestResult(1, 0, 0), result);
        assertTrue(summaries.find("antigravity-cli", "s1").orElse("").contains("late"));
    }

    @Test
    void reIngestingAnUnchangedSummaryIsSkipped() {
        IngestSummary s = new IngestSummary("antigravity-cli", "s1", "{\"summary\":\"same\"}");
        assertEquals(new IngestResult(1, 0, 0), ingestor.ingestSummaries(List.of(s)));
        assertEquals(new IngestResult(0, 1, 0), ingestor.ingestSummaries(List.of(s)));
    }

    @Test
    void aSummaryForAnUnknownSourceOrWithNoBodyIsCountedAsFailed() {
        assertEquals(
            new IngestResult(0, 0, 1),
            ingestor.ingestSummaries(List.of(new IngestSummary("borg", "s1", "{}")))
        );
        assertEquals(
            new IngestResult(0, 0, 1),
            ingestor.ingestSummaries(List.of(new IngestSummary("antigravity-cli", "s1", "  ")))
        );
    }

    @Test
    void aNonJsonSummaryBodyIsFailedWithoutCrashingTheBatch() {
        // A non-JSON body can't go in the jsonb column; reject it as this item's failure (caught up
        // front, so a genuine store outage still propagates rather than looking like a per-item fail).
        IngestResult result = ingestor.ingestSummaries(
            List.of(
                new IngestSummary("antigravity-cli", "s1", "not json"),
                new IngestSummary("antigravity-cli", "s2", "{\"summary\":\"ok\"}")
            )
        );
        assertEquals(new IngestResult(1, 0, 1), result);
        assertTrue(summaries.find("antigravity-cli", "s2").isPresent());
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty());
    }

    @Test
    void aMalformedSummaryDoesNotCrashTheBatchAndTheSessionStillIngests() {
        // The summary lands in a jsonb column, so non-JSON would fail the insert. One bad summary
        // must not take down the session it rode in with, nor the rest of the batch.
        IngestResult result = ingestor.ingest(
            List.of(
                new IngestSession("antigravity-cli", "s1", null, 1L, ANTIGRAVITY_RAW, "not json"),
                new IngestSession("antigravity-cli", "s2", null, 1L, ANTIGRAVITY_RAW, null)
            )
        );

        assertEquals(new IngestResult(2, 0, 0), result);
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty()); // the bad summary was dropped
    }

    /**
     * A stored summary is read back by every cross-session endpoint, up to 150 at a time, so an
     * oversized one degrades far more than the request that pushed it — it keeps degrading until the
     * row is deleted by hand. Both routes that accept a summary have to bound it.
     */
    private static String oversizedSummary() {
        return "{\"summary\":\"" + "a".repeat(Ingestor.MAX_SUMMARY_CHARS) + "\"}";
    }

    @Test
    void anOversizedSummaryIsRejectedOnItsOwnEndpoint() {
        IngestResult result = ingestor.ingestSummaries(
            List.of(new IngestSummary("antigravity-cli", "s1", oversizedSummary()))
        );

        assertEquals(new IngestResult(0, 0, 1), result);
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty());
    }

    @Test
    void anOversizedSummaryRidingWithATrajectoryIsDroppedButTheSessionStillIngests() {
        // Same bound, different consequence: the transcript is what matters, so it is kept.
        IngestResult result = ingestor.ingest(
            List.of(
                new IngestSession(
                    "antigravity-cli",
                    "s1",
                    null,
                    1L,
                    ANTIGRAVITY_RAW,
                    oversizedSummary()
                )
            )
        );

        assertEquals(new IngestResult(1, 0, 0), result);
        assertTrue(summaries.find("antigravity-cli", "s1").isEmpty());
    }

    @Test
    void aSummaryAtTheLimitIsStillAccepted() {
        // The bound must not reject a legitimately large analysis; the server's own normalizer
        // produces roughly 120 KB in the worst case, well under the limit.
        String body = "{\"summary\":\"" + "a".repeat(Ingestor.MAX_SUMMARY_CHARS - 20) + "\"}";
        assertTrue(body.length() <= Ingestor.MAX_SUMMARY_CHARS);

        assertEquals(
            new IngestResult(1, 0, 0),
            ingestor.ingestSummaries(List.of(new IngestSummary("antigravity-cli", "s1", body)))
        );
        assertTrue(summaries.find("antigravity-cli", "s1").isPresent());
    }

    // ----- attached files, so the preview works off the machine that ran the agent -----

    private IngestSession pushWithFiles(String id, List<IngestFile> files) {
        return new IngestSession("antigravity-cli", id, null, 1L, ANTIGRAVITY_RAW, null, files);
    }

    @Test
    void storesTheFilesATrajectoryAttached() {
        ingestor.ingest(
            List.of(
                pushWithFiles(
                    "s1",
                    List.of(new IngestFile("/home/dev/project/Parser.java", "class Parser {}"))
                )
            )
        );

        assertEquals(
            "class Parser {}",
            sessionFiles.find("antigravity-cli", "s1", "/home/dev/project/Parser.java").orElse(null)
        );
    }

    @Test
    void reIngestingReplacesTheAttachedFilesRatherThanAccumulating() {
        // The transcript decides which files it references, so one no longer mentioned must stop
        // being served rather than linger from an earlier push.
        ingestor.ingest(List.of(pushWithFiles("s1", List.of(new IngestFile("/a.txt", "first")))));
        ingestor.ingest(List.of(pushWithFiles("s1", List.of(new IngestFile("/b.txt", "second")))));

        assertTrue(sessionFiles.find("antigravity-cli", "s1", "/a.txt").isEmpty());
        assertEquals("second", sessionFiles.find("antigravity-cli", "s1", "/b.txt").orElse(null));
    }

    @Test
    void anOversizedAttachmentIsDroppedButTheRestOfTheSessionSurvives() {
        // The client bounds these too, but a client is not something the server gets to trust.
        ingestor.ingest(
            List.of(
                pushWithFiles(
                    "s1",
                    List.of(
                        new IngestFile("/huge.log", "x".repeat(Ingestor.MAX_FILE_CHARS + 1)),
                        new IngestFile("/small.txt", "kept")
                    )
                )
            )
        );

        assertTrue(sessionFiles.find("antigravity-cli", "s1", "/huge.log").isEmpty());
        assertEquals("kept", sessionFiles.find("antigravity-cli", "s1", "/small.txt").orElse(null));
    }

    @Test
    void aPushCarryingNoFilesLeavesNoAttachments() {
        ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)));
        assertTrue(sessionFiles.find("antigravity-cli", "s1", "/anything").isEmpty());
    }

    private String storedSteps(String source, String id) throws SQLException {
        try (
            Connection c = dataSource().getConnection();
            PreparedStatement s = c.prepareStatement(
                "SELECT steps FROM sessions WHERE source=? AND id=?"
            )
        ) {
            s.setString(1, source);
            s.setString(2, id);
            try (ResultSet rs = s.executeQuery()) {
                return rs.next() ? rs.getString(1) : null;
            }
        }
    }

    private String storedTitle(String source, String id) throws SQLException {
        try (
            Connection c = dataSource().getConnection();
            PreparedStatement s = c.prepareStatement(
                "SELECT title FROM sessions WHERE source=? AND id=?"
            )
        ) {
            s.setString(1, source);
            s.setString(2, id);
            try (ResultSet rs = s.executeQuery()) {
                return rs.next() ? rs.getString(1) : null;
            }
        }
    }

    @Test
    void storesAntigravityTranscriptAsNormalizedSteps() throws Exception {
        assertEquals(
            new IngestResult(1, 0, 0),
            ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)))
        );

        JsonNode steps = MAPPER.readTree(storedSteps("antigravity-cli", "s1"));
        assertTrue(steps.isArray());
        assertEquals(2, steps.size());
        assertEquals("USER_INPUT", steps.get(0).get("type").asText());
    }

    @Test
    void convertsClaudeCodeTranscriptIntoTheSharedStepSchema() throws Exception {
        ingestor.ingest(List.of(push("claude-code", "c1", CLAUDE_RAW)));

        JsonNode steps = MAPPER.readTree(storedSteps("claude-code", "c1"));
        // The client pushed Claude Code's own schema; the store holds the timeline schema.
        assertEquals("USER_INPUT", steps.get(0).get("type").asText());
        assertEquals("MESSAGE", steps.get(1).get("type").asText());
        assertEquals("MODEL", steps.get(1).get("source").asText());
    }

    @Test
    void derivesATitleFromTheTranscriptWhenTheClientSendsNone() throws Exception {
        ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)));
        // The <USER_REQUEST> wrapper is stripped.
        assertEquals("fix the build", storedTitle("antigravity-cli", "s1"));
    }

    @Test
    void theClientsTitleWinsOverTheDerivedOne() throws Exception {
        ingestor.ingest(
            List.of(
                new IngestSession(
                    "antigravity-cli",
                    "s1",
                    "  Explicit  ",
                    1L,
                    ANTIGRAVITY_RAW,
                    null
                )
            )
        );
        assertEquals("Explicit", storedTitle("antigravity-cli", "s1"));
    }

    @Test
    void fallsBackToAStableTitleWhenTheTranscriptRevealsNone() throws Exception {
        ingestor.ingest(List.of(push("antigravity-cli", "abcdef123456", "")));
        assertEquals(
            "antigravity-cli session abcdef12",
            storedTitle("antigravity-cli", "abcdef123456")
        );
    }

    @Test
    void rePushingTheSameTrajectorySkipsRatherThanDuplicating() {
        assertEquals(
            new IngestResult(1, 0, 0),
            ingestor.ingest(List.of(push("codex", "x", "{\"type\":\"session_meta\"}\n")))
        );
        // Idempotent: the CLI can be run on a cron without growing the table.
        assertEquals(
            new IngestResult(0, 1, 0),
            ingestor.ingest(List.of(push("codex", "x", "{\"type\":\"session_meta\"}\n")))
        );
    }

    @Test
    void aChangedTranscriptIsIngestedAgain() {
        ingestor.ingest(List.of(push("antigravity-cli", "s1", ANTIGRAVITY_RAW)));
        // A session grows as the agent keeps working; the newer content must land.
        String longer =
            ANTIGRAVITY_RAW +
            "{\"type\":\"MESSAGE\",\"source\":\"MODEL\",\"content\":\"more\",\"created_at\":\"2026-06-19T10:02:00Z\"}\n";
        assertEquals(
            new IngestResult(1, 0, 0),
            ingestor.ingest(List.of(push("antigravity-cli", "s1", longer)))
        );
    }

    @Test
    void rejectsUnknownSourcesAndMissingIdsWithoutFailingTheBatch() {
        IngestResult result = ingestor.ingest(
            java.util.Arrays.asList(
                push("borg", "s1", ANTIGRAVITY_RAW), // unknown source
                push("antigravity-cli", "", ANTIGRAVITY_RAW), // no id
                null,
                push("antigravity-cli", "good", ANTIGRAVITY_RAW)
            )
        );
        // One bad entry must not lose the rest of a batch.
        assertEquals(new IngestResult(1, 0, 3), result);
    }

    @Test
    void aPushedIdCannotForgeALogRecordOfItsOwn() {
        // id and source come straight from the body of an (optionally unauthenticated) ingest POST,
        // and the console appender ends a record at a newline, so a raw line break in either would
        // let a client write log lines of its own choosing around the one the server meant to write.
        String forged = "s1\n00:00:00.000 [main] WARN  i.g.g.a.Ingestor - nothing to see here";
        String logged = captureConsole(() -> ingestor.ingest(List.of(push("borg", forged, ""))));

        assertTrue(logged.contains("Rejecting trajectory"), "the rejection must still be logged");
        List<String> forgedLines = logged
            .lines()
            .filter(line -> line.contains("nothing to see here"))
            .toList();
        assertEquals(1, forgedLines.size(), "the pushed id must not span log lines:\n" + logged);
        assertTrue(
            forgedLines.get(0).contains("Rejecting trajectory"),
            "the pushed id forged a log record of its own:\n" + logged
        );
        assertTrue(logged.contains("s1\\n00:00:00.000"), "the line break should read as an escape");
    }

    /** Runs {@code body} with the console appender's output captured. */
    private static String captureConsole(Runnable body) {
        PrintStream original = System.out;
        ByteArrayOutputStream captured = new ByteArrayOutputStream();
        System.setOut(new PrintStream(captured, true, StandardCharsets.UTF_8));
        try {
            body.run();
        } finally {
            System.out.flush();
            System.setOut(original);
        }
        return captured.toString(StandardCharsets.UTF_8);
    }

    @Test
    void skipsMalformedTranscriptLinesRatherThanStoringUnparseableSteps() throws Exception {
        String raw = "{\"type\":\"USER_INPUT\",\"content\":\"hi\"}\nnot json at all\n";
        ingestor.ingest(List.of(push("antigravity-cli", "s1", raw)));

        JsonNode steps = MAPPER.readTree(storedSteps("antigravity-cli", "s1"));
        assertEquals(1, steps.size());
    }

    @Test
    void hashesTheTranscriptTheServerReceivedRatherThanTrustingTheClient() {
        // Same bytes hash the same; different bytes do not. The CLI reproduces this over the file.
        assertEquals(Ingestor.sha256("abc"), Ingestor.sha256("abc"));
        assertNotEquals(Ingestor.sha256("abc"), Ingestor.sha256("abd"));
        assertEquals(
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
            Ingestor.sha256("abc")
        );
    }

    @Test
    void aZeroMtimeBecomesNowSoTheSessionStillSortsToTheTop() throws SQLException {
        long before = System.currentTimeMillis();
        ingestor.ingest(
            List.of(new IngestSession("antigravity-cli", "s1", null, 0L, ANTIGRAVITY_RAW, null))
        );
        try (
            Connection c = dataSource().getConnection();
            PreparedStatement s = c.prepareStatement(
                "SELECT source_mtime FROM sessions WHERE id='s1'"
            )
        ) {
            try (ResultSet rs = s.executeQuery()) {
                assertTrue(rs.next());
                assertTrue(rs.getLong(1) >= before, "a 0 mtime must not sort to the epoch");
            }
        }
    }
}
