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
import static org.junit.jupiter.api.Assertions.assertFalse;
import static org.junit.jupiter.api.Assertions.assertTrue;

import java.util.Locale;
import org.junit.jupiter.api.Test;

/** Tests that an untrusted value cannot end the log record it is interpolated into. */
class LogSafeTest {

    /** Every character class a reader of the log stream could treat as a record boundary. */
    private static final char[] RECORD_BOUNDARIES = {
        '\n', // LF
        '\r', // CR
        0x0b, // VT
        '\f', // FF
        0x00, // NUL
        0x1b, // ESC
        0x85, // NEL
        0x7f, // DEL
        0x9b, // CSI
        0x2028, // LINE SEPARATOR
        0x2029, // PARAGRAPH SEPARATOR
    };

    @Test
    void keepsAnOrdinaryValueExactlyAsItWasPushed() {
        // The common case must stay readable: an operator greps the log for the id they were given.
        assertEquals("antigravity-cli", LogSafe.escape("antigravity-cli"));
        assertEquals("abcdef123456", LogSafe.escape("abcdef123456"));
        assertEquals("", LogSafe.escape(""));
        assertEquals("a b/c-d_e.f:g", LogSafe.escape("a b/c-d_e.f:g"));
    }

    @Test
    void passesNonAsciiTextThroughUntouched() {
        // Escaping is about record boundaries, not about ASCII: a Japanese title or an emoji in an
        // id is not an attack, and mangling it would cost an operator the value of the log line.
        assertEquals("héllo — 世界", LogSafe.escape("héllo — 世界"));
        assertEquals("rocket 🚀 id", LogSafe.escape("rocket 🚀 id"));
    }

    @Test
    void turnsALineBreakIntoAPrintableEscape() {
        assertEquals("s1\\nWARN forged", LogSafe.escape("s1\nWARN forged"));
        assertEquals("s1\\r\\nWARN forged", LogSafe.escape("s1\r\nWARN forged"));
        assertEquals("s1\\tcol", LogSafe.escape("s1\tcol"));
    }

    @Test
    void leavesNoRecordBoundaryOfAnyKindInTheResult() {
        for (char boundary : RECORD_BOUNDARIES) {
            String escaped = LogSafe.escape("s1" + boundary + "forged");
            assertEquals(
                1,
                escaped.lines().count(),
                () -> "U+" + Integer.toHexString(boundary) + " still split the record: " + escaped
            );
            for (int i = 0; i < escaped.length(); i++) {
                char c = escaped.charAt(i);
                assertFalse(
                    Character.isISOControl(c) || c == 0x2028 || c == 0x2029,
                    () ->
                        "escaping U+" + Integer.toHexString(boundary) + " left a control character"
                );
            }
            assertTrue(escaped.startsWith("s1"), escaped);
            assertTrue(escaped.endsWith("forged"), escaped);
        }
    }

    @Test
    void escapesTheSameWhateverTheDefaultLocaleIs() {
        // A locale-sensitive hex formatter would render 0x1B as "1b" here and something else under a
        // Turkish locale, so an aggregator's rules would stop matching on a machine set to tr-TR.
        Locale original = Locale.getDefault();
        try {
            Locale.setDefault(Locale.forLanguageTag("tr-TR"));
            assertEquals("s1\\u001Bforged", LogSafe.escape("s1" + (char) 0x1b + "forged"));
            assertEquals("s1\\u2028forged", LogSafe.escape("s1" + (char) 0x2028 + "forged"));
        } finally {
            Locale.setDefault(original);
        }
    }

    @Test
    void capsARunawayValueAndSaysThatItDidSo() {
        String escaped = LogSafe.escape("a".repeat(5_000));
        assertEquals("a".repeat(512) + "...[truncated]", escaped);
        // A value right at the cap is not truncated, so nothing is marked that was not cut.
        assertEquals("a".repeat(512), LogSafe.escape("a".repeat(512)));
    }

    @Test
    void countsTheEscapedRecordBoundariesAgainstTheContentNotTheCap() {
        // 600 newlines is 600 chars of input: the cap applies before escaping, so the result is
        // bounded (2 chars per escape) and still cannot break the record.
        String escaped = LogSafe.escape("\n".repeat(600));
        assertEquals(1, escaped.lines().count());
        assertEquals("\\n".repeat(512) + "...[truncated]", escaped);
    }

    @Test
    void neverCutsASurrogatePairInHalf() {
        // The pair straddles the 512-char cap: keeping its high half alone would emit a lone
        // surrogate, which the encoder writes out as a replacement byte.
        String escaped = LogSafe.escape("a".repeat(511) + "🚀" + "tail");
        assertEquals("a".repeat(511) + "...[truncated]", escaped);
        assertFalse(Character.isHighSurrogate(escaped.charAt(510)));

        // A pair that fits inside the cap survives whole.
        String fits = LogSafe.escape("a".repeat(510) + "🚀" + "tail");
        assertEquals("a".repeat(510) + "🚀" + "...[truncated]", fits);
    }

    @Test
    void rendersNullTheWaySlf4jWould() {
        // The call sites hand it values that can be null (an exception with no message); logging
        // "null" is what the unescaped placeholder did, so no call site changes shape.
        assertEquals("null", LogSafe.escape(null));
    }
}
