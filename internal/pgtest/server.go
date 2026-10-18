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

// Package pgtest provides an in-process PostgreSQL v3 wire-protocol test server
// over net.Pipe() connections, backed by a thread-safe relational engine that
// implements the Agent Brain Visualizer schema and queries.
package pgtest

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/pgdriver"
)

var serverCounter uint64

// AuthMode configures the PostgreSQL v3 authentication handshake required by Server.
type AuthMode int

const (
	AuthOK AuthMode = iota
	AuthCleartext
	AuthMD5
	AuthSCRAMSHA256
)

type sessionKey struct {
	source string
	id     string
}

type sessionFileKey struct {
	source    string
	sessionID string
	path      string
}

type sessionRow struct {
	source      string
	id          string
	title       *string
	updatedAt   time.Time
	steps       string
	contentHash string
	sourceMtime int64
	rawSource   *string
	ingestedAt  time.Time
}

type summaryRow struct {
	source      string
	sessionID   string
	summary     string
	shortTitle  *string
	contentHash *string
	updatedAt   time.Time
}

type sessionFileRow struct {
	source    string
	sessionID string
	path      string
	content   string
	updatedAt time.Time
}

type evalRunRow struct {
	runID             int64
	savedAt           string
	flavor            string
	modelLabel        string
	sessionCount      int
	evaluatedSessions int
	avgScore          float64
	checkPassRates    string
	judged            bool
	judgedSessions    int
	avgFaithfulness   float64
	avgActionability  float64
	avgClarity        float64
}

type dbState struct {
	tables       map[string]bool
	sessions     map[sessionKey]*sessionRow
	summaries    map[sessionKey]*summaryRow
	sessionFiles map[sessionFileKey]*sessionFileRow
	evalRuns     []*evalRunRow
	nextRunID    int64
}

func newDBState() *dbState {
	return &dbState{
		tables: map[string]bool{
			"sessions":      true,
			"summaries":     true,
			"session_files": true,
			"eval_runs":     true,
		},
		sessions:     make(map[sessionKey]*sessionRow),
		summaries:    make(map[sessionKey]*summaryRow),
		sessionFiles: make(map[sessionFileKey]*sessionFileRow),
		evalRuns:     make([]*evalRunRow, 0),
		nextRunID:    1,
	}
}

func (st *dbState) clone() *dbState {
	c := &dbState{
		tables:       make(map[string]bool, len(st.tables)),
		sessions:     make(map[sessionKey]*sessionRow, len(st.sessions)),
		summaries:    make(map[sessionKey]*summaryRow, len(st.summaries)),
		sessionFiles: make(map[sessionFileKey]*sessionFileRow, len(st.sessionFiles)),
		evalRuns:     make([]*evalRunRow, len(st.evalRuns)),
		nextRunID:    st.nextRunID,
	}
	for k, v := range st.tables {
		c.tables[k] = v
	}
	for k, v := range st.sessions {
		rowCopy := *v
		rowCopy.title = cloneStrPtr(v.title)
		rowCopy.rawSource = cloneStrPtr(v.rawSource)
		c.sessions[k] = &rowCopy
	}
	for k, v := range st.summaries {
		rowCopy := *v
		rowCopy.shortTitle = cloneStrPtr(v.shortTitle)
		rowCopy.contentHash = cloneStrPtr(v.contentHash)
		c.summaries[k] = &rowCopy
	}
	for k, v := range st.sessionFiles {
		rowCopy := *v
		c.sessionFiles[k] = &rowCopy
	}
	for i, v := range st.evalRuns {
		rowCopy := *v
		c.evalRuns[i] = &rowCopy
	}
	return c
}

func cloneStrPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

func strPtr(s string) *string {
	return &s
}

func nullStrEqual(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

// Server is an in-process PostgreSQL v3 wire-protocol server.
type Server struct {
	id       string
	mu       sync.Mutex
	state    *dbState
	closed   bool
	conns    map[net.Conn]struct{}
	authMode AuthMode
	authUser string
	authPass string
}

// NewServer starts a new in-process PostgreSQL wire server and registers its pipe dialer.
func NewServer() *Server {
	id := fmt.Sprintf("pgtest-%d", atomic.AddUint64(&serverCounter, 1))
	s := &Server{
		id:       id,
		state:    newDBState(),
		conns:    make(map[net.Conn]struct{}),
		authMode: AuthOK,
		authUser: "test",
		authPass: "test",
	}
	pgdriver.RegisterPipeDialer(id, s.dial)
	return s
}

// NewTestDB creates a new Server and *sql.DB, registering cleanup on t.
func NewTestDB(t testing.TB) (*sql.DB, *Server) {
	t.Helper()
	srv := NewServer()
	db := srv.OpenDB()
	t.Cleanup(func() {
		_ = db.Close()
		srv.Close()
	})
	return db, srv
}

// SetAuth configures the authentication mode and expected credentials for new connections.
func (s *Server) SetAuth(mode AuthMode, user, password string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authMode = mode
	s.authUser = user
	s.authPass = password
}

// DSN returns the pgtest:// connection URL for this server.
func (s *Server) DSN() string {
	return "pgtest://" + s.id + "/agentbrainviz"
}

// OpenDB opens a *sql.DB connected to this Server.
func (s *Server) OpenDB() *sql.DB {
	s.mu.Lock()
	user := s.authUser
	pass := s.authPass
	s.mu.Unlock()
	db, err := pgdriver.Open(s.DSN(), user, pass)
	if err != nil {
		panic(fmt.Sprintf("pgtest.OpenDB: %v", err))
	}
	return db
}

// Reset clears all stored rows and resets identity sequences.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = newDBState()
}

// Close shuts down the server, unregisters its pipe dialer, and closes all open connections.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.conns = make(map[net.Conn]struct{})
	s.mu.Unlock()

	pgdriver.UnregisterPipeDialer(s.id)
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) dial(ctx context.Context) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("pgtest: server is closed")
	}
	clientConn, serverConn := net.Pipe()
	s.conns[serverConn] = struct{}{}
	s.mu.Unlock()

	go s.serveConn(serverConn)
	return clientConn, nil
}

func (s *Server) removeConn(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
	_ = c.Close()
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type sqlError struct {
	code    string
	message string
}

func (e *sqlError) Error() string {
	return e.message
}

func (s *Server) serveConn(nc net.Conn) {
	defer s.removeConn(nc)

	user, err := readStartup(nc)
	if err != nil {
		return
	}

	if err := s.authenticateConn(nc, user); err != nil {
		return
	}

	// Send ParameterStatus, BackendKeyData, ReadyForQuery.
	if err := writeParameterStatus(nc, "server_version", "17.0"); err != nil {
		return
	}
	if err := writeBackendKeyData(nc, 1001, 2002); err != nil {
		return
	}
	if err := writeReadyForQuery(nc, 'I'); err != nil {
		return
	}

	var txState *dbState

	for {
		typ, payload, err := readFrontendMsg(nc)
		if err != nil {
			return
		}
		if s.isClosed() {
			_ = writeErrorResponse(nc, "57P01", "terminating connection due to administrator command")
			return
		}

		switch typ {
		case 'X': // Terminate
			return
		case 'Q': // Simple Query
			sqlText := strings.TrimRight(string(payload), "\x00")
			stmts := splitStatements(sqlText)
			if len(stmts) == 0 {
				if err := writeMsg(nc, 'I', nil); err != nil {
					return
				}
			} else {
				for _, stmt := range stmts {
					cols, rows, tag, execErr := s.execStatement(stmt, &txState)
					if execErr != nil {
						code := "42601"
						var se *sqlError
						if errors.As(execErr, &se) && se.code != "" {
							code = se.code
						}
						if err := writeErrorResponse(nc, code, execErr.Error()); err != nil {
							return
						}
						break
					}
					if cols != nil {
						if err := writeRowDescription(nc, cols); err != nil {
							return
						}
						for _, r := range rows {
							if err := writeDataRow(nc, r); err != nil {
								return
							}
						}
					}
					if err := writeCommandComplete(nc, tag); err != nil {
						return
					}
				}
			}
			txStatus := byte('I')
			if txState != nil {
				txStatus = 'T'
			}
			if err := writeReadyForQuery(nc, txStatus); err != nil {
				return
			}
		default:
			if err := writeErrorResponse(nc, "08P01", fmt.Sprintf("unsupported frontend message %q", typ)); err != nil {
				return
			}
			if err := writeReadyForQuery(nc, 'I'); err != nil {
				return
			}
		}
	}
}

func readStartup(nc net.Conn) (string, error) {
	for {
		var lenBuf [4]byte
		if _, err := io.ReadFull(nc, lenBuf[:]); err != nil {
			return "", err
		}
		length := int(binary.BigEndian.Uint32(lenBuf[:]))
		if length < 8 || length > 65536 {
			return "", fmt.Errorf("pgtest: invalid startup packet length %d", length)
		}
		body := make([]byte, length-4)
		if _, err := io.ReadFull(nc, body); err != nil {
			return "", err
		}
		proto := int32(binary.BigEndian.Uint32(body[:4]))
		if proto == 80877103 { // SSLRequest
			if _, err := nc.Write([]byte{'N'}); err != nil {
				return "", err
			}
			continue
		}
		if proto != 196608 {
			return "", fmt.Errorf("pgtest: unsupported protocol version %d", proto)
		}
		params := bytes.Split(body[4:], []byte{0})
		var user string
		for i := 0; i+1 < len(params); i += 2 {
			k := string(params[i])
			v := string(params[i+1])
			if k == "user" {
				user = v
			}
		}
		return user, nil
	}
}

func (s *Server) authenticateConn(nc net.Conn, clientUser string) error {
	s.mu.Lock()
	mode := s.authMode
	expectedUser := s.authUser
	expectedPass := s.authPass
	s.mu.Unlock()

	switch mode {
	case AuthOK:
		return writeAuthCode(nc, 0, nil)

	case AuthCleartext:
		if err := writeAuthCode(nc, 3, nil); err != nil {
			return err
		}
		typ, payload, err := readFrontendMsg(nc)
		if err != nil {
			return err
		}
		if typ != 'p' {
			_ = writeErrorResponse(nc, "08P01", "expected password message")
			return errors.New("expected password message")
		}
		gotPass := strings.TrimRight(string(payload), "\x00")
		if (expectedUser != "" && clientUser != expectedUser) || gotPass != expectedPass {
			_ = writeErrorResponse(nc, "28P01", "password authentication failed")
			return errors.New("auth failed")
		}
		return writeAuthCode(nc, 0, nil)

	case AuthMD5:
		salt := []byte{'a', 'b', 'c', 'd'}
		if err := writeAuthCode(nc, 5, salt); err != nil {
			return err
		}
		typ, payload, err := readFrontendMsg(nc)
		if err != nil {
			return err
		}
		if typ != 'p' {
			_ = writeErrorResponse(nc, "08P01", "expected MD5 password message")
			return errors.New("expected password message")
		}
		gotPass := strings.TrimRight(string(payload), "\x00")
		inner := md5.Sum([]byte(expectedPass + expectedUser))
		innerHex := hex.EncodeToString(inner[:])
		var outerIn []byte
		outerIn = append(outerIn, innerHex...)
		outerIn = append(outerIn, salt...)
		outer := md5.Sum(outerIn)
		want := "md5" + hex.EncodeToString(outer[:])
		if clientUser != expectedUser || gotPass != want {
			_ = writeErrorResponse(nc, "28P01", "password authentication failed")
			return errors.New("md5 auth failed")
		}
		return writeAuthCode(nc, 0, nil)

	case AuthSCRAMSHA256:
		mech := []byte("SCRAM-SHA-256\x00\x00")
		if err := writeAuthCode(nc, 10, mech); err != nil {
			return err
		}
		typ, payload, err := readFrontendMsg(nc)
		if err != nil {
			return err
		}
		if typ != 'p' {
			_ = writeErrorResponse(nc, "08P01", "expected SASLInitialResponse")
			return errors.New("expected SASLInitialResponse")
		}
		idx := bytes.IndexByte(payload, 0)
		if idx < 0 || idx+5 > len(payload) {
			_ = writeErrorResponse(nc, "08P01", "malformed SASLInitialResponse")
			return errors.New("malformed SASLInitialResponse")
		}
		clientFirst := string(payload[idx+5:])
		clientFirstBare := strings.TrimPrefix(clientFirst, "n,,")
		var clientNonce string
		for _, part := range strings.Split(clientFirstBare, ",") {
			if strings.HasPrefix(part, "r=") {
				clientNonce = part[2:]
			}
		}
		if clientNonce == "" {
			_ = writeErrorResponse(nc, "08P01", "missing client nonce")
			return errors.New("missing client nonce")
		}
		combinedNonce := clientNonce + "srvnonce123456"
		salt := []byte("0123456789abcdef")
		iterations := 4096
		serverFirst := fmt.Sprintf("r=%s,s=%s,i=%d", combinedNonce, base64.StdEncoding.EncodeToString(salt), iterations)
		if err := writeAuthCode(nc, 11, []byte(serverFirst)); err != nil {
			return err
		}

		typ, payload, err = readFrontendMsg(nc)
		if err != nil {
			return err
		}
		if typ != 'p' {
			_ = writeErrorResponse(nc, "08P01", "expected SASLResponse")
			return errors.New("expected SASLResponse")
		}
		clientFinal := string(payload)
		pIdx := strings.LastIndex(clientFinal, ",p=")
		if pIdx < 0 {
			_ = writeErrorResponse(nc, "08P01", "missing client proof")
			return errors.New("missing client proof")
		}
		clientFinalNoProof := clientFinal[:pIdx]
		gotProofB64 := clientFinal[pIdx+3:]

		saltedPass := pbkdf2SHA256([]byte(expectedPass), salt, iterations, 32)
		clientKey := hmacSHA256(saltedPass, []byte("Client Key"))
		storedKey := sha256.Sum256(clientKey)
		authMsg := clientFirstBare + "," + serverFirst + "," + clientFinalNoProof
		clientSig := hmacSHA256(storedKey[:], []byte(authMsg))
		wantProof := make([]byte, len(clientKey))
		for i := range clientKey {
			wantProof[i] = clientKey[i] ^ clientSig[i]
		}
		if (expectedUser != "" && clientUser != expectedUser) || gotProofB64 != base64.StdEncoding.EncodeToString(wantProof) {
			_ = writeErrorResponse(nc, "28P01", "SCRAM password authentication failed")
			return errors.New("scram auth failed")
		}

		serverKey := hmacSHA256(saltedPass, []byte("Server Key"))
		serverSig := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(authMsg)))
		if err := writeAuthCode(nc, 12, []byte("v="+serverSig)); err != nil {
			return err
		}
		return writeAuthCode(nc, 0, nil)
	}
	return nil
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	numBlocks := (keyLen + sha256.Size - 1) / sha256.Size
	var out []byte
	for block := 1; block <= numBlocks; block++ {
		var blockBuf []byte
		blockBuf = append(blockBuf, salt...)
		blockBuf = append(blockBuf, byte(block>>24), byte(block>>16), byte(block>>8), byte(block))
		u := hmacSHA256(password, blockBuf)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 2; i <= iter; i++ {
			u = hmacSHA256(password, u)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

func readFrontendMsg(nc net.Conn) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(nc, hdr[:]); err != nil {
		return 0, nil, err
	}
	typ := hdr[0]
	length := int(binary.BigEndian.Uint32(hdr[1:5]))
	if length < 4 {
		return 0, nil, fmt.Errorf("pgtest: invalid frontend message length %d", length)
	}
	payload := make([]byte, length-4)
	if len(payload) > 0 {
		if _, err := io.ReadFull(nc, payload); err != nil {
			return 0, nil, err
		}
	}
	return typ, payload, nil
}

func writeMsg(nc net.Conn, typ byte, payload []byte) error {
	buf := make([]byte, 1+4+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := nc.Write(buf)
	return err
}

func writeAuthCode(nc net.Conn, code int32, extra []byte) error {
	payload := make([]byte, 4+len(extra))
	binary.BigEndian.PutUint32(payload[:4], uint32(code))
	copy(payload[4:], extra)
	return writeMsg(nc, 'R', payload)
}

func writeParameterStatus(nc net.Conn, name, value string) error {
	var buf bytes.Buffer
	buf.WriteString(name)
	buf.WriteByte(0)
	buf.WriteString(value)
	buf.WriteByte(0)
	return writeMsg(nc, 'S', buf.Bytes())
}

func writeBackendKeyData(nc net.Conn, pid, key int32) error {
	var payload [8]byte
	binary.BigEndian.PutUint32(payload[0:4], uint32(pid))
	binary.BigEndian.PutUint32(payload[4:8], uint32(key))
	return writeMsg(nc, 'K', payload[:])
}

func writeReadyForQuery(nc net.Conn, status byte) error {
	return writeMsg(nc, 'Z', []byte{status})
}

func writeCommandComplete(nc net.Conn, tag string) error {
	payload := make([]byte, len(tag)+1)
	copy(payload, tag)
	payload[len(tag)] = 0
	return writeMsg(nc, 'C', payload)
}

func writeErrorResponse(nc net.Conn, code, message string) error {
	var buf bytes.Buffer
	buf.WriteByte('S')
	buf.WriteString("ERROR")
	buf.WriteByte(0)
	buf.WriteByte('C')
	buf.WriteString(code)
	buf.WriteByte(0)
	buf.WriteByte('M')
	buf.WriteString(message)
	buf.WriteByte(0)
	buf.WriteByte(0)
	return writeMsg(nc, 'E', buf.Bytes())
}

func writeRowDescription(nc net.Conn, cols []string) error {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, int16(len(cols)))
	for _, c := range cols {
		buf.WriteString(c)
		buf.WriteByte(0)
		_ = binary.Write(&buf, binary.BigEndian, int32(0))  // table OID
		_ = binary.Write(&buf, binary.BigEndian, int16(0))  // attr num
		_ = binary.Write(&buf, binary.BigEndian, int32(25)) // TEXTOID
		_ = binary.Write(&buf, binary.BigEndian, int16(-1)) // type size
		_ = binary.Write(&buf, binary.BigEndian, int32(-1)) // type modifier
		_ = binary.Write(&buf, binary.BigEndian, int16(0))  // text format
	}
	return writeMsg(nc, 'T', buf.Bytes())
}

func writeDataRow(nc net.Conn, row []*string) error {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.BigEndian, int16(len(row)))
	for _, col := range row {
		if col == nil {
			_ = binary.Write(&buf, binary.BigEndian, int32(-1))
		} else {
			b := []byte(*col)
			_ = binary.Write(&buf, binary.BigEndian, int32(len(b)))
			buf.Write(b)
		}
	}
	return writeMsg(nc, 'D', buf.Bytes())
}

// --- SQL Tokenizer and Relational Executor ---

type tokenKind int

const (
	tokWord tokenKind = iota
	tokString
	tokNumber
	tokSymbol
)

type token struct {
	kind  tokenKind
	val   string
	upper string
}

func splitStatements(sqlText string) []string {
	var stmts []string
	var cur strings.Builder
	i := 0
	n := len(sqlText)
	for i < n {
		ch := sqlText[i]
		if ch == '\'' {
			cur.WriteByte(ch)
			i++
			for i < n {
				c := sqlText[i]
				cur.WriteByte(c)
				i++
				if c == '\'' {
					if i < n && sqlText[i] == '\'' {
						cur.WriteByte(sqlText[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}
		if ch == '-' && i+1 < n && sqlText[i+1] == '-' {
			i += 2
			for i < n && sqlText[i] != '\n' {
				i++
			}
			cur.WriteByte(' ')
			continue
		}
		if ch == '/' && i+1 < n && sqlText[i+1] == '*' {
			i += 2
			for i < n {
				if sqlText[i] == '*' && i+1 < n && sqlText[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			cur.WriteByte(' ')
			continue
		}
		if ch == ';' {
			s := strings.TrimSpace(cur.String())
			if s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
			i++
			continue
		}
		cur.WriteByte(ch)
		i++
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	return stmts
}

func tokenize(stmt string) ([]token, error) {
	var raw []token
	i := 0
	n := len(stmt)
	for i < n {
		ch := stmt[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			i++
			continue
		}
		if ch == '-' && i+1 < n && stmt[i+1] == '-' {
			i += 2
			for i < n && stmt[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '/' && i+1 < n && stmt[i+1] == '*' {
			i += 2
			for i < n {
				if stmt[i] == '*' && i+1 < n && stmt[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
			continue
		}
		if ch == '\'' {
			var sb strings.Builder
			i++
			closed := false
			for i < n {
				c := stmt[i]
				i++
				if c == '\'' {
					if i < n && stmt[i] == '\'' {
						sb.WriteByte('\'')
						i++
						continue
					}
					closed = true
					break
				}
				sb.WriteByte(c)
			}
			if !closed {
				return nil, &sqlError{code: "42601", message: "unterminated quoted string"}
			}
			raw = append(raw, token{kind: tokString, val: sb.String()})
			continue
		}
		if (ch >= '0' && ch <= '9') || (ch == '-' && i+1 < n && stmt[i+1] >= '0' && stmt[i+1] <= '9') {
			j := i
			if stmt[j] == '-' {
				j++
			}
			for j < n && ((stmt[j] >= '0' && stmt[j] <= '9') || stmt[j] == '.' || stmt[j] == 'e' || stmt[j] == 'E' || stmt[j] == '+' || stmt[j] == '-') {
				if (stmt[j] == '+' || stmt[j] == '-') && stmt[j-1] != 'e' && stmt[j-1] != 'E' {
					break
				}
				j++
			}
			numStr := stmt[i:j]
			raw = append(raw, token{kind: tokNumber, val: numStr, upper: numStr})
			i = j
			continue
		}
		if ch == ':' && i+1 < n && stmt[i+1] == ':' {
			raw = append(raw, token{kind: tokSymbol, val: "::", upper: "::"})
			i += 2
			continue
		}
		if ch == '<' && i+1 < n && stmt[i+1] == '>' {
			raw = append(raw, token{kind: tokSymbol, val: "<>", upper: "<>"})
			i += 2
			continue
		}
		if ch == '!' && i+1 < n && stmt[i+1] == '=' {
			raw = append(raw, token{kind: tokSymbol, val: "<>", upper: "<>"})
			i += 2
			continue
		}
		if strings.ContainsRune("(),=.*", rune(ch)) {
			s := string(ch)
			raw = append(raw, token{kind: tokSymbol, val: s, upper: s})
			i++
			continue
		}
		if isWordStart(ch) {
			j := i + 1
			for j < n && isWordPart(stmt[j]) {
				j++
			}
			w := stmt[i:j]
			raw = append(raw, token{kind: tokWord, val: w, upper: strings.ToUpper(w)})
			i = j
			continue
		}
		return nil, &sqlError{code: "42601", message: fmt.Sprintf("unexpected character %q in SQL", ch)}
	}

	// Validate ::jsonb typecasts and collapse NOW() and ::<type>.
	out := make([]token, 0, len(raw))
	for idx := 0; idx < len(raw); idx++ {
		t := raw[idx]
		if t.kind == tokSymbol && t.val == "::" {
			if idx+1 >= len(raw) {
				return nil, &sqlError{code: "42601", message: "missing type name after ::"}
			}
			typeName := raw[idx+1].upper
			if typeName == "JSONB" || typeName == "JSON" {
				if len(out) == 0 {
					return nil, &sqlError{code: "42601", message: "missing value before ::jsonb"}
				}
				prev := out[len(out)-1]
				if prev.kind == tokString {
					if !json.Valid([]byte(prev.val)) {
						return nil, &sqlError{code: "22P02", message: fmt.Sprintf("invalid input syntax for type json: %q", prev.val)}
					}
				} else if prev.upper != "NULL" {
					return nil, &sqlError{code: "22P02", message: "invalid input syntax for type json"}
				}
			}
			idx++ // skip type name
			continue
		}
		if t.kind == tokWord && t.upper == "NOW" && idx+2 < len(raw) && raw[idx+1].val == "(" && raw[idx+2].val == ")" {
			out = append(out, token{kind: tokWord, val: "now()", upper: "NOW()"})
			idx += 2
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func isWordStart(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
}

func isWordPart(ch byte) bool {
	return isWordStart(ch) || (ch >= '0' && ch <= '9')
}

func (s *Server) execStatement(stmt string, txState **dbState) ([]string, [][]*string, string, error) {
	tokens, err := tokenize(stmt)
	if err != nil {
		return nil, nil, "", err
	}
	if len(tokens) == 0 {
		return nil, nil, "", nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil, "", &sqlError{code: "57P01", message: "server is closed"}
	}

	switch tokens[0].upper {
	case "BEGIN", "START":
		if *txState == nil {
			*txState = s.state.clone()
		}
		return nil, nil, "BEGIN", nil

	case "COMMIT":
		if *txState != nil {
			s.state = *txState
			*txState = nil
		}
		return nil, nil, "COMMIT", nil

	case "ROLLBACK":
		*txState = nil
		return nil, nil, "ROLLBACK", nil
	}

	st := s.state
	if *txState != nil {
		st = *txState
	}

	switch tokens[0].upper {
	case "CREATE":
		return execCreate(st, tokens)
	case "ALTER":
		return execAlter(st, tokens)
	case "TRUNCATE":
		return execTruncate(st, tokens)
	case "INSERT":
		return execInsert(st, tokens)
	case "UPDATE":
		return execUpdate(st, tokens)
	case "DELETE":
		return execDelete(st, tokens)
	case "SELECT":
		return execSelect(st, tokens)
	default:
		return nil, nil, "", &sqlError{code: "42601", message: fmt.Sprintf("syntax error at or near %q", tokens[0].val)}
	}
}

func execCreate(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	if len(tokens) < 3 {
		return nil, nil, "", &sqlError{code: "42601", message: "incomplete CREATE statement"}
	}
	switch tokens[1].upper {
	case "TABLE":
		idx := 2
		if idx+2 < len(tokens) && tokens[idx].upper == "IF" && tokens[idx+1].upper == "NOT" && tokens[idx+2].upper == "EXISTS" {
			idx += 3
		}
		if idx >= len(tokens) {
			return nil, nil, "", &sqlError{code: "42601", message: "missing table name in CREATE TABLE"}
		}
		st.tables[strings.ToLower(tokens[idx].val)] = true
		return nil, nil, "CREATE TABLE", nil
	case "INDEX":
		return nil, nil, "CREATE INDEX", nil
	default:
		return nil, nil, "", &sqlError{code: "42601", message: fmt.Sprintf("unsupported CREATE %s", tokens[1].val)}
	}
}

func execAlter(_ *dbState, tokens []token) ([]string, [][]*string, string, error) {
	if len(tokens) < 3 || tokens[1].upper != "TABLE" {
		return nil, nil, "", &sqlError{code: "42601", message: "invalid ALTER statement"}
	}
	return nil, nil, "ALTER TABLE", nil
}

func execTruncate(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	idx := 1
	if idx < len(tokens) && tokens[idx].upper == "TABLE" {
		idx++
	}
	var tableNames []string
	for idx < len(tokens) {
		u := tokens[idx].upper
		if u == "RESTART" || u == "CONTINUE" || u == "CASCADE" || u == "RESTRICT" {
			break
		}
		if tokens[idx].val != "," {
			tableNames = append(tableNames, strings.ToLower(tokens[idx].val))
		}
		idx++
	}
	if len(tableNames) == 0 {
		return nil, nil, "", &sqlError{code: "42601", message: "missing table name in TRUNCATE"}
	}
	for _, tbl := range tableNames {
		switch tbl {
		case "sessions":
			st.sessions = make(map[sessionKey]*sessionRow)
		case "summaries":
			st.summaries = make(map[sessionKey]*summaryRow)
		case "session_files":
			st.sessionFiles = make(map[sessionFileKey]*sessionFileRow)
		case "eval_runs":
			st.evalRuns = make([]*evalRunRow, 0)
			st.nextRunID = 1
		default:
			return nil, nil, "", &sqlError{code: "42P01", message: fmt.Sprintf("relation %q does not exist", tbl)}
		}
	}
	return nil, nil, "TRUNCATE TABLE", nil
}

func parseParenTokens(tokens []token, start int) ([]token, int, error) {
	if start >= len(tokens) || tokens[start].val != "(" {
		return nil, start, &sqlError{code: "42601", message: "expected '('"}
	}
	var items []token
	i := start + 1
	for i < len(tokens) && tokens[i].val != ")" {
		if tokens[i].val != "," {
			items = append(items, tokens[i])
		}
		i++
	}
	if i >= len(tokens) || tokens[i].val != ")" {
		return nil, i, &sqlError{code: "42601", message: "expected ')'"}
	}
	return items, i + 1, nil
}

func execInsert(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	if len(tokens) < 4 || tokens[1].upper != "INTO" {
		return nil, nil, "", &sqlError{code: "42601", message: "invalid INSERT statement"}
	}
	table := strings.ToLower(tokens[2].val)
	cols, nextIdx, err := parseParenTokens(tokens, 3)
	if err != nil {
		return nil, nil, "", err
	}
	if nextIdx >= len(tokens) || tokens[nextIdx].upper != "VALUES" {
		return nil, nil, "", &sqlError{code: "42601", message: "expected VALUES in INSERT"}
	}
	vals, afterVals, err := parseParenTokens(tokens, nextIdx+1)
	if err != nil {
		return nil, nil, "", err
	}
	if len(cols) != len(vals) {
		return nil, nil, "", &sqlError{code: "42601", message: "INSERT column count does not match value count"}
	}

	colMap := make(map[string]token, len(cols))
	for i, c := range cols {
		colMap[strings.ToLower(c.val)] = vals[i]
	}

	hasOnConflict := afterVals < len(tokens) && tokens[afterVals].upper == "ON"

	switch table {
	case "sessions":
		source := tokenString(colMap["source"])
		id := tokenString(colMap["id"])
		title := tokenNullableString(colMap["title"])
		updatedAt, err := tokenTime(colMap["updated_at"])
		if err != nil {
			return nil, nil, "", err
		}
		steps := tokenString(colMap["steps"])
		if !json.Valid([]byte(steps)) {
			return nil, nil, "", &sqlError{code: "22P02", message: "invalid input syntax for type json"}
		}
		contentHash := tokenString(colMap["content_hash"])
		sourceMtime, err := strconv.ParseInt(colMap["source_mtime"].val, 10, 64)
		if err != nil {
			return nil, nil, "", &sqlError{code: "22P02", message: "invalid bigint for source_mtime"}
		}
		rawSource := tokenNullableString(colMap["raw_source"])

		key := sessionKey{source: source, id: id}
		existing, exists := st.sessions[key]
		if !exists {
			st.sessions[key] = &sessionRow{
				source:      source,
				id:          id,
				title:       title,
				updatedAt:   updatedAt,
				steps:       steps,
				contentHash: contentHash,
				sourceMtime: sourceMtime,
				rawSource:   rawSource,
				ingestedAt:  time.Now().UTC(),
			}
			return nil, nil, "INSERT 0 1", nil
		}
		if !hasOnConflict {
			return nil, nil, "", &sqlError{code: "23505", message: "duplicate key value violates unique constraint"}
		}
		if existing.contentHash == contentHash {
			return nil, nil, "INSERT 0 0", nil
		}
		existing.title = title
		existing.updatedAt = updatedAt
		existing.steps = steps
		existing.contentHash = contentHash
		existing.sourceMtime = sourceMtime
		existing.ingestedAt = time.Now().UTC()
		return nil, nil, "INSERT 0 1", nil

	case "summaries":
		source := tokenString(colMap["source"])
		sessionID := tokenString(colMap["session_id"])
		summary := tokenString(colMap["summary"])
		if !json.Valid([]byte(summary)) {
			return nil, nil, "", &sqlError{code: "22P02", message: "invalid input syntax for type json"}
		}
		shortTitle := tokenNullableString(colMap["short_title"])
		contentHash := tokenNullableString(colMap["content_hash"])

		key := sessionKey{source: source, id: sessionID}
		existing, exists := st.summaries[key]
		if !exists {
			st.summaries[key] = &summaryRow{
				source:      source,
				sessionID:   sessionID,
				summary:     summary,
				shortTitle:  shortTitle,
				contentHash: contentHash,
				updatedAt:   time.Now().UTC(),
			}
			return nil, nil, "INSERT 0 1", nil
		}
		if !hasOnConflict {
			return nil, nil, "", &sqlError{code: "23505", message: "duplicate key value violates unique constraint"}
		}
		if nullStrEqual(existing.contentHash, contentHash) {
			return nil, nil, "INSERT 0 0", nil
		}
		existing.summary = summary
		if shortTitle != nil {
			existing.shortTitle = shortTitle
		}
		existing.contentHash = contentHash
		existing.updatedAt = time.Now().UTC()
		return nil, nil, "INSERT 0 1", nil

	case "session_files":
		source := tokenString(colMap["source"])
		sessionID := tokenString(colMap["session_id"])
		path := tokenString(colMap["path"])
		content := tokenString(colMap["content"])

		key := sessionFileKey{source: source, sessionID: sessionID, path: path}
		existing, exists := st.sessionFiles[key]
		if !exists {
			st.sessionFiles[key] = &sessionFileRow{
				source:    source,
				sessionID: sessionID,
				path:      path,
				content:   content,
				updatedAt: time.Now().UTC(),
			}
			return nil, nil, "INSERT 0 1", nil
		}
		if !hasOnConflict {
			return nil, nil, "", &sqlError{code: "23505", message: "duplicate key value violates unique constraint"}
		}
		if existing.content == content {
			return nil, nil, "INSERT 0 0", nil
		}
		existing.content = content
		existing.updatedAt = time.Now().UTC()
		return nil, nil, "INSERT 0 1", nil

	case "eval_runs":
		savedAt := tokenString(colMap["saved_at"])
		flavor := tokenString(colMap["flavor"])
		modelLabel := tokenString(colMap["model_label"])
		sessionCount, _ := strconv.Atoi(colMap["session_count"].val)
		evaluatedSessions, _ := strconv.Atoi(colMap["evaluated_sessions"].val)
		avgScore, _ := strconv.ParseFloat(colMap["avg_score"].val, 64)
		checkPassRates := tokenString(colMap["check_pass_rates"])
		if !json.Valid([]byte(checkPassRates)) {
			return nil, nil, "", &sqlError{code: "22P02", message: "invalid input syntax for type json"}
		}
		judged := colMap["judged"].upper == "TRUE" || colMap["judged"].val == "t" || colMap["judged"].val == "1"
		judgedSessions, _ := strconv.Atoi(colMap["judged_sessions"].val)
		avgFaith, _ := strconv.ParseFloat(colMap["avg_faithfulness"].val, 64)
		avgAction, _ := strconv.ParseFloat(colMap["avg_actionability"].val, 64)
		avgClarity, _ := strconv.ParseFloat(colMap["avg_clarity"].val, 64)

		runID := st.nextRunID
		st.nextRunID++
		st.evalRuns = append(st.evalRuns, &evalRunRow{
			runID:             runID,
			savedAt:           savedAt,
			flavor:            flavor,
			modelLabel:        modelLabel,
			sessionCount:      sessionCount,
			evaluatedSessions: evaluatedSessions,
			avgScore:          avgScore,
			checkPassRates:    checkPassRates,
			judged:            judged,
			judgedSessions:    judgedSessions,
			avgFaithfulness:   avgFaith,
			avgActionability:  avgAction,
			avgClarity:        avgClarity,
		})
		return nil, nil, "INSERT 0 1", nil

	default:
		return nil, nil, "", &sqlError{code: "42P01", message: fmt.Sprintf("relation %q does not exist", table)}
	}
}

func execUpdate(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	if len(tokens) < 4 {
		return nil, nil, "", &sqlError{code: "42601", message: "invalid UPDATE statement"}
	}
	table := strings.ToLower(tokens[1].val)
	whereIdx := findKeyword(tokens, "WHERE")
	setTokens := tokens[3:]
	var whereTokens []token
	if whereIdx > 0 {
		setTokens = tokens[3:whereIdx]
		whereTokens = tokens[whereIdx+1:]
	}
	eqs, _ := parseWherePredicates(whereTokens)

	switch table {
	case "summaries":
		var count int
		for _, row := range st.summaries {
			if src, ok := eqs["source"]; ok && row.source != src {
				continue
			}
			if sid, ok := eqs["session_id"]; ok && row.sessionID != sid {
				continue
			}
			for i := 0; i+2 < len(setTokens); i++ {
				if setTokens[i+1].val == "=" {
					col := strings.ToLower(setTokens[i].val)
					valTok := setTokens[i+2]
					switch col {
					case "content_hash":
						row.contentHash = tokenNullableString(valTok)
					case "short_title":
						row.shortTitle = tokenNullableString(valTok)
					case "summary":
						row.summary = tokenString(valTok)
					}
					i += 2
				}
			}
			count++
		}
		return nil, nil, fmt.Sprintf("UPDATE %d", count), nil
	default:
		return nil, nil, "", &sqlError{code: "42P01", message: fmt.Sprintf("unsupported UPDATE on %q", table)}
	}
}

func execDelete(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	if len(tokens) < 3 || tokens[1].upper != "FROM" {
		return nil, nil, "", &sqlError{code: "42601", message: "invalid DELETE statement"}
	}
	table := strings.ToLower(tokens[2].val)
	whereIdx := findKeyword(tokens, "WHERE")
	var whereTokens []token
	if whereIdx > 0 {
		whereTokens = tokens[whereIdx+1:]
	}

	switch table {
	case "summaries":
		eqs, _ := parseWherePredicates(whereTokens)
		deleted := 0
		for k, row := range st.summaries {
			if src, ok := eqs["source"]; ok && row.source != src {
				continue
			}
			if sid, ok := eqs["session_id"]; ok && row.sessionID != sid {
				continue
			}
			delete(st.summaries, k)
			deleted++
		}
		return nil, nil, fmt.Sprintf("DELETE %d", deleted), nil

	case "session_files":
		eqs, _ := parseWherePredicates(whereTokens)
		deleted := 0
		for k, row := range st.sessionFiles {
			if src, ok := eqs["source"]; ok && row.source != src {
				continue
			}
			if sid, ok := eqs["session_id"]; ok && row.sessionID != sid {
				continue
			}
			if p, ok := eqs["path"]; ok && row.path != p {
				continue
			}
			delete(st.sessionFiles, k)
			deleted++
		}
		return nil, nil, fmt.Sprintf("DELETE %d", deleted), nil

	case "sessions":
		eqs, _ := parseWherePredicates(whereTokens)
		deleted := 0
		for k, row := range st.sessions {
			if src, ok := eqs["source"]; ok && row.source != src {
				continue
			}
			if id, ok := eqs["id"]; ok && row.id != id {
				continue
			}
			delete(st.sessions, k)
			deleted++
		}
		return nil, nil, fmt.Sprintf("DELETE %d", deleted), nil

	case "eval_runs":
		// Check for WHERE run_id NOT IN (... LIMIT N)
		if findKeyword(whereTokens, "NOT") >= 0 && findKeyword(whereTokens, "IN") >= 0 {
			limitIdx := findKeyword(whereTokens, "LIMIT")
			if limitIdx < 0 || limitIdx+1 >= len(whereTokens) {
				return nil, nil, "", &sqlError{code: "42601", message: "expected LIMIT in eval_runs trim query"}
			}
			limit, err := strconv.Atoi(whereTokens[limitIdx+1].val)
			if err != nil {
				return nil, nil, "", &sqlError{code: "22P02", message: "invalid LIMIT value"}
			}
			sorted := make([]*evalRunRow, len(st.evalRuns))
			copy(sorted, st.evalRuns)
			sortEvalRuns(sorted)
			if len(sorted) <= limit {
				return nil, nil, "DELETE 0", nil
			}
			keep := make(map[int64]bool, limit)
			for _, r := range sorted[:limit] {
				keep[r.runID] = true
			}
			remaining := make([]*evalRunRow, 0, limit)
			deleted := 0
			for _, r := range st.evalRuns {
				if keep[r.runID] {
					remaining = append(remaining, r)
				} else {
					deleted++
				}
			}
			st.evalRuns = remaining
			return nil, nil, fmt.Sprintf("DELETE %d", deleted), nil
		}

		eqs, _ := parseWherePredicates(whereTokens)
		remaining := make([]*evalRunRow, 0, len(st.evalRuns))
		deleted := 0
		for _, r := range st.evalRuns {
			match := true
			if savedAt, ok := eqs["saved_at"]; ok && r.savedAt != savedAt {
				match = false
			}
			if flavor, ok := eqs["flavor"]; ok && r.flavor != flavor {
				match = false
			}
			if match {
				deleted++
			} else {
				remaining = append(remaining, r)
			}
		}
		st.evalRuns = remaining
		return nil, nil, fmt.Sprintf("DELETE %d", deleted), nil

	default:
		return nil, nil, "", &sqlError{code: "42P01", message: fmt.Sprintf("relation %q does not exist", table)}
	}
}

func execSelect(st *dbState, tokens []token) ([]string, [][]*string, string, error) {
	fromIdx := findKeyword(tokens, "FROM")
	if fromIdx < 0 {
		// e.g. SELECT 1
		if len(tokens) == 2 && tokens[1].val == "1" {
			return []string{"?column?"}, [][]*string{{strPtr("1")}}, "SELECT 1", nil
		}
		return nil, nil, "", &sqlError{code: "42601", message: "unsupported SELECT without FROM"}
	}

	if fromIdx+1 >= len(tokens) {
		return nil, nil, "", &sqlError{code: "42601", message: "missing table name after FROM"}
	}
	table := strings.ToLower(tokens[fromIdx+1].val)
	selectTokens := tokens[1:fromIdx]

	whereIdx := findKeyword(tokens, "WHERE")
	orderIdx := findKeyword(tokens, "ORDER")
	limitIdx := findKeyword(tokens, "LIMIT")
	offsetIdx := findKeyword(tokens, "OFFSET")

	var whereTokens []token
	if whereIdx > 0 {
		end := len(tokens)
		for _, idx := range []int{orderIdx, limitIdx, offsetIdx} {
			if idx > whereIdx && idx < end {
				end = idx
			}
		}
		whereTokens = tokens[whereIdx+1 : end]
	}
	eqs, notNullCols := parseWherePredicates(whereTokens)

	limit := -1
	if limitIdx > 0 && limitIdx+1 < len(tokens) {
		if n, err := strconv.Atoi(tokens[limitIdx+1].val); err == nil {
			limit = n
		}
	}
	offset := 0
	if offsetIdx > 0 && offsetIdx+1 < len(tokens) {
		if n, err := strconv.Atoi(tokens[offsetIdx+1].val); err == nil {
			offset = n
		}
	}

	isCount := len(selectTokens) >= 1 && selectTokens[0].upper == "COUNT"

	switch table {
	case "sessions":
		var matched []*sessionRow
		for _, r := range st.sessions {
			if src, ok := eqs["source"]; ok && r.source != src {
				continue
			}
			if id, ok := eqs["id"]; ok && r.id != id {
				continue
			}
			matched = append(matched, r)
		}

		if isCount {
			c := strconv.Itoa(len(matched))
			return []string{"count"}, [][]*string{{strPtr(c)}}, "SELECT 1", nil
		}

		// Sort by updated_at DESC, id ASC when ORDER BY is present, or id ASC otherwise.
		if orderIdx > 0 {
			sort.Slice(matched, func(i, j int) bool {
				if !matched[i].updatedAt.Equal(matched[j].updatedAt) {
					return matched[i].updatedAt.After(matched[j].updatedAt)
				}
				return matched[i].id < matched[j].id
			})
		} else {
			sort.Slice(matched, func(i, j int) bool {
				return matched[i].id < matched[j].id
			})
		}

		if offset > 0 {
			if offset >= len(matched) {
				matched = nil
			} else {
				matched = matched[offset:]
			}
		}
		if limit >= 0 && limit < len(matched) {
			matched = matched[:limit]
		}

		hasJoin := findKeyword(tokens, "JOIN") > 0

		// Determine query projection shape.
		if hasJoin && findKeyword(selectTokens, "COALESCE") >= 0 {
			// sessionListSQL: SELECT s.id, COALESCE(NULLIF(m.short_title, ''), s.title) AS title, s.source_mtime
			rows := make([][]*string, 0, len(matched))
			for _, sRow := range matched {
				mRow := st.summaries[sessionKey{source: sRow.source, id: sRow.id}]
				var title string
				if mRow != nil && mRow.shortTitle != nil && *mRow.shortTitle != "" {
					title = *mRow.shortTitle
				} else if sRow.title != nil {
					title = *sRow.title
				}
				mtimeStr := strconv.FormatInt(sRow.sourceMtime, 10)
				rows = append(rows, []*string{strPtr(sRow.id), strPtr(title), strPtr(mtimeStr)})
			}
			return []string{"id", "title", "source_mtime"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil
		}

		if hasJoin {
			// sessionCollectSQL: SELECT s.id, s.steps, m.summary
			rows := make([][]*string, 0, len(matched))
			for _, sRow := range matched {
				mRow := st.summaries[sessionKey{source: sRow.source, id: sRow.id}]
				var sumPtr *string
				if mRow != nil {
					sumPtr = strPtr(mRow.summary)
				}
				rows = append(rows, []*string{strPtr(sRow.id), strPtr(sRow.steps), sumPtr})
			}
			return []string{"id", "steps", "summary"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil
		}

		if len(selectTokens) == 1 && selectTokens[0].val == "1" {
			rows := make([][]*string, 0, len(matched))
			for range matched {
				rows = append(rows, []*string{strPtr("1")})
			}
			return []string{"?column?"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil
		}

		if len(selectTokens) == 1 && strings.EqualFold(selectTokens[0].val, "steps") {
			rows := make([][]*string, 0, len(matched))
			for _, sRow := range matched {
				rows = append(rows, []*string{strPtr(sRow.steps)})
			}
			return []string{"steps"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil
		}

		// Default: SELECT id, content_hash
		rows := make([][]*string, 0, len(matched))
		for _, sRow := range matched {
			rows = append(rows, []*string{strPtr(sRow.id), strPtr(sRow.contentHash)})
		}
		return []string{"id", "content_hash"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil

	case "summaries":
		var matched []*summaryRow
		for _, r := range st.summaries {
			if src, ok := eqs["source"]; ok && r.source != src {
				continue
			}
			if sid, ok := eqs["session_id"]; ok && r.sessionID != sid {
				continue
			}
			if notNullCols["content_hash"] && r.contentHash == nil {
				continue
			}
			matched = append(matched, r)
		}

		if isCount {
			c := strconv.Itoa(len(matched))
			return []string{"count"}, [][]*string{{strPtr(c)}}, "SELECT 1", nil
		}

		sort.Slice(matched, func(i, j int) bool {
			return matched[i].sessionID < matched[j].sessionID
		})

		var colNames []string
		for _, t := range selectTokens {
			if t.kind == tokWord {
				colNames = append(colNames, strings.ToLower(t.val))
			}
		}

		rows := make([][]*string, 0, len(matched))
		for _, r := range matched {
			row := make([]*string, len(colNames))
			for i, col := range colNames {
				switch col {
				case "source":
					row[i] = strPtr(r.source)
				case "session_id":
					row[i] = strPtr(r.sessionID)
				case "summary":
					row[i] = strPtr(r.summary)
				case "short_title":
					row[i] = cloneStrPtr(r.shortTitle)
				case "content_hash":
					row[i] = cloneStrPtr(r.contentHash)
				}
			}
			rows = append(rows, row)
		}
		return colNames, rows, fmt.Sprintf("SELECT %d", len(rows)), nil

	case "session_files":
		var matched []*sessionFileRow
		for _, r := range st.sessionFiles {
			if src, ok := eqs["source"]; ok && r.source != src {
				continue
			}
			if sid, ok := eqs["session_id"]; ok && r.sessionID != sid {
				continue
			}
			if p, ok := eqs["path"]; ok && r.path != p {
				continue
			}
			matched = append(matched, r)
		}

		if isCount {
			c := strconv.Itoa(len(matched))
			return []string{"count"}, [][]*string{{strPtr(c)}}, "SELECT 1", nil
		}

		sort.Slice(matched, func(i, j int) bool {
			return matched[i].path < matched[j].path
		})

		rows := make([][]*string, 0, len(matched))
		for _, r := range matched {
			rows = append(rows, []*string{strPtr(r.content)})
		}
		return []string{"content"}, rows, fmt.Sprintf("SELECT %d", len(rows)), nil

	case "eval_runs":
		var matched []*evalRunRow
		for _, r := range st.evalRuns {
			if flavor, ok := eqs["flavor"]; ok && r.flavor != flavor {
				continue
			}
			if savedAt, ok := eqs["saved_at"]; ok && r.savedAt != savedAt {
				continue
			}
			matched = append(matched, r)
		}

		if isCount {
			c := strconv.Itoa(len(matched))
			return []string{"count"}, [][]*string{{strPtr(c)}}, "SELECT 1", nil
		}

		sortEvalRuns(matched)
		if limit >= 0 && limit < len(matched) {
			matched = matched[:limit]
		}

		cols := []string{
			"saved_at", "flavor", "model_label", "session_count", "evaluated_sessions",
			"avg_score", "check_pass_rates", "judged", "judged_sessions",
			"avg_faithfulness", "avg_actionability", "avg_clarity",
		}
		rows := make([][]*string, 0, len(matched))
		for _, r := range matched {
			judgedStr := "f"
			if r.judged {
				judgedStr = "t"
			}
			rows = append(rows, []*string{
				strPtr(r.savedAt),
				strPtr(r.flavor),
				strPtr(r.modelLabel),
				strPtr(strconv.Itoa(r.sessionCount)),
				strPtr(strconv.Itoa(r.evaluatedSessions)),
				strPtr(strconv.FormatFloat(r.avgScore, 'f', -1, 64)),
				strPtr(r.checkPassRates),
				strPtr(judgedStr),
				strPtr(strconv.Itoa(r.judgedSessions)),
				strPtr(strconv.FormatFloat(r.avgFaithfulness, 'f', -1, 64)),
				strPtr(strconv.FormatFloat(r.avgActionability, 'f', -1, 64)),
				strPtr(strconv.FormatFloat(r.avgClarity, 'f', -1, 64)),
			})
		}
		return cols, rows, fmt.Sprintf("SELECT %d", len(rows)), nil

	default:
		return nil, nil, "", &sqlError{code: "42P01", message: fmt.Sprintf("relation %q does not exist", table)}
	}
}

func sortEvalRuns(runs []*evalRunRow) {
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].savedAt != runs[j].savedAt {
			return runs[i].savedAt > runs[j].savedAt
		}
		return runs[i].runID > runs[j].runID
	})
}

func findKeyword(tokens []token, kw string) int {
	for i, t := range tokens {
		if t.kind == tokWord && t.upper == kw {
			return i
		}
	}
	return -1
}

func parseWherePredicates(tokens []token) (map[string]string, map[string]bool) {
	eqs := make(map[string]string)
	notNull := make(map[string]bool)
	for i := 0; i < len(tokens); i++ {
		// Handle table alias prefix like s.source = 'codex'
		col := strings.ToLower(tokens[i].val)
		if i+2 < len(tokens) && tokens[i+1].val == "." {
			col = strings.ToLower(tokens[i+2].val)
			i += 2
		}
		if i+2 < len(tokens) && tokens[i+1].val == "=" {
			eqs[col] = tokenString(tokens[i+2])
			i += 2
			continue
		}
		if i+3 < len(tokens) && tokens[i+1].upper == "IS" && tokens[i+2].upper == "NOT" && tokens[i+3].upper == "NULL" {
			notNull[col] = true
			i += 3
			continue
		}
	}
	return eqs, notNull
}

func tokenString(t token) string {
	return t.val
}

func tokenNullableString(t token) *string {
	if t.kind == tokWord && (t.upper == "NULL" || t.upper == "") {
		return nil
	}
	v := t.val
	return &v
}

func tokenTime(t token) (time.Time, error) {
	if t.upper == "NOW()" {
		return time.Now().UTC(), nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, t.val)
	if err != nil {
		return time.Time{}, &sqlError{code: "22007", message: fmt.Sprintf("invalid timestamp %q: %v", t.val, err)}
	}
	return parsed.UTC(), nil
}
