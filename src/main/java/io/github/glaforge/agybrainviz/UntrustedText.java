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

import java.security.SecureRandom;
import java.util.HexFormat;
import java.util.regex.Pattern;

/**
 * Renders text that crossed an untrusted boundary: an ingested transcript on its way into an LLM
 * prompt ({@link #fencedTranscript}, {@link #fencedAnalysis}), and a model reply on its way into the
 * summary cache ({@link #boundedLine}, {@link #boundedText}).
 *
 * <p>Transcripts — and the analyses derived from them — are pushed by whoever can reach {@code
 * /api/ingest/sessions}, so everything they contain must reach the model as data, never as
 * instructions. A fenced value carries its own delimiters:
 *
 * <pre>
 * &lt;untrusted_transcript_5f2c9a03d1b74e68&gt;
 * …the ingested text…
 * &lt;/untrusted_transcript_5f2c9a03d1b74e68&gt;
 * </pre>
 *
 * <p>The tag ends in a fresh {@link SecureRandom} nonce, generated per call and never reused, so
 * ingested text — written long before that nonce existed — cannot spell the closing marker whatever
 * lookalike it tries: {@code </untrusted-transcript>}, a zero-width space inside the word, or a
 * Cyrillic/fullwidth homoglyph all fail to match it. A guessable fence would be worse than none at
 * all here, because the prompts tell the model that the fenced region is data and the surrounding
 * message is instruction — so a forged close would promote attacker text to the instruction side.
 *
 * <p>Defence in depth, applied to the fenced text before it is wrapped: invisible format characters
 * (zero-width space/joiner, soft hyphen, BOM, the U+E00xx tag block) are dropped, other control and
 * line-separator characters collapse to spaces, and anything shaped like a fence marker is replaced
 * with a placeholder, so no lookalike marker reaches the model to confuse it. Replacing that shape
 * costs a little fidelity — a transcript that genuinely discusses "untrusted input" reaches the
 * model as "[marker removed]" — which is worth it for text that only ever feeds a prompt.
 *
 * <p>Unlike the miner's one-line evidence items, a transcript is legitimately multi-line and must
 * stay readable as one, so newlines and tabs are preserved on the way into a prompt. {@link
 * #fenced(String, String)} takes the tag as a parameter so any other untrusted prompt slot (the
 * miner's evidence block, say) can reuse the same unforgeable fence.
 */
final class UntrustedText {

    private UntrustedText() {}

    /** Fence tag for a raw transcript, or a digest rendered from one. */
    static final String TRANSCRIPT_TAG = "untrusted_transcript";

    /** Fence tag for an AI analysis of a transcript, which inherits the transcript's taint. */
    static final String ANALYSIS_TAG = "untrusted_analysis";

    /** What a fence lookalike found inside untrusted text is replaced with. */
    static final String MARKER_PLACEHOLDER = "[marker removed]";

    /** 64 random bits per fence: far beyond guessing, and never reused across calls. */
    private static final int NONCE_BYTES = 8;

    private static final SecureRandom NONCES = new SecureRandom();

    /**
     * Anything shaped like a fence marker: the literal word however it is punctuated or spaced,
     * plus the word that follows it. Both quantifiers are possessive, so the match stays linear on
     * hostile input.
     */
    private static final Pattern FENCE_LOOKALIKE = Pattern.compile(
        "untrusted[\\p{Punct}\\s]*+\\w*+",
        Pattern.CASE_INSENSITIVE
    );

    /** Fences an ingested transcript (or a digest rendered from one) for a prompt slot. */
    static String fencedTranscript(String text) {
        return fenced(TRANSCRIPT_TAG, text);
    }

    /** Fences an AI analysis of an ingested transcript for a prompt slot. */
    static String fencedAnalysis(String text) {
        return fenced(ANALYSIS_TAG, text);
    }

    /**
     * Wraps untrusted text in a fence whose tag ends in a fresh random nonce, after rendering the
     * text itself inert. The text is still passed through in full — only its ability to shape the
     * prompt around it is removed.
     */
    static String fenced(String tag, String text) {
        byte[] nonce = new byte[NONCE_BYTES];
        NONCES.nextBytes(nonce);
        String fenceTag = tag + "_" + HexFormat.of().formatHex(nonce);
        return "<" + fenceTag + ">\n" + inert(text) + "\n</" + fenceTag + ">";
    }

    /**
     * Neutralizes an ingested value's ability to shape the prompt it is rendered into: fence
     * lookalikes are replaced, invisible format characters are dropped, and control characters other
     * than newline and tab collapse to spaces.
     */
    static String inert(String text) {
        if (text == null) return "";
        return FENCE_LOOKALIKE.matcher(scrub(text, true)).replaceAll(MARKER_PLACEHOLDER);
    }

    /**
     * A single-line, length-capped rendering of one field of a model reply. The reply was produced
     * from an untrusted transcript, so a field must not be able to carry unbounded text, terminal
     * escapes, invisible characters, or forged line breaks into the store, the UI, and the miner
     * that later re-reads the recommendations as evidence. {@code null} stays {@code null}, so the
     * cached JSON keeps its shape.
     */
    static String boundedLine(String value, int max) {
        if (value == null) return null;
        return cap(scrub(value, false).replaceAll("\\s{2,}", " ").strip(), max);
    }

    /** As {@link #boundedLine}, but for a paragraph field whose line breaks are worth keeping. */
    static String boundedText(String value, int max) {
        if (value == null) return null;
        return cap(scrub(value, true).strip(), max);
    }

    private static String cap(String value, int max) {
        return value.length() > max ? value.substring(0, max) + "..." : value;
    }

    /**
     * Drops the characters that are invisible to a human reading the text and replaces the ones that
     * could corrupt the structure it is rendered into with a space. Newlines and tabs are noise in a
     * one-line field but structure in a transcript or a paragraph, so {@code keepLineBreaks}
     * decides. Iterates by code point so supplementary invisibles (the U+E0000 tag block used to
     * smuggle hidden text) are covered too.
     */
    private static String scrub(String text, boolean keepLineBreaks) {
        StringBuilder out = new StringBuilder(text.length());
        int i = 0;
        while (i < text.length()) {
            int cp = text.codePointAt(i);
            i += Character.charCount(cp);
            if (keepLineBreaks && (cp == '\n' || cp == '\t')) {
                out.appendCodePoint(cp);
            } else if (Character.getType(cp) == Character.FORMAT) {
                // Zero-width space/joiner, soft hyphen, BOM, tag characters: invisible to a reader,
                // and enough to break a marker's spelling apart.
                continue;
            } else if (isStructural(cp)) {
                out.append(' ');
            } else {
                out.appendCodePoint(cp);
            }
        }
        return out.toString();
    }

    /** True for characters that could forge a line — or a terminal escape — in the rendered text. */
    private static boolean isStructural(int cp) {
        int type = Character.getType(cp);
        return (
            Character.isISOControl(cp) ||
            type == Character.LINE_SEPARATOR ||
            type == Character.PARAGRAPH_SEPARATOR
        );
    }
}
