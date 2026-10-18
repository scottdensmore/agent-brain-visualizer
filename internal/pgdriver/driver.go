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

// Package pgdriver implements a pure-Go database/sql driver speaking the
// PostgreSQL v3 wire protocol over TCP, Unix sockets, or in-process net.Pipe
// connections for sandboxed testing.
package pgdriver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DriverName = "postgres"

func init() {
	sql.Register(DriverName, &Driver{})
}

// PipeDialer dials an in-process net.Conn for a pgtest://<id> DSN.
type PipeDialer func(ctx context.Context) (net.Conn, error)

var (
	pipeDialersMu sync.RWMutex
	pipeDialers   = make(map[string]PipeDialer)
)

// RegisterPipeDialer registers an in-process dialer for "pgtest://<id>" DSNs.
func RegisterPipeDialer(id string, dial PipeDialer) {
	pipeDialersMu.Lock()
	defer pipeDialersMu.Unlock()
	pipeDialers[id] = dial
}

// UnregisterPipeDialer removes a previously registered pipe dialer.
func UnregisterPipeDialer(id string) {
	pipeDialersMu.Lock()
	defer pipeDialersMu.Unlock()
	delete(pipeDialers, id)
}

func lookupPipeDialer(id string) (PipeDialer, bool) {
	pipeDialersMu.RLock()
	defer pipeDialersMu.RUnlock()
	d, ok := pipeDialers[id]
	return d, ok
}

// Config holds parsed PostgreSQL connection parameters.
type Config struct {
	Network  string
	Addr     string
	Host     string
	Port     string
	User     string
	Password string
	Database string
	PipeID   string
	Params   map[string]string
}

// ParseDSN parses a PostgreSQL DSN in any of the supported formats:
//   - jdbc:postgresql://host[:port]/database[?param=val...]
//   - postgres://[user[:password]@]host[:port]/database[?param=val...]
//   - postgresql://[user[:password]@]host[:port]/database[?param=val...]
//   - pgtest://<serverID>[/database][?param=val...]
//   - keyword=value pairs (host=... port=... user=... password=... dbname=...)
func ParseDSN(dsn, fallbackUser, fallbackPassword string) (*Config, error) {
	trimmed := strings.TrimSpace(dsn)
	if trimmed == "" {
		return nil, errors.New("pgdriver: empty DSN")
	}

	cfg := &Config{
		Network:  "tcp",
		Host:     "localhost",
		Port:     "5432",
		User:     fallbackUser,
		Password: fallbackPassword,
		Database: "postgres",
		Params:   make(map[string]string),
	}

	if strings.HasPrefix(trimmed, "jdbc:postgresql:") {
		trimmed = "postgres:" + strings.TrimPrefix(trimmed, "jdbc:postgresql:")
	}

	if strings.HasPrefix(trimmed, "pgtest://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("pgdriver: invalid pgtest DSN: %w", err)
		}
		pipeID := u.Host
		if pipeID == "" {
			pipeID = strings.TrimPrefix(u.Path, "/")
		}
		if pipeID == "" {
			return nil, errors.New("pgdriver: pgtest DSN missing server ID")
		}
		cfg.Network = "pipe"
		cfg.PipeID = pipeID
		cfg.Host = pipeID
		cfg.Port = "0"
		cfg.Addr = pipeID
		if u.User != nil {
			if u.User.Username() != "" {
				cfg.User = u.User.Username()
			}
			if pw, ok := u.User.Password(); ok {
				cfg.Password = pw
			}
		}
		if db := strings.TrimPrefix(u.Path, "/"); db != "" && u.Host != "" {
			cfg.Database = db
		}
		applyQueryParams(cfg, u.Query())
		if cfg.User == "" {
			cfg.User = "postgres"
		}
		return cfg, nil
	}

	if strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return nil, fmt.Errorf("pgdriver: invalid URL DSN: %w", err)
		}
		if u.User != nil {
			if u.User.Username() != "" {
				cfg.User = u.User.Username()
			}
			if pw, ok := u.User.Password(); ok {
				cfg.Password = pw
			}
		}
		if u.Host != "" {
			if h, p, err := net.SplitHostPort(u.Host); err == nil {
				if h != "" {
					cfg.Host = h
				}
				if p != "" {
					cfg.Port = p
				}
			} else {
				cfg.Host = strings.Trim(u.Host, "[]")
			}
		}
		if db := strings.TrimPrefix(u.Path, "/"); db != "" {
			cfg.Database = db
		}
		applyQueryParams(cfg, u.Query())
		cfg.Addr = net.JoinHostPort(cfg.Host, cfg.Port)
		if cfg.User == "" {
			cfg.User = "postgres"
		}
		return cfg, nil
	}

	// Parse keyword=value DSN format.
	if strings.Contains(trimmed, "=") {
		for _, part := range strings.Fields(trimmed) {
			k, v, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			v = strings.Trim(v, "'\"")
			switch strings.ToLower(k) {
			case "host":
				cfg.Host = v
			case "port":
				cfg.Port = v
			case "user", "username":
				cfg.User = v
			case "password":
				cfg.Password = v
			case "dbname", "database":
				cfg.Database = v
			default:
				cfg.Params[k] = v
			}
		}
		cfg.Addr = net.JoinHostPort(cfg.Host, cfg.Port)
		if cfg.User == "" {
			cfg.User = "postgres"
		}
		return cfg, nil
	}

	return nil, fmt.Errorf("pgdriver: unsupported DSN format %q", dsn)
}

func applyQueryParams(cfg *Config, q url.Values) {
	for k, vals := range q {
		if len(vals) == 0 {
			continue
		}
		v := vals[len(vals)-1]
		switch strings.ToLower(k) {
		case "user", "username":
			if v != "" {
				cfg.User = v
			}
		case "password":
			cfg.Password = v
		case "dbname", "database":
			if v != "" {
				cfg.Database = v
			}
		case "host":
			if v != "" {
				cfg.Host = v
			}
		case "port":
			if v != "" {
				cfg.Port = v
			}
		default:
			cfg.Params[k] = v
		}
	}
}

// Open opens a *sql.DB using the pure-Go PostgreSQL v3 wire driver.
func Open(dsn, user, password string) (*sql.DB, error) {
	cfg, err := ParseDSN(dsn, user, password)
	if err != nil {
		return nil, err
	}
	return sql.OpenDB(&connector{cfg: cfg}), nil
}

// Driver implements database/sql/driver.Driver and driver.DriverContext.
type Driver struct{}

// Open implements driver.Driver.
func (d *Driver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

// OpenConnector implements driver.DriverContext.
func (d *Driver) OpenConnector(name string) (driver.Connector, error) {
	cfg, err := ParseDSN(name, "", "")
	if err != nil {
		return nil, err
	}
	return &connector{cfg: cfg}, nil
}

type connector struct {
	cfg *Config
}

func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	return dialConn(ctx, c.cfg)
}

func (c *connector) Driver() driver.Driver {
	return &Driver{}
}

// PGError represents an ErrorResponse ('E') returned by PostgreSQL.
type PGError struct {
	Severity string
	Code     string
	Message  string
	Detail   string
}

func (e *PGError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("postgres %s (%s): %s", e.Severity, e.Code, e.Message)
	}
	return fmt.Sprintf("postgres: %s", e.Message)
}

type pgConn struct {
	nc        net.Conn
	cfg       *Config
	mu        sync.Mutex
	bad       bool
	inTx      bool
	processID int32
	secretKey int32
}

func dialConn(ctx context.Context, cfg *Config) (*pgConn, error) {
	var (
		nc  net.Conn
		err error
	)
	if cfg.Network == "pipe" {
		dialer, ok := lookupPipeDialer(cfg.PipeID)
		if !ok {
			return nil, fmt.Errorf("pgdriver: unknown pgtest server %q", cfg.PipeID)
		}
		nc, err = dialer(ctx)
	} else {
		var d net.Dialer
		nc, err = d.DialContext(ctx, cfg.Network, cfg.Addr)
	}
	if err != nil {
		return nil, err
	}

	c := &pgConn{
		nc:  nc,
		cfg: cfg,
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(deadline)
		defer func() { _ = nc.SetDeadline(time.Time{}) }()
	}
	if err := c.startup(); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return c, nil
}

func (c *pgConn) startup() error {
	var body bytes.Buffer
	// Protocol version 3.0 (0x00030000 = 196608).
	_ = binary.Write(&body, binary.BigEndian, int32(196608))
	body.WriteString("user\x00")
	body.WriteString(c.cfg.User)
	body.WriteByte(0)
	if c.cfg.Database != "" {
		body.WriteString("database\x00")
		body.WriteString(c.cfg.Database)
		body.WriteByte(0)
	}
	body.WriteByte(0)

	var frame bytes.Buffer
	_ = binary.Write(&frame, binary.BigEndian, int32(4+body.Len()))
	frame.Write(body.Bytes())

	if _, err := c.nc.Write(frame.Bytes()); err != nil {
		return err
	}

	for {
		typ, payload, err := c.readMsg()
		if err != nil {
			return err
		}
		switch typ {
		case 'R':
			if err := c.handleAuth(payload); err != nil {
				return err
			}
		case 'S':
			// ParameterStatus: ignore
		case 'K':
			if len(payload) >= 8 {
				c.processID = int32(binary.BigEndian.Uint32(payload[0:4]))
				c.secretKey = int32(binary.BigEndian.Uint32(payload[4:8]))
			}
		case 'N':
			// NoticeResponse: ignore
		case 'E':
			return parseErrorResponse(payload)
		case 'Z':
			if len(payload) >= 1 {
				c.inTx = payload[0] == 'T' || payload[0] == 'E'
			}
			return nil
		default:
			return fmt.Errorf("pgdriver: unexpected startup message type %q", typ)
		}
	}
}

func (c *pgConn) handleAuth(payload []byte) error {
	if len(payload) < 4 {
		return errors.New("pgdriver: truncated Authentication message")
	}
	code := int32(binary.BigEndian.Uint32(payload[:4]))
	switch code {
	case 0: // AuthenticationOk
		return nil
	case 3: // AuthenticationCleartextPassword
		var buf bytes.Buffer
		buf.WriteString(c.cfg.Password)
		buf.WriteByte(0)
		return c.writeMsg('p', buf.Bytes())
	case 5: // AuthenticationMD5Password
		if len(payload) < 8 {
			return errors.New("pgdriver: truncated MD5 salt")
		}
		salt := payload[4:8]
		inner := md5.Sum([]byte(c.cfg.Password + c.cfg.User))
		innerHex := hex.EncodeToString(inner[:])
		var outerInput []byte
		outerInput = append(outerInput, innerHex...)
		outerInput = append(outerInput, salt...)
		outer := md5.Sum(outerInput)
		md5Pass := "md5" + hex.EncodeToString(outer[:])
		var buf bytes.Buffer
		buf.WriteString(md5Pass)
		buf.WriteByte(0)
		return c.writeMsg('p', buf.Bytes())
	case 10: // AuthenticationSASL
		return c.handleSCRAM(payload[4:])
	default:
		return fmt.Errorf("pgdriver: unsupported authentication method %d", code)
	}
}

func (c *pgConn) handleSCRAM(mechBytes []byte) error {
	supportsSCRAM := false
	for _, m := range bytes.Split(mechBytes, []byte{0}) {
		if string(m) == "SCRAM-SHA-256" {
			supportsSCRAM = true
			break
		}
	}
	if !supportsSCRAM {
		return errors.New("pgdriver: server does not offer SCRAM-SHA-256")
	}

	nonceBytes := make([]byte, 18)
	if _, err := io.ReadFull(rand.Reader, nonceBytes); err != nil {
		return err
	}
	clientNonce := base64.StdEncoding.EncodeToString(nonceBytes)
	clientFirstBare := "n=,r=" + clientNonce
	clientFirstMsg := "n,," + clientFirstBare

	var initBuf bytes.Buffer
	initBuf.WriteString("SCRAM-SHA-256")
	initBuf.WriteByte(0)
	_ = binary.Write(&initBuf, binary.BigEndian, int32(len(clientFirstMsg)))
	initBuf.WriteString(clientFirstMsg)
	if err := c.writeMsg('p', initBuf.Bytes()); err != nil {
		return err
	}

	// Read SASLContinue (code 11).
	typ, payload, err := c.readMsg()
	if err != nil {
		return err
	}
	if typ == 'E' {
		return parseErrorResponse(payload)
	}
	if typ != 'R' || len(payload) < 4 || int32(binary.BigEndian.Uint32(payload[:4])) != 11 {
		return fmt.Errorf("pgdriver: expected AuthenticationSASLContinue, got %q", typ)
	}
	serverFirstMsg := string(payload[4:])
	combinedNonce, salt, iterations, err := parseSCRAMServerFirst(serverFirstMsg)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(combinedNonce, clientNonce) {
		return errors.New("pgdriver: SCRAM server nonce does not start with client nonce")
	}

	saltedPassword := pbkdf2SHA256([]byte(c.cfg.Password), salt, iterations, 32)
	clientKey := hmacSHA256(saltedPassword, []byte("Client Key"))
	storedKey := sha256.Sum256(clientKey)
	clientFinalNoProof := "c=biws,r=" + combinedNonce
	authMsg := clientFirstBare + "," + serverFirstMsg + "," + clientFinalNoProof
	clientSig := hmacSHA256(storedKey[:], []byte(authMsg))
	clientProof := make([]byte, len(clientKey))
	for i := range clientKey {
		clientProof[i] = clientKey[i] ^ clientSig[i]
	}
	clientFinalMsg := clientFinalNoProof + ",p=" + base64.StdEncoding.EncodeToString(clientProof)
	if err := c.writeMsg('p', []byte(clientFinalMsg)); err != nil {
		return err
	}

	serverKey := hmacSHA256(saltedPassword, []byte("Server Key"))
	expectedServerSig := base64.StdEncoding.EncodeToString(hmacSHA256(serverKey, []byte(authMsg)))

	// Read SASLFinal (code 12).
	typ, payload, err = c.readMsg()
	if err != nil {
		return err
	}
	if typ == 'E' {
		return parseErrorResponse(payload)
	}
	if typ != 'R' || len(payload) < 4 || int32(binary.BigEndian.Uint32(payload[:4])) != 12 {
		return fmt.Errorf("pgdriver: expected AuthenticationSASLFinal, got %q", typ)
	}
	serverFinal := string(payload[4:])
	if !strings.HasPrefix(serverFinal, "v=") || strings.TrimPrefix(serverFinal, "v=") != expectedServerSig {
		return errors.New("pgdriver: SCRAM server signature verification failed")
	}
	return nil
}

func parseSCRAMServerFirst(msg string) (nonce string, salt []byte, iterations int, err error) {
	for _, part := range strings.Split(msg, ",") {
		switch {
		case strings.HasPrefix(part, "r="):
			nonce = part[2:]
		case strings.HasPrefix(part, "s="):
			salt, err = base64.StdEncoding.DecodeString(part[2:])
			if err != nil {
				return "", nil, 0, fmt.Errorf("pgdriver: invalid SCRAM salt: %w", err)
			}
		case strings.HasPrefix(part, "i="):
			iterations, err = strconv.Atoi(part[2:])
			if err != nil || iterations <= 0 {
				return "", nil, 0, fmt.Errorf("pgdriver: invalid SCRAM iteration count %q", part[2:])
			}
		}
	}
	if nonce == "" || len(salt) == 0 || iterations <= 0 {
		return "", nil, 0, fmt.Errorf("pgdriver: incomplete SCRAM server-first-message %q", msg)
	}
	return nonce, salt, iterations, nil
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

func (c *pgConn) writeMsg(typ byte, payload []byte) error {
	buf := make([]byte, 1+4+len(payload))
	buf[0] = typ
	binary.BigEndian.PutUint32(buf[1:5], uint32(4+len(payload)))
	copy(buf[5:], payload)
	_, err := c.nc.Write(buf)
	if err != nil {
		c.bad = true
	}
	return err
}

func (c *pgConn) readMsg() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.nc, hdr[:]); err != nil {
		c.bad = true
		return 0, nil, err
	}
	typ := hdr[0]
	length := int(binary.BigEndian.Uint32(hdr[1:5]))
	if length < 4 {
		c.bad = true
		return 0, nil, fmt.Errorf("pgdriver: invalid message length %d", length)
	}
	payload := make([]byte, length-4)
	if len(payload) > 0 {
		if _, err := io.ReadFull(c.nc, payload); err != nil {
			c.bad = true
			return 0, nil, err
		}
	}
	return typ, payload, nil
}

func parseErrorResponse(payload []byte) error {
	pgErr := &PGError{}
	i := 0
	for i < len(payload) {
		fieldType := payload[i]
		i++
		if fieldType == 0 {
			break
		}
		end := bytes.IndexByte(payload[i:], 0)
		if end < 0 {
			break
		}
		val := string(payload[i : i+end])
		i += end + 1
		switch fieldType {
		case 'S':
			if pgErr.Severity == "" {
				pgErr.Severity = val
			}
		case 'V':
			pgErr.Severity = val
		case 'C':
			pgErr.Code = val
		case 'M':
			pgErr.Message = val
		case 'D':
			pgErr.Detail = val
		}
	}
	if pgErr.Message == "" {
		pgErr.Message = "unknown database error"
	}
	return pgErr
}

// InterpolateParams substitutes PostgreSQL positional placeholders ($1, $2, ...)
// with safely escaped SQL literals outside of string literals and comments.
func InterpolateParams(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	byOrdinal := make(map[int]driver.Value, len(args))
	for i, a := range args {
		ord := a.Ordinal
		if ord <= 0 {
			ord = i + 1
		}
		byOrdinal[ord] = a.Value
	}

	var out strings.Builder
	out.Grow(len(query) + len(args)*16)

	i := 0
	n := len(query)
	for i < n {
		ch := query[i]

		// Single-quoted string literal
		if ch == '\'' {
			out.WriteByte(ch)
			i++
			for i < n {
				c := query[i]
				out.WriteByte(c)
				i++
				if c == '\'' {
					if i < n && query[i] == '\'' {
						out.WriteByte(query[i])
						i++
						continue
					}
					break
				}
			}
			continue
		}

		// Line comment --
		if ch == '-' && i+1 < n && query[i+1] == '-' {
			for i < n && query[i] != '\n' {
				out.WriteByte(query[i])
				i++
			}
			continue
		}

		// Block comment /* ... */
		if ch == '/' && i+1 < n && query[i+1] == '*' {
			out.WriteString("/*")
			i += 2
			for i < n {
				if query[i] == '*' && i+1 < n && query[i+1] == '/' {
					out.WriteString("*/")
					i += 2
					break
				}
				out.WriteByte(query[i])
				i++
			}
			continue
		}

		// Positional parameter $N
		if ch == '$' && i+1 < n && query[i+1] >= '1' && query[i+1] <= '9' {
			j := i + 1
			for j < n && query[j] >= '0' && query[j] <= '9' {
				j++
			}
			ord, err := strconv.Atoi(query[i+1 : j])
			if err != nil {
				return "", fmt.Errorf("pgdriver: invalid parameter placeholder %q: %w", query[i:j], err)
			}
			val, ok := byOrdinal[ord]
			if !ok {
				return "", fmt.Errorf("pgdriver: missing value for parameter $%d", ord)
			}
			lit, err := formatLiteral(val)
			if err != nil {
				return "", err
			}
			out.WriteString(lit)
			i = j
			continue
		}

		out.WriteByte(ch)
		i++
	}
	return out.String(), nil
}

func formatLiteral(v driver.Value) (string, error) {
	if v == nil {
		return "NULL", nil
	}
	switch val := v.(type) {
	case string:
		return "'" + strings.ReplaceAll(val, "'", "''") + "'", nil
	case []byte:
		return "'" + strings.ReplaceAll(string(val), "'", "''") + "'", nil
	case bool:
		if val {
			return "TRUE", nil
		}
		return "FALSE", nil
	case int64:
		return strconv.FormatInt(val, 10), nil
	case int:
		return strconv.Itoa(val), nil
	case float64:
		return strconv.FormatFloat(val, 'f', -1, 64), nil
	case time.Time:
		return "'" + val.UTC().Format(time.RFC3339Nano) + "'", nil
	default:
		return "", fmt.Errorf("pgdriver: unsupported parameter type %T", v)
	}
}

func (c *pgConn) simpleQuery(ctx context.Context, sqlText string) ([]string, [][]*string, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.bad {
		return nil, nil, 0, driver.ErrBadConn
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = c.nc.SetDeadline(deadline)
		defer func() { _ = c.nc.SetDeadline(time.Time{}) }()
	}

	payload := make([]byte, len(sqlText)+1)
	copy(payload, sqlText)
	payload[len(sqlText)] = 0
	if err := c.writeMsg('Q', payload); err != nil {
		return nil, nil, 0, err
	}

	var (
		columns      []string
		rows         [][]*string
		rowsAffected int64
		queryErr     error
	)

	for {
		typ, msg, err := c.readMsg()
		if err != nil {
			return nil, nil, 0, err
		}
		switch typ {
		case 'T': // RowDescription
			cols, err := parseRowDescription(msg)
			if err != nil {
				queryErr = err
			} else {
				columns = cols
			}
		case 'D': // DataRow
			row, err := parseDataRow(msg)
			if err != nil {
				queryErr = err
			} else {
				rows = append(rows, row)
			}
		case 'C': // CommandComplete
			tag := strings.TrimRight(string(msg), "\x00")
			rowsAffected += parseCommandCompleteCount(tag)
		case 'I': // EmptyQueryResponse
		case 'E': // ErrorResponse
			if queryErr == nil {
				queryErr = parseErrorResponse(msg)
			}
		case 'N', 'S': // NoticeResponse, ParameterStatus
		case 'Z': // ReadyForQuery
			if len(msg) >= 1 {
				c.inTx = msg[0] == 'T' || msg[0] == 'E'
			}
			return columns, rows, rowsAffected, queryErr
		default:
			if queryErr == nil {
				queryErr = fmt.Errorf("pgdriver: unexpected wire message %q", typ)
			}
		}
	}
}

func parseRowDescription(msg []byte) ([]string, error) {
	if len(msg) < 2 {
		return nil, errors.New("pgdriver: truncated RowDescription")
	}
	count := int(binary.BigEndian.Uint16(msg[:2]))
	cols := make([]string, 0, count)
	i := 2
	for c := 0; c < count; c++ {
		end := bytes.IndexByte(msg[i:], 0)
		if end < 0 {
			return nil, errors.New("pgdriver: truncated column name in RowDescription")
		}
		name := string(msg[i : i+end])
		i += end + 1
		// Skip table OID (4), attr num (2), type OID (4), type size (2), type mod (4), format (2) = 18 bytes.
		if i+18 > len(msg) {
			return nil, errors.New("pgdriver: truncated field metadata in RowDescription")
		}
		i += 18
		cols = append(cols, name)
	}
	return cols, nil
}

func parseDataRow(msg []byte) ([]*string, error) {
	if len(msg) < 2 {
		return nil, errors.New("pgdriver: truncated DataRow")
	}
	count := int(binary.BigEndian.Uint16(msg[:2]))
	row := make([]*string, count)
	i := 2
	for c := 0; c < count; c++ {
		if i+4 > len(msg) {
			return nil, errors.New("pgdriver: truncated column length in DataRow")
		}
		colLen := int(int32(binary.BigEndian.Uint32(msg[i : i+4])))
		i += 4
		if colLen < 0 {
			row[c] = nil
			continue
		}
		if i+colLen > len(msg) {
			return nil, errors.New("pgdriver: truncated column data in DataRow")
		}
		s := string(msg[i : i+colLen])
		row[c] = &s
		i += colLen
	}
	return row, nil
}

func parseCommandCompleteCount(tag string) int64 {
	fields := strings.Fields(tag)
	if len(fields) == 0 {
		return 0
	}
	switch fields[0] {
	case "INSERT", "UPDATE", "DELETE", "MOVE", "FETCH", "COPY":
		if n, err := strconv.ParseInt(fields[len(fields)-1], 10, 64); err == nil {
			return n
		}
	}
	return 0
}

// ExecContext implements driver.ExecerContext.
func (c *pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	sqlText, err := InterpolateParams(query, args)
	if err != nil {
		return nil, err
	}
	_, _, affected, err := c.simpleQuery(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(affected), nil
}

// QueryContext implements driver.QueryerContext.
func (c *pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	sqlText, err := InterpolateParams(query, args)
	if err != nil {
		return nil, err
	}
	cols, data, _, err := c.simpleQuery(ctx, sqlText)
	if err != nil {
		return nil, err
	}
	return &pgRows{
		columns: cols,
		rows:    data,
	}, nil
}

// Ping implements driver.Pinger.
func (c *pgConn) Ping(ctx context.Context) error {
	_, _, _, err := c.simpleQuery(ctx, "SELECT 1")
	return err
}

// Prepare implements driver.Conn.
func (c *pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext implements driver.ConnPrepareContext.
func (c *pgConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	if c.bad {
		return nil, driver.ErrBadConn
	}
	return &pgStmt{conn: c, query: query}, nil
}

// Close implements driver.Conn.
func (c *pgConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.bad {
		_ = c.writeMsg('X', nil)
	}
	c.bad = true
	return c.nc.Close()
}

// Begin implements driver.Conn.
func (c *pgConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

// BeginTx implements driver.ConnBeginTx.
func (c *pgConn) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	if _, _, _, err := c.simpleQuery(ctx, "BEGIN"); err != nil {
		return nil, err
	}
	return &pgTx{conn: c}, nil
}

// CheckNamedValue implements driver.NamedValueChecker so time.Time and int values pass through.
func (c *pgConn) CheckNamedValue(_ *driver.NamedValue) error {
	return nil
}

type pgTx struct {
	conn *pgConn
}

func (tx *pgTx) Commit() error {
	_, _, _, err := tx.conn.simpleQuery(context.Background(), "COMMIT")
	return err
}

func (tx *pgTx) Rollback() error {
	_, _, _, err := tx.conn.simpleQuery(context.Background(), "ROLLBACK")
	return err
}

type pgStmt struct {
	conn  *pgConn
	query string
}

func (s *pgStmt) Close() error {
	return nil
}

func (s *pgStmt) NumInput() int {
	return -1
}

func (s *pgStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), valuesToNamed(args))
}

func (s *pgStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}

func (s *pgStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), valuesToNamed(args))
}

func (s *pgStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}

func valuesToNamed(args []driver.Value) []driver.NamedValue {
	named := make([]driver.NamedValue, len(args))
	for i, v := range args {
		named[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return named
}

type pgRows struct {
	columns []string
	rows    [][]*string
	idx     int
}

func (r *pgRows) Columns() []string {
	return r.columns
}

func (r *pgRows) Close() error {
	return nil
}

func (r *pgRows) Next(dest []driver.Value) error {
	if r.idx >= len(r.rows) {
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	for i := range dest {
		if i >= len(row) || row[i] == nil {
			dest[i] = nil
		} else {
			dest[i] = *row[i]
		}
	}
	return nil
}
