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

import jakarta.inject.Singleton;
import java.sql.Connection;
import java.sql.PreparedStatement;
import java.sql.ResultSet;
import java.sql.SQLException;
import java.util.List;
import java.util.Optional;
import javax.sql.DataSource;

/**
 * Stores the files a transcript attached, keyed to the session that referenced them.
 *
 * <p>These exist so the inline preview can serve a file the machine running the UI has never seen.
 * Previously the preview read the server's own disk, which meant it worked only when the viewer and
 * the agent were the same machine — and, because the read was sandboxed to {@code ~/.gemini}, it did
 * not even work for the ordinary case of a project file attached to a prompt.
 */
@Singleton
public class SessionFileRepository {

    private static final String UPSERT_SQL = """
        INSERT INTO session_files (source, session_id, path, content, updated_at)
        VALUES (?, ?, ?, ?, now())
        ON CONFLICT (source, session_id, path) DO UPDATE
           SET content = excluded.content,
               updated_at = now()
           WHERE session_files.content IS DISTINCT FROM excluded.content
        """;

    private static final String FIND_SQL =
        "SELECT content FROM session_files WHERE source = ? AND session_id = ? AND path = ?";

    private static final String DELETE_FOR_SESSION_SQL =
        "DELETE FROM session_files WHERE source = ? AND session_id = ?";

    private final DataSource dataSource;

    public SessionFileRepository(DataSource dataSource) {
        this.dataSource = dataSource;
    }

    /**
     * Replaces the attachments recorded for one session.
     *
     * <p>Replace rather than merge: the transcript is the source of truth for which files it
     * references, so a file no longer mentioned after an edit should stop being served rather than
     * linger. Called inside the same push that stores the transcript.
     */
    public void replaceForSession(String source, String sessionId, List<IngestFile> files) {
        try (Connection conn = dataSource.getConnection()) {
            boolean previousAutoCommit = conn.getAutoCommit();
            conn.setAutoCommit(false);
            try {
                try (PreparedStatement delete = conn.prepareStatement(DELETE_FOR_SESSION_SQL)) {
                    delete.setString(1, source);
                    delete.setString(2, sessionId);
                    delete.executeUpdate();
                }
                if (!files.isEmpty()) {
                    try (PreparedStatement insert = conn.prepareStatement(UPSERT_SQL)) {
                        for (IngestFile file : files) {
                            insert.setString(1, source);
                            insert.setString(2, sessionId);
                            insert.setString(3, file.path());
                            insert.setString(4, file.content());
                            insert.addBatch();
                        }
                        insert.executeBatch();
                    }
                }
                conn.commit();
            } catch (SQLException e) {
                conn.rollback();
                throw e;
            } finally {
                conn.setAutoCommit(previousAutoCommit);
            }
        } catch (SQLException e) {
            throw new StoreUnavailableException(
                "Could not store the attached files for " + sessionId,
                e
            );
        }
    }

    /** The stored content of one attached file, or empty when this session did not carry it. */
    public Optional<String> find(String source, String sessionId, String path) {
        try (
            Connection conn = dataSource.getConnection();
            PreparedStatement ps = conn.prepareStatement(FIND_SQL)
        ) {
            ps.setString(1, source);
            ps.setString(2, sessionId);
            ps.setString(3, path);
            try (ResultSet rs = ps.executeQuery()) {
                return rs.next() ? Optional.ofNullable(rs.getString(1)) : Optional.empty();
            }
        } catch (SQLException e) {
            throw new StoreUnavailableException(
                "Could not read the attached files for " + sessionId,
                e
            );
        }
    }

    /** Removes a session's attachments, so deleting a session does not leave them behind. */
    public void deleteForSession(String source, String sessionId) {
        try (
            Connection conn = dataSource.getConnection();
            PreparedStatement ps = conn.prepareStatement(DELETE_FOR_SESSION_SQL)
        ) {
            ps.setString(1, source);
            ps.setString(2, sessionId);
            ps.executeUpdate();
        } catch (SQLException e) {
            throw new StoreUnavailableException(
                "Could not delete the attached files for " + sessionId,
                e
            );
        }
    }
}
