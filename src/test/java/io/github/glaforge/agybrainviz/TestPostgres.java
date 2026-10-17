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

import com.github.dockerjava.api.model.ExposedPort;
import com.github.dockerjava.api.model.PortBinding;
import com.github.dockerjava.api.model.Ports;
import com.zaxxer.hikari.HikariConfig;
import com.zaxxer.hikari.HikariDataSource;
import java.util.Map;
import javax.sql.DataSource;
import org.testcontainers.containers.PostgreSQLContainer;

/**
 * The one Postgres container shared by every test in the JVM.
 *
 * <p>Tests must not depend on a developer's {@code docker compose} stack being up, or on CI adding a
 * service container: those make {@code ./gradlew build} pass or fail based on ambient state. The
 * container is started on first use and left for Testcontainers' reaper, since starting it dominates
 * the runtime of the tests that need it.
 */
final class TestPostgres {

    private TestPostgres() {}

    private static PostgreSQLContainer<?> container;
    private static HikariDataSource dataSource;

    static synchronized PostgreSQLContainer<?> container() {
        if (container == null) {
            container =
                new PostgreSQLContainer<>("postgres:17-alpine")
                    .withCreateContainerCmdModifier(cmd ->
                        cmd
                            .getHostConfig()
                            .withPortBindings(
                                new PortBinding(
                                    Ports.Binding.bindIpAndPort("127.0.0.1", 0),
                                    new ExposedPort(5432)
                                )
                            )
                    );
            container.start();
        }
        return container;
    }

    /** A pooled {@link DataSource} on the shared container, with the app's schema already applied. */
    static synchronized DataSource dataSource() {
        if (dataSource == null) {
            PostgreSQLContainer<?> pg = container();
            HikariConfig config = new HikariConfig();
            config.setJdbcUrl(pg.getJdbcUrl());
            config.setUsername(pg.getUsername());
            config.setPassword(pg.getPassword());
            config.setMaximumPoolSize(4);
            dataSource = new HikariDataSource(config);
            try {
                SchemaBootstrap.apply(dataSource, SchemaBootstrap.readSchema());
            } catch (Exception e) {
                throw new IllegalStateException("Could not apply the test schema", e);
            }
        }
        return dataSource;
    }

    /**
     * The container's connection settings, shaped as Micronaut datasource properties.
     *
     * <p>A {@code @MicronautTest} that touches the store overrides {@code application.yml} with these,
     * so its context talks to the container rather than to a developer's local Postgres.
     */
    static Map<String, String> datasourceProperties() {
        PostgreSQLContainer<?> pg = container();
        return Map.of(
            "datasources.default.url",
            pg.getJdbcUrl(),
            "datasources.default.username",
            pg.getUsername(),
            "datasources.default.password",
            pg.getPassword()
        );
    }
}
