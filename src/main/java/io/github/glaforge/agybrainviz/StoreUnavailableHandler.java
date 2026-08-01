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

import io.micronaut.context.annotation.Requires;
import io.micronaut.http.HttpRequest;
import io.micronaut.http.HttpResponse;
import io.micronaut.http.HttpStatus;
import io.micronaut.http.annotation.Produces;
import io.micronaut.http.server.exceptions.ExceptionHandler;
import jakarta.inject.Singleton;
import java.util.Map;
import org.slf4j.Logger;
import org.slf4j.LoggerFactory;

/** Turns a store outage into a 503 that says what to do about it, rather than a bare 500. */
@Produces
@Singleton
@Requires(classes = { StoreUnavailableException.class, ExceptionHandler.class })
public class StoreUnavailableHandler
    implements ExceptionHandler<StoreUnavailableException, HttpResponse<?>> {

    private static final Logger LOG = LoggerFactory.getLogger(StoreUnavailableHandler.class);

    @Override
    public HttpResponse<?> handle(HttpRequest request, StoreUnavailableException exception) {
        // Both of these carry request-supplied text — the path, and the repository message, which
        // quotes the id (an ingest body's, on the push path) it could not store.
        LOG.warn(
            "Store unavailable serving {}: {}",
            LogSafe.escape(request.getPath()),
            LogSafe.escape(exception.getMessage())
        );
        return HttpResponse
            .status(HttpStatus.SERVICE_UNAVAILABLE)
            .body(
                Map.of(
                    "error",
                    "The trajectory store is unavailable. Is the database running? Try `docker compose up -d`."
                )
            );
    }
}
