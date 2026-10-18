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

// Command agy-brain-viz runs the Agent Brain Visualizer web server and API.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/scottdensmore/agent-brain-visualizer/internal/config"
	"github.com/scottdensmore/agent-brain-visualizer/internal/pgdriver"
	"github.com/scottdensmore/agent-brain-visualizer/internal/server"
	"github.com/scottdensmore/agent-brain-visualizer/internal/store"
)

// version can be overridden at link time via -ldflags="-X main.version=...".
var version = ""

func effectiveVersion() string {
	if strings.TrimSpace(version) != "" {
		return version
	}
	return server.DefaultVersion
}

type cliOptions struct {
	showHelp      bool
	showVersion   bool
	hostOverride  string
	portOverride  *int
	dotenvEnabled *bool
	dotenvPath    string
	userHome      string
}

func parseArgs(args []string) (cliOptions, error) {
	var opts cliOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			opts.showHelp = true
		case arg == "-v" || arg == "--version":
			opts.showVersion = true
		case arg == "-port" || arg == "--port":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing value for %s", arg)
			}
			i++
			p, err := strconv.Atoi(args[i])
			if err != nil {
				return opts, fmt.Errorf("invalid port %q: %w", args[i], err)
			}
			opts.portOverride = &p
		case strings.HasPrefix(arg, "--port="), strings.HasPrefix(arg, "-port="):
			val := arg[strings.IndexByte(arg, '=')+1:]
			p, err := strconv.Atoi(val)
			if err != nil {
				return opts, fmt.Errorf("invalid port %q: %w", val, err)
			}
			opts.portOverride = &p
		case arg == "-host" || arg == "--host":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("missing value for %s", arg)
			}
			i++
			opts.hostOverride = args[i]
		case strings.HasPrefix(arg, "--host="), strings.HasPrefix(arg, "-host="):
			opts.hostOverride = arg[strings.IndexByte(arg, '=')+1:]
		case strings.HasPrefix(arg, "-D"):
			kv := arg[2:]
			eq := strings.IndexByte(kv, '=')
			if eq <= 0 {
				return opts, fmt.Errorf("invalid -D property %q (expected -Dkey=value)", arg)
			}
			key := kv[:eq]
			val := kv[eq+1:]
			switch key {
			case "micronaut.server.port", "server.port":
				p, err := strconv.Atoi(val)
				if err != nil {
					return opts, fmt.Errorf("invalid port %q: %w", val, err)
				}
				opts.portOverride = &p
			case "micronaut.server.host", "server.host":
				opts.hostOverride = val
			case "dotenv.enabled":
				b := config.ParseFlag(val)
				opts.dotenvEnabled = &b
			case "dotenv.path":
				opts.dotenvPath = val
			case "user.home":
				opts.userHome = val
			}
		default:
			return opts, fmt.Errorf("unknown argument: %s", arg)
		}
	}
	return opts, nil
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage: agy-brain-viz [options]

Agent Brain Visualizer HTTP server and UI.

Options:
  -h, --help                Show this help message and exit
  -v, --version             Print build version and exit
  --host <host>             Bind address (default: MICRONAUT_SERVER_HOST or 127.0.0.1)
  --port <port>             Bind port (default: MICRONAUT_SERVER_PORT or 8080; 0 for random)
  -Ddotenv.enabled=<bool>   Enable or disable loading .env from the working directory
  -Ddotenv.path=<path>      Path to .env file (default: .env)
`)
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	startTime := time.Now()
	opts, err := parseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		printUsage(stderr)
		return 2
	}
	if opts.showHelp {
		printUsage(stdout)
		return 0
	}
	if opts.showVersion {
		fmt.Fprintf(stdout, "agy-brain-viz %s\n", effectiveVersion())
		return 0
	}

	if opts.dotenvEnabled != nil {
		config.SetDotEnvEnabled(*opts.dotenvEnabled)
	}
	if opts.dotenvPath != "" {
		config.SetDotEnvPath(opts.dotenvPath)
	}

	srvCfg := config.LoadServerConfig()
	if opts.hostOverride != "" {
		srvCfg.Host = opts.hostOverride
	}
	if opts.portOverride != nil {
		srvCfg.Port = *opts.portOverride
	}

	apiCfg := config.LoadApiConfig()
	ingestCfg := config.LoadIngestConfig()
	aiCfg := config.LoadAiConfig()

	config.LogApiSecurityAdvisory(apiCfg, srvCfg.Host)
	config.LogIngestSecurityAdvisory(ingestCfg)

	db, err := pgdriver.Open(srvCfg.DatabaseURL, srvCfg.PostgresUser, srvCfg.PostgresPassword)
	if err != nil {
		log.Printf("WARN  Application - Could not configure database connection (%s); store endpoints will report 503", config.EscapeLog(err.Error()))
	} else {
		defer db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		store.BootstrapSchema(ctx, db)
		cancel()
	}

	handler := server.New(server.Dependencies{
		Version:      effectiveVersion(),
		UserHome:     opts.userHome,
		DB:           db,
		ApiConfig:    apiCfg,
		IngestConfig: ingestCfg,
		AiConfig:     aiCfg,
	})

	listenAddr := net.JoinHostPort(srvCfg.Host, strconv.Itoa(srvCfg.Port))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		fmt.Fprintf(stderr, "Error listening on %s: %v\n", listenAddr, err)
		return 1
	}

	actualPort := srvCfg.Port
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		actualPort = tcpAddr.Port
	}
	displayHost := srvCfg.Host
	if displayHost == "" || displayHost == "0.0.0.0" || displayHost == "::" {
		displayHost = "localhost"
	}

	httpServer := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	elapsedMs := time.Since(startTime).Milliseconds()
	log.Printf("INFO  Application - Startup completed in %dms. Server Running: http://%s:%d", elapsedMs, displayHost, actualPort)

	if err := httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "Server error: %v\n", err)
		return 1
	}
	return 0
}
