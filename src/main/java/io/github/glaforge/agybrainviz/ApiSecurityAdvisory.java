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

import io.micronaut.context.annotation.Value;
import io.micronaut.context.event.ApplicationEventListener;
import io.micronaut.context.event.StartupEvent;
import jakarta.inject.Singleton;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/**
 * Announces the read/compute API's security posture at startup, alongside {@link
 * IngestSecurityAdvisory}.
 *
 * <p>Ingest already had this. The read side did not, which made the boot log actively misleading: an
 * operator who set {@code INGEST_TOKEN} was told "Ingest endpoints require a bearer token" and heard
 * nothing at all about {@code /api/brain}, which serves every stored transcript and the {@code
 * ~/.gemini} file preview to whoever can reach the port.
 *
 * <p>Every message names the bind address, because the token alone does not tell an operator whether
 * anyone else can reach them — and because the one population a loopback default inconveniences (a
 * deliberately exposed deployment) deserves to see, in the boot log, exactly why its API went quiet.
 */
@Singleton
public class ApiSecurityAdvisory implements ApplicationEventListener<StartupEvent> {

    private static final Logger LOG = LoggerFactory.getLogger(ApiSecurityAdvisory.class);

    /** The four states of the read/compute guard, in words the boot log reports. */
    enum Posture {
        /** A token is set; the API requires it, wherever it listens. */
        AUTHENTICATED,
        /** No token, but nothing off this machine can reach the port — the shipped default. */
        LOOPBACK_ONLY,
        /** No token and a reachable bind: every stored trajectory is served to the network. */
        EXPOSED,
        /** Auth was required but no token is set, so the whole API is refused until one is. */
        MISCONFIGURED,
    }

    private final ApiConfig config;
    private final String host;

    public ApiSecurityAdvisory(ApiConfig config, @Value("${micronaut.server.host:}") String host) {
        this.config = config;
        this.host = host;
    }

    @Override
    public void onApplicationEvent(StartupEvent event) {
        String where = describeBind(host);
        switch (classify(config, host)) {
            case AUTHENTICATED -> LOG.info(
                "The read/compute API requires a bearer token (API_TOKEN); listening on {}.",
                where
            );
            case LOOPBACK_ONLY -> LOG.info(
                "The read/compute API is unauthenticated but listening on {}, so only this machine " +
                "can reach it. Set API_TOKEN before widening MICRONAUT_SERVER_HOST.",
                where
            );
            case EXPOSED -> LOG.warn(
                "The read/compute API is UNAUTHENTICATED and listening on {} — anyone who can reach " +
                "this port can read every stored trajectory and spend this server's AI credits. Set " +
                "API_TOKEN (and see the deployment-security notes in the README). In a container " +
                "this is the bind inside the container; what the host publishes decides who can " +
                "actually reach it.",
                where
            );
            case MISCONFIGURED -> LOG.error(
                "API_REQUIRE_AUTH is set but API_TOKEN is empty — the whole read/compute API will be " +
                "refused until a token is configured. Set API_TOKEN."
            );
        }
    }

    /**
     * Pure classification of the configured posture, so it can be reasoned about without booting.
     *
     * <p>An unset host is treated as exposed, not as loopback: that is precisely the state in which
     * Micronaut binds the wildcard address.
     */
    static Posture classify(ApiConfig config, String host) {
        if (config.token().isPresent()) return Posture.AUTHENTICATED;
        if (config.requireAuth()) return Posture.MISCONFIGURED;
        return isLoopback(host) ? Posture.LOOPBACK_ONLY : Posture.EXPOSED;
    }

    /** Whether a configured bind host keeps the server unreachable from other machines. */
    static boolean isLoopback(String host) {
        if (host == null) return false;
        String trimmed = host.trim();
        return (
            trimmed.equals("127.0.0.1") ||
            trimmed.equals("localhost") ||
            trimmed.equals("::1") ||
            trimmed.equals("[::1]")
        );
    }

    /** The bind address in words, so an unset host reads as what it actually does. */
    static String describeBind(String host) {
        if (host == null || host.isBlank()) return "every interface (no micronaut.server.host set)";
        return isLoopback(host)
            ? host + " (this machine only)"
            : host + " (reachable off this machine)";
    }
}
