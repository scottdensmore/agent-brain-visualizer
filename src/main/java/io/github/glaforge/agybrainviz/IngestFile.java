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

import io.micronaut.serde.annotation.Serdeable;

/**
 * One file a pushed transcript attached, travelling with the session so the inline preview does not
 * depend on the file being present on the machine serving the UI.
 *
 * @param path the absolute path as the transcript spelled it. It is a lookup key, not something the
 *     server ever opens — the content came from the client that had the file.
 * @param content the file's text, sanitized to valid UTF-8 by the client exactly as transcripts are
 */
@Serdeable
public record IngestFile(String path, String content) {}
