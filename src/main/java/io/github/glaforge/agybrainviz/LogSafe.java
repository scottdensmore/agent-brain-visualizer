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

import java.util.HexFormat;

/**
 * Renders one untrusted value as a single printable run of characters, for interpolation into a log
 * message.
 *
 * <p>The appenders in {@code logback.xml} end a record at a newline ({@code %msg%n}), and copy the
 * message through verbatim. A value that arrived in a request body — an ingest {@code source} or
 * {@code id}, say — can therefore carry a line break and forge whole records around the real one,
 * hiding an abuse from an operator reading the log or from an aggregator parsing the stream.
 * {@link #escape(String)} folds every character that could end a record into a printable escape, so
 * the value can only ever be part of the line it was logged on.
 *
 * <p>This is a helper for the call sites that use it, not a property of the logging system: a value
 * logged without it stays raw, and a value carried inside a {@link Throwable} passed to the logger is
 * written by the appender's throwable converter, past {@code %msg}, where this cannot reach it.
 */
final class LogSafe {

    /** Past this, an over-long value is noise that pushes the real record off an operator's screen. */
    private static final int MAX_LENGTH = 512;

    private static final String TRUNCATION_MARKER = "...[truncated]";

    /** U+2028 LINE SEPARATOR, a record boundary for a JSON or Unicode-aware reader. */
    private static final char LINE_SEPARATOR = 0x2028;

    /** U+2029 PARAGRAPH SEPARATOR, likewise. */
    private static final char PARAGRAPH_SEPARATOR = 0x2029;

    /** {@link HexFormat} is fixed-alphabet, so an escape reads the same under any default locale. */
    private static final HexFormat HEX = HexFormat.of().withUpperCase();

    private LogSafe() {}

    /**
     * Returns {@code value} with CR, LF, TAB, every other ISO control character, and the Unicode line
     * separators U+2028/U+2029 replaced by printable escapes, capped at 512 characters.
     *
     * <p>A value shorter than the cap that holds none of those is returned unchanged, so ordinary ids
     * and sources still read exactly as they were pushed. {@code null} renders as {@code "null"}, the
     * same as slf4j's own interpolation of a null argument.
     */
    static String escape(String value) {
        if (value == null) return "null";
        int end = value.length();
        boolean truncated = end > MAX_LENGTH;
        if (truncated) {
            end = MAX_LENGTH;
            // Never cut between the halves of a surrogate pair: the leftover half is not a character,
            // and the encoder would write it out as a replacement byte.
            if (Character.isHighSurrogate(value.charAt(end - 1))) end--;
        }
        StringBuilder escaped = new StringBuilder(end + TRUNCATION_MARKER.length());
        for (int i = 0; i < end; i++) {
            char c = value.charAt(i);
            switch (c) {
                case '\n' -> escaped.append("\\n");
                case '\r' -> escaped.append("\\r");
                case '\t' -> escaped.append("\\t");
                default -> {
                    // The ISO controls cover NUL..US, DEL and the C1 block — which is where NEL
                    // (U+0085) and CSI (U+009B) live.
                    if (
                        Character.isISOControl(c) || c == LINE_SEPARATOR || c == PARAGRAPH_SEPARATOR
                    ) {
                        escaped.append("\\u").append(HEX.toHexDigits(c));
                    } else {
                        escaped.append(c);
                    }
                }
            }
        }
        if (truncated) escaped.append(TRUNCATION_MARKER);
        return escaped.toString();
    }
}
