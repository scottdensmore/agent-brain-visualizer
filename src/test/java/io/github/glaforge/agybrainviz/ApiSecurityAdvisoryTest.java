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

import io.github.glaforge.agybrainviz.ApiSecurityAdvisory.Posture;
import java.util.List;
import org.junit.jupiter.api.Test;

/** The boot-time classification that tells an operator who can reach the read/compute API. */
class ApiSecurityAdvisoryTest {

    private static final String LOOPBACK = "127.0.0.1";

    @Test
    void aConfiguredTokenIsAuthenticatedWhereverItListens() {
        assertEquals(
            Posture.AUTHENTICATED,
            ApiSecurityAdvisory.classify(new ApiConfig("t"), LOOPBACK)
        );
        assertEquals(
            Posture.AUTHENTICATED,
            ApiSecurityAdvisory.classify(new ApiConfig("t"), "0.0.0.0")
        );
        assertEquals(
            Posture.AUTHENTICATED,
            ApiSecurityAdvisory.classify(new ApiConfig("t", true), "")
        );
    }

    @Test
    void noTokenOnLoopbackIsTheShippedDefault() {
        assertEquals(
            Posture.LOOPBACK_ONLY,
            ApiSecurityAdvisory.classify(new ApiConfig(""), LOOPBACK)
        );
    }

    @Test
    void noTokenOnAReachableBindIsExposed() {
        for (String host : List.of("0.0.0.0", "192.168.1.10", "::")) {
            assertEquals(
                Posture.EXPOSED,
                ApiSecurityAdvisory.classify(new ApiConfig(""), host),
                host + " is reachable off this machine"
            );
        }
    }

    @Test
    void anUnsetHostIsExposedBecauseThatIsWhatMicronautDoes() {
        // The whole point of the finding: no host configured means the wildcard address, not
        // localhost. Classifying it as loopback would reproduce the presumption that was wrong.
        assertEquals(Posture.EXPOSED, ApiSecurityAdvisory.classify(new ApiConfig(""), ""));
        assertEquals(Posture.EXPOSED, ApiSecurityAdvisory.classify(new ApiConfig(""), null));
    }

    @Test
    void requiringAuthWithoutATokenIsAMisconfigurationWhereverItListens() {
        assertEquals(
            Posture.MISCONFIGURED,
            ApiSecurityAdvisory.classify(new ApiConfig("", true), LOOPBACK)
        );
        assertEquals(
            Posture.MISCONFIGURED,
            ApiSecurityAdvisory.classify(new ApiConfig("   ", true), "0.0.0.0")
        );
    }

    @Test
    void recognisesTheLoopbackSpellings() {
        for (String host : List.of("127.0.0.1", "localhost", "::1", "[::1]", " 127.0.0.1 ")) {
            assertTrue(ApiSecurityAdvisory.isLoopback(host), host + " should count as loopback");
        }
        for (String host : List.of(
            "0.0.0.0",
            "::",
            "10.0.0.5",
            "example.com",
            "",
            "127.0.0.1.evil.com"
        )) {
            assertFalse(
                ApiSecurityAdvisory.isLoopback(host),
                host + " should not count as loopback"
            );
        }
    }

    @Test
    void everyPostureNamesTheBindAddress() {
        // The operator a loopback default inconveniences must be able to see why from the boot log.
        assertTrue(ApiSecurityAdvisory.describeBind("").contains("every interface"));
        assertTrue(ApiSecurityAdvisory.describeBind(LOOPBACK).contains("this machine only"));
        assertTrue(
            ApiSecurityAdvisory.describeBind("0.0.0.0").contains("reachable off this machine")
        );
    }
}
