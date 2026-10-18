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

// Package config manages environment/.env loading, AI/API/Ingest/Server configuration,
// constant-time bearer token verification, startup security advisories, and log escaping.
package config

import (
	"crypto/subtle"
	"fmt"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf16"
)

var (
	exportPrefixRe = regexp.MustCompile(`^export\s+`)

	dotEnvMu      sync.Mutex
	dotEnvEnabled = true
	dotEnvPath    = ".env"
	dotEnvCached  map[string]string
)

// SetDotEnvEnabled controls whether .env files are loaded (disabled in tests).
func SetDotEnvEnabled(enabled bool) {
	dotEnvMu.Lock()
	defer dotEnvMu.Unlock()
	dotEnvEnabled = enabled
	dotEnvCached = nil
}

// SetDotEnvPath overrides the path to the .env file and clears the cache.
func SetDotEnvPath(path string) {
	dotEnvMu.Lock()
	defer dotEnvMu.Unlock()
	if strings.TrimSpace(path) == "" {
		dotEnvPath = ".env"
	} else {
		dotEnvPath = path
	}
	dotEnvCached = nil
}

// DotEnvEnabled reports whether .env loading is active.
func DotEnvEnabled() bool {
	dotEnvMu.Lock()
	defer dotEnvMu.Unlock()
	if v, ok := os.LookupEnv("DOTENV_ENABLED"); ok {
		return !strings.EqualFold(strings.TrimSpace(v), "false")
	}
	return dotEnvEnabled
}

// DotEnvValues returns the cached .env values, loading once on first access.
func DotEnvValues() map[string]string {
	dotEnvMu.Lock()
	defer dotEnvMu.Unlock()
	if dotEnvCached != nil {
		out := make(map[string]string, len(dotEnvCached))
		for k, v := range dotEnvCached {
			out[k] = v
		}
		return out
	}
	enabled := dotEnvEnabled
	if v, ok := os.LookupEnv("DOTENV_ENABLED"); ok {
		enabled = !strings.EqualFold(strings.TrimSpace(v), "false")
	}
	if !enabled {
		dotEnvCached = map[string]string{}
		return map[string]string{}
	}
	path := dotEnvPath
	if v := strings.TrimSpace(os.Getenv("DOTENV_PATH")); v != "" {
		path = v
	}
	dotEnvCached = LoadDotEnv(path)
	out := make(map[string]string, len(dotEnvCached))
	for k, v := range dotEnvCached {
		out[k] = v
	}
	return out
}

// DotEnvGet returns the .env value for key, or ("", false) when unset.
func DotEnvGet(key string) (string, bool) {
	vals := DotEnvValues()
	v, ok := vals[key]
	return v, ok
}

// LoadDotEnv reads and parses a .env file; a missing, directory, or unreadable path yields an empty map.
func LoadDotEnv(path string) map[string]string {
	if strings.TrimSpace(path) == "" {
		return map[string]string{}
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return map[string]string{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	return ParseDotEnv(lines)
}

// ParseDotEnv parses KEY=VALUE lines following the exact DotEnv rules:
// blank lines and '#' comments ignored, UTF-8 BOM stripped, leading 'export ' tolerated,
// single/double quotes stripped (dropping trailing comments after closing quote),
// and trailing ' # comment' dropped on unquoted values.
func ParseDotEnv(lines []string) map[string]string {
	values := make(map[string]string)
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "\uFEFF") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "\uFEFF"))
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = exportPrefixRe.ReplaceAllString(line, "")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		if key == "" {
			continue
		}
		values[key] = cleanDotEnvValue(strings.TrimSpace(line[eq+1:]))
	}
	return values
}

func cleanDotEnvValue(value string) string {
	if len(value) > 0 {
		quote := value[0]
		if quote == '"' || quote == '\'' {
			if closeIdx := strings.IndexByte(value[1:], quote); closeIdx >= 0 {
				return value[1 : closeIdx+1]
			}
		}
	}
	if comment := strings.Index(value, " #"); comment >= 0 {
		value = value[:comment]
	}
	return strings.TrimSpace(value)
}

// Env reads a real environment variable first (even if set to blank), falling back to DotEnv.
func Env(key string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	if v, ok := DotEnvGet(key); ok {
		return v
	}
	return ""
}

// EnvOr returns Env(key) if non-blank, otherwise fallback.
func EnvOr(key, fallback string) string {
	v := Env(key)
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

// ParseFlag parses a boolean-ish env flag: true/1/yes/on (case-insensitive) is true.
func ParseFlag(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "1", "yes", "on":
		return true
	default:
		return false
	}
}

// Provider identifies the configured LLM backend.
type Provider string

const (
	ProviderGemini Provider = "GEMINI"
	ProviderOllama Provider = "OLLAMA"

	DefaultGeminiModel   = "gemini-3.5-flash"
	DefaultOllamaBaseURL = "http://localhost:11434"
	DefaultOllamaModel   = "gemma4"
)

// AiConfig holds configuration for the AI backend (Gemini or Ollama).
type AiConfig struct {
	providerName  string
	geminiAPIKey  string
	geminiModel   string
	ollamaBaseURL string
	ollamaModel   string
}

// LoadAiConfig constructs an AiConfig from environment variables and .env.
func LoadAiConfig() *AiConfig {
	return NewAiConfig(
		Env("AI_PROVIDER"),
		Env("GEMINI_API_KEY"),
		EnvOr("GEMINI_MODEL", DefaultGeminiModel),
		EnvOr("OLLAMA_BASE_URL", DefaultOllamaBaseURL),
		EnvOr("OLLAMA_MODEL", DefaultOllamaModel),
	)
}

// NewAiConfig constructs an AiConfig with explicit values (test seam).
func NewAiConfig(providerName, geminiAPIKey, geminiModel, ollamaBaseURL, ollamaModel string) *AiConfig {
	return &AiConfig{
		providerName:  providerName,
		geminiAPIKey:  geminiAPIKey,
		geminiModel:   blankTo(geminiModel, DefaultGeminiModel),
		ollamaBaseURL: blankTo(ollamaBaseURL, DefaultOllamaBaseURL),
		ollamaModel:   blankTo(ollamaModel, DefaultOllamaModel),
	}
}

func blankTo(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// Provider returns ProviderOllama if providerName is "ollama" (case-insensitive), else ProviderGemini.
func (c *AiConfig) Provider() Provider {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.providerName), "ollama") {
		return ProviderOllama
	}
	return ProviderGemini
}

// GeminiAPIKey returns the configured Gemini API key and true if non-blank.
func (c *AiConfig) GeminiAPIKey() (string, bool) {
	if c == nil || strings.TrimSpace(c.geminiAPIKey) == "" {
		return "", false
	}
	return c.geminiAPIKey, true
}

// GeminiModel returns the configured Gemini model name.
func (c *AiConfig) GeminiModel() string {
	if c == nil || strings.TrimSpace(c.geminiModel) == "" {
		return DefaultGeminiModel
	}
	return c.geminiModel
}

// OllamaBaseURL returns the configured Ollama server base URL.
func (c *AiConfig) OllamaBaseURL() string {
	if c == nil || strings.TrimSpace(c.ollamaBaseURL) == "" {
		return DefaultOllamaBaseURL
	}
	return c.ollamaBaseURL
}

// OllamaModel returns the configured Ollama model name.
func (c *AiConfig) OllamaModel() string {
	if c == nil || strings.TrimSpace(c.ollamaModel) == "" {
		return DefaultOllamaModel
	}
	return c.ollamaModel
}

// IsConfigured reports whether the selected AI provider has everything it needs to run.
func (c *AiConfig) IsConfigured() bool {
	if c.Provider() == ProviderOllama {
		return true
	}
	_, ok := c.GeminiAPIKey()
	return ok
}

// NotConfiguredMessage returns the user-facing message when IsConfigured() is false.
func (c *AiConfig) NotConfiguredMessage() string {
	return "Error: GEMINI_API_KEY environment variable is not set. Cannot use LangChain4j analysis."
}

// ApiConfig holds bearer token authentication settings for the read/compute API.
type ApiConfig struct {
	apiToken    string
	requireAuth bool
}

// LoadApiConfig constructs an ApiConfig from environment variables and .env.
func LoadApiConfig() *ApiConfig {
	return NewApiConfig(Env("API_TOKEN"), ParseFlag(Env("API_REQUIRE_AUTH")))
}

// NewApiConfig constructs an ApiConfig with explicit values.
func NewApiConfig(apiToken string, requireAuth bool) *ApiConfig {
	return &ApiConfig{
		apiToken:    apiToken,
		requireAuth: requireAuth,
	}
}

// Token returns the trimmed API token and true if non-blank.
func (c *ApiConfig) Token() (string, bool) {
	if c == nil {
		return "", false
	}
	t := strings.TrimSpace(c.apiToken)
	if t == "" {
		return "", false
	}
	return t, true
}

// RequireAuth reports whether authentication is required even if no token is set.
func (c *ApiConfig) RequireAuth() bool {
	return c != nil && c.requireAuth
}

// IngestConfig holds bearer token authentication settings for /api/ingest/**.
type IngestConfig struct {
	ingestToken string
	requireAuth bool
}

// LoadIngestConfig constructs an IngestConfig from environment variables and .env.
func LoadIngestConfig() *IngestConfig {
	return NewIngestConfig(Env("INGEST_TOKEN"), ParseFlag(Env("INGEST_REQUIRE_AUTH")))
}

// NewIngestConfig constructs an IngestConfig with explicit values.
func NewIngestConfig(ingestToken string, requireAuth bool) *IngestConfig {
	return &IngestConfig{
		ingestToken: ingestToken,
		requireAuth: requireAuth,
	}
}

// Token returns the trimmed ingest token and true if non-blank.
func (c *IngestConfig) Token() (string, bool) {
	if c == nil {
		return "", false
	}
	t := strings.TrimSpace(c.ingestToken)
	if t == "" {
		return "", false
	}
	return t, true
}

// RequireAuth reports whether ingest authentication is required even if no token is set.
func (c *IngestConfig) RequireAuth() bool {
	return c != nil && c.requireAuth
}

// MatchesBearerToken reports whether header is an "Authorization: Bearer <token>" header
// whose token matches expected in constant time.
func MatchesBearerToken(expected, header string) bool {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	presented := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

// ApiPosture represents the 4 states of the read/compute API guard.
type ApiPosture string

const (
	ApiPostureAuthenticated ApiPosture = "AUTHENTICATED"
	ApiPostureLoopbackOnly  ApiPosture = "LOOPBACK_ONLY"
	ApiPostureExposed       ApiPosture = "EXPOSED"
	ApiPostureMisconfigured ApiPosture = "MISCONFIGURED"
)

// ClassifyApiPosture classifies the read/compute API security posture.
func ClassifyApiPosture(cfg *ApiConfig, host string) ApiPosture {
	if _, ok := cfg.Token(); ok {
		return ApiPostureAuthenticated
	}
	if cfg.RequireAuth() {
		return ApiPostureMisconfigured
	}
	if IsLoopback(host) {
		return ApiPostureLoopbackOnly
	}
	return ApiPostureExposed
}

// IsLoopback reports whether a configured bind host restricts connections to the local machine.
func IsLoopback(host string) bool {
	trimmed := strings.TrimSpace(host)
	return trimmed == "127.0.0.1" ||
		trimmed == "localhost" ||
		trimmed == "::1" ||
		trimmed == "[::1]"
}

// DescribeBind returns a human-readable description of the bind host for startup logs.
func DescribeBind(host string) string {
	if strings.TrimSpace(host) == "" {
		return "every interface (no micronaut.server.host set)"
	}
	if IsLoopback(host) {
		return host + " (this machine only)"
	}
	return host + " (reachable off this machine)"
}

// LogApiSecurityAdvisory logs the startup advisory for the read/compute API.
func LogApiSecurityAdvisory(cfg *ApiConfig, host string) {
	where := DescribeBind(host)
	switch ClassifyApiPosture(cfg, host) {
	case ApiPostureAuthenticated:
		log.Printf("INFO  ApiSecurityAdvisory - The read/compute API requires a bearer token (API_TOKEN); listening on %s.", where)
	case ApiPostureLoopbackOnly:
		log.Printf("INFO  ApiSecurityAdvisory - The read/compute API is unauthenticated but listening on %s, so only this machine can reach it. Set API_TOKEN before widening MICRONAUT_SERVER_HOST.", where)
	case ApiPostureExposed:
		log.Printf("WARN  ApiSecurityAdvisory - The read/compute API is UNAUTHENTICATED and listening on %s — anyone who can reach this port can read every stored trajectory and spend this server's AI credits. Set API_TOKEN (and see the deployment-security notes in the README). In a container this is the bind inside the container; what the host publishes decides who can actually reach it.", where)
	case ApiPostureMisconfigured:
		log.Printf("ERROR ApiSecurityAdvisory - API_REQUIRE_AUTH is set but API_TOKEN is empty — the whole read/compute API will be refused until a token is configured. Set API_TOKEN.")
	}
}

// IngestPosture represents the 3 states of the ingest guard.
type IngestPosture string

const (
	IngestPostureAuthenticated IngestPosture = "AUTHENTICATED"
	IngestPostureOpen          IngestPosture = "OPEN"
	IngestPostureMisconfigured IngestPosture = "MISCONFIGURED"
)

// ClassifyIngestPosture classifies the ingest endpoints' security posture.
func ClassifyIngestPosture(cfg *IngestConfig) IngestPosture {
	if _, ok := cfg.Token(); ok {
		return IngestPostureAuthenticated
	}
	if cfg.RequireAuth() {
		return IngestPostureMisconfigured
	}
	return IngestPostureOpen
}

// LogIngestSecurityAdvisory logs the startup advisory for the ingest endpoints.
func LogIngestSecurityAdvisory(cfg *IngestConfig) {
	switch ClassifyIngestPosture(cfg) {
	case IngestPostureAuthenticated:
		log.Printf("INFO  IngestSecurityAdvisory - Ingest endpoints require a bearer token (INGEST_TOKEN).")
	case IngestPostureOpen:
		log.Printf("WARN  IngestSecurityAdvisory - Ingest endpoints are UNAUTHENTICATED — anyone who can reach this port can write trajectories. That's fine on localhost; set INGEST_TOKEN before exposing the server off this machine (and see the deployment-security notes in the README).")
	case IngestPostureMisconfigured:
		log.Printf("ERROR IngestSecurityAdvisory - INGEST_REQUIRE_AUTH is set but INGEST_TOKEN is empty — all ingest will be refused until a token is configured. Set INGEST_TOKEN.")
	}
}

const (
	logSafeMaxLength        = 512
	logSafeTruncationMarker = "...[truncated]"
)

// EscapeLog renders an untrusted string as a single printable line for log messages,
// matching Java's LogSafe.escape (512 UTF-16 code-unit cap, never splitting surrogate pairs,
// escaping \n, \r, \t, ISO controls, and U+2028/U+2029 as uppercase \uXXXX).
func EscapeLog(value string) string {
	u16 := utf16.Encode([]rune(value))
	end := len(u16)
	truncated := end > logSafeMaxLength
	if truncated {
		end = logSafeMaxLength
		if end > 0 && isHighSurrogate(u16[end-1]) {
			end--
		}
	}
	var sb strings.Builder
	sb.Grow(end + len(logSafeTruncationMarker))
	for i := 0; i < end; i++ {
		c := u16[i]
		if isHighSurrogate(c) && i+1 < end && isLowSurrogate(u16[i+1]) {
			r := utf16.DecodeRune(rune(c), rune(u16[i+1]))
			sb.WriteRune(r)
			i++
			continue
		}
		switch c {
		case '\n':
			sb.WriteString("\\n")
		case '\r':
			sb.WriteString("\\r")
		case '\t':
			sb.WriteString("\\t")
		default:
			if isISOControl(c) || c == 0x2028 || c == 0x2029 {
				fmt.Fprintf(&sb, "\\u%04X", c)
			} else {
				sb.WriteRune(rune(c))
			}
		}
	}
	if truncated {
		sb.WriteString(logSafeTruncationMarker)
	}
	return sb.String()
}

func isHighSurrogate(u uint16) bool {
	return u >= 0xD800 && u <= 0xDBFF
}

func isLowSurrogate(u uint16) bool {
	return u >= 0xDC00 && u <= 0xDFFF
}

func isISOControl(u uint16) bool {
	return u <= 0x001F || (u >= 0x007F && u <= 0x009F)
}

// ServerConfig holds HTTP server and database connection settings.
type ServerConfig struct {
	Host             string
	Port             int
	MaxRequestSize   int64
	DatabaseURL      string
	PostgresUser     string
	PostgresPassword string
}

const (
	DefaultServerHost       = "127.0.0.1"
	DefaultServerPort       = 8080
	DefaultMaxRequestSize   = 134217728 // 128 MiB
	DefaultDatabaseURL      = "jdbc:postgresql://localhost:55433/agentbrainviz"
	DefaultPostgresUser     = "agentviz"
	DefaultPostgresPassword = "agentviz"
)

// LoadServerConfig loads HTTP server and Postgres connection settings from env and .env.
func LoadServerConfig() *ServerConfig {
	host := Env("SERVER_HOST")
	if strings.TrimSpace(host) == "" {
		host = Env("MICRONAUT_SERVER_HOST")
	}
	if strings.TrimSpace(host) == "" {
		host = DefaultServerHost
	}

	port := DefaultServerPort
	portStr := Env("SERVER_PORT")
	if strings.TrimSpace(portStr) == "" {
		portStr = Env("MICRONAUT_SERVER_PORT")
	}
	if strings.TrimSpace(portStr) != "" {
		if p, err := strconv.Atoi(strings.TrimSpace(portStr)); err == nil && p >= 0 {
			port = p
		}
	}

	return &ServerConfig{
		Host:             host,
		Port:             port,
		MaxRequestSize:   DefaultMaxRequestSize,
		DatabaseURL:      EnvOr("DATABASE_URL", DefaultDatabaseURL),
		PostgresUser:     EnvOr("POSTGRES_USER", DefaultPostgresUser),
		PostgresPassword: EnvOr("POSTGRES_PASSWORD", DefaultPostgresPassword),
	}
}
