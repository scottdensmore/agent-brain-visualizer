// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pgdriver_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"
	"testing"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/pgdriver"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgtest"
)

func TestParseDSNFormats(t *testing.T) {
	tests := []struct {
		name     string
		dsn      string
		fbUser   string
		fbPass   string
		wantNet  string
		wantHost string
		wantPort string
		wantUser string
		wantPass string
		wantDB   string
	}{
		{
			name:     "jdbc url with fallback credentials",
			dsn:      "jdbc:postgresql://localhost:55433/agentbrainviz?sslmode=disable",
			fbUser:   "agentviz",
			fbPass:   "secret",
			wantNet:  "tcp",
			wantHost: "localhost",
			wantPort: "55433",
			wantUser: "agentviz",
			wantPass: "secret",
			wantDB:   "agentbrainviz",
		},
		{
			name:     "postgres url with embedded credentials",
			dsn:      "postgres://alice:wonderland@db.example.com:5433/trajectories",
			fbUser:   "ignored",
			fbPass:   "ignored",
			wantNet:  "tcp",
			wantHost: "db.example.com",
			wantPort: "5433",
			wantUser: "alice",
			wantPass: "wonderland",
			wantDB:   "trajectories",
		},
		{
			name:     "postgresql url with default port",
			dsn:      "postgresql://bob@127.0.0.1/mydb",
			fbUser:   "",
			fbPass:   "pw",
			wantNet:  "tcp",
			wantHost: "127.0.0.1",
			wantPort: "5432",
			wantUser: "bob",
			wantPass: "pw",
			wantDB:   "mydb",
		},
		{
			name:     "pgtest pipe dsn",
			dsn:      "pgtest://srv-42/testdb",
			fbUser:   "tester",
			fbPass:   "testpass",
			wantNet:  "pipe",
			wantHost: "srv-42",
			wantPort: "0",
			wantUser: "tester",
			wantPass: "testpass",
			wantDB:   "testdb",
		},
		{
			name:     "key value dsn",
			dsn:      "host=10.0.0.5 port=6432 user=pguser password=pgpass dbname=braindb sslmode=disable",
			wantNet:  "tcp",
			wantHost: "10.0.0.5",
			wantPort: "6432",
			wantUser: "pguser",
			wantPass: "pgpass",
			wantDB:   "braindb",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := pgdriver.ParseDSN(tc.dsn, tc.fbUser, tc.fbPass)
			if err != nil {
				t.Fatalf("ParseDSN(%q) unexpected error: %v", tc.dsn, err)
			}
			if cfg.Network != tc.wantNet {
				t.Errorf("Network = %q, want %q", cfg.Network, tc.wantNet)
			}
			if cfg.Host != tc.wantHost {
				t.Errorf("Host = %q, want %q", cfg.Host, tc.wantHost)
			}
			if cfg.Port != tc.wantPort {
				t.Errorf("Port = %q, want %q", cfg.Port, tc.wantPort)
			}
			if cfg.User != tc.wantUser {
				t.Errorf("User = %q, want %q", cfg.User, tc.wantUser)
			}
			if cfg.Password != tc.wantPass {
				t.Errorf("Password = %q, want %q", cfg.Password, tc.wantPass)
			}
			if cfg.Database != tc.wantDB {
				t.Errorf("Database = %q, want %q", cfg.Database, tc.wantDB)
			}
		})
	}
}

func TestParseDSNErrors(t *testing.T) {
	for _, bad := range []string{"", "   ", "pgtest://", "not-a-dsn"} {
		if _, err := pgdriver.ParseDSN(bad, "", ""); err == nil {
			t.Errorf("ParseDSN(%q) succeeded, expected error", bad)
		}
	}
}

func TestInterpolateParams(t *testing.T) {
	ts := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	args := []driver.NamedValue{
		{Ordinal: 1, Value: "it's a test"},
		{Ordinal: 2, Value: int64(42)},
		{Ordinal: 3, Value: true},
		{Ordinal: 4, Value: false},
		{Ordinal: 5, Value: nil},
		{Ordinal: 6, Value: 3.5},
		{Ordinal: 7, Value: ts},
		{Ordinal: 8, Value: []byte(`{"a":1}`)},
		{Ordinal: 9, Value: 9},
		{Ordinal: 10, Value: "ten"},
	}
	query := `SELECT $1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9, $10, '$1 not replaced' -- $1 comment
/* $2 block */`
	got, err := pgdriver.InterpolateParams(query, args)
	if err != nil {
		t.Fatalf("InterpolateParams failed: %v", err)
	}
	wantPrefix := `SELECT 'it''s a test', 42, TRUE, FALSE, NULL, 3.5, '2026-07-09T12:00:00Z', '{"a":1}'::jsonb, 9, 'ten', '$1 not replaced'`
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("InterpolateParams =\n%s\nwant prefix:\n%s", got, wantPrefix)
	}
}

func TestWireAuthenticationModes(t *testing.T) {
	modes := []struct {
		name string
		mode pgtest.AuthMode
	}{
		{name: "AuthOK", mode: pgtest.AuthOK},
		{name: "AuthCleartext", mode: pgtest.AuthCleartext},
		{name: "AuthMD5", mode: pgtest.AuthMD5},
		{name: "AuthSCRAMSHA256", mode: pgtest.AuthSCRAMSHA256},
	}

	for _, tc := range modes {
		t.Run(tc.name, func(t *testing.T) {
			srv := pgtest.NewServer()
			defer srv.Close()
			srv.SetAuth(tc.mode, "agentviz", "s3cr3tPass!")

			db, err := pgdriver.Open(srv.DSN(), "agentviz", "s3cr3tPass!")
			if err != nil {
				t.Fatalf("Open failed: %v", err)
			}
			defer db.Close()

			if err := db.PingContext(context.Background()); err != nil {
				t.Fatalf("PingContext with valid credentials failed: %v", err)
			}

			var one int
			if err := db.QueryRow("SELECT 1").Scan(&one); err != nil || one != 1 {
				t.Fatalf("SELECT 1 = (%d, %v), want (1, nil)", one, err)
			}

			if tc.mode != pgtest.AuthOK {
				badDB, err := pgdriver.Open(srv.DSN(), "agentviz", "wrong-password")
				if err != nil {
					t.Fatalf("Open failed: %v", err)
				}
				defer badDB.Close()
				if err := badDB.PingContext(context.Background()); err == nil {
					t.Fatalf("PingContext with wrong password succeeded for mode %s, want auth error", tc.name)
				}
			}
		})
	}
}

func TestPreparedStatementsAndTransactionsOverWire(t *testing.T) {
	db, _ := pgtest.NewTestDB(t)

	stmt, err := db.Prepare(`INSERT INTO summaries (source, session_id, summary, short_title, content_hash, updated_at) VALUES ($1, $2, $3::jsonb, $4, $5, now())`)
	if err != nil {
		t.Fatalf("Prepare failed: %v", err)
	}
	defer stmt.Close()

	if _, err := stmt.Exec("codex", "s1", `{"summary":"ok"}`, "Title 1", "h1"); err != nil {
		t.Fatalf("stmt.Exec failed: %v", err)
	}

	// Verify rollback discards uncommitted changes.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin failed: %v", err)
	}
	if _, err := tx.Exec(`DELETE FROM summaries WHERE source = $1 AND session_id = $2`, "codex", "s1"); err != nil {
		t.Fatalf("tx.Exec DELETE failed: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}

	var got string
	if err := db.QueryRow(`SELECT summary FROM summaries WHERE source = $1 AND session_id = $2`, "codex", "s1").Scan(&got); err != nil {
		t.Fatalf("QueryRow after Rollback failed: %v", err)
	}
	if got != `{"summary":"ok"}` {
		t.Errorf("summary after Rollback = %q, want %q", got, `{"summary":"ok"}`)
	}

	// Verify sql.Open("postgres", dsn) registration works directly.
	srv2 := pgtest.NewServer()
	defer srv2.Close()
	db2, err := sql.Open("postgres", srv2.DSN())
	if err != nil {
		t.Fatalf("sql.Open failed: %v", err)
	}
	defer db2.Close()
	if err := db2.Ping(); err != nil {
		t.Fatalf("db2.Ping failed: %v", err)
	}
}
