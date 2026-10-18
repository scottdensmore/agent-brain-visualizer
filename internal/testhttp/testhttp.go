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

// Package testhttp provides an httptest.Server constructor that transparently
// falls back to in-memory net.Pipe connections when the OS or sandbox blocks
// loopback TCP connect() syscalls, while using standard TCP sockets otherwise.
package testhttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"
)

var (
	listeners sync.Map // string (host:port) -> *pipeListener
	nextPort  atomic.Int32
	tcpOnce   sync.Once
	tcpWorks  bool
)

func init() {
	nextPort.Store(41000)
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		origDial := tr.DialContext
		if origDial == nil {
			var d net.Dialer
			origDial = d.DialContext
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if v, ok := listeners.Load(addr); ok {
				return v.(*pipeListener).dial(ctx)
			}
			return origDial(ctx, network, addr)
		}
	}
}

func canDialTCP() bool {
	tcpOnce.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return
		}
		defer ln.Close()
		conn, err := net.DialTimeout("tcp", ln.Addr().String(), 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		tcpWorks = true
	})
	return tcpWorks
}

// NewServer starts and returns a new httptest.Server. If the environment allows
// loopback TCP connections, it delegates directly to httptest.NewServer;
// otherwise it uses an in-memory net.Pipe listener wired into http.DefaultTransport.
func NewServer(handler http.Handler) *httptest.Server {
	if canDialTCP() {
		return httptest.NewServer(handler)
	}
	port := int(nextPort.Add(1))
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}
	pl := &pipeListener{
		addr:   addr,
		accept: make(chan net.Conn, 64),
		done:   make(chan struct{}),
	}
	listeners.Store(addr.String(), pl)
	srv := &httptest.Server{
		Listener: pl,
		Config:   &http.Server{Handler: handler},
	}
	srv.Start()
	if tr, ok := srv.Client().Transport.(*http.Transport); ok {
		tr.DialContext = http.DefaultTransport.(*http.Transport).DialContext
	}
	return srv
}

type pipeListener struct {
	addr   net.Addr
	accept chan net.Conn
	done   chan struct{}
	once   sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accept:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		listeners.Delete(l.addr.String())
		close(l.done)
	})
	return nil
}

func (l *pipeListener) Addr() net.Addr {
	return l.addr
}

func (l *pipeListener) dial(ctx context.Context) (net.Conn, error) {
	select {
	case <-l.done:
		return nil, errors.New("connection refused")
	default:
	}
	c1, c2 := net.Pipe()
	select {
	case l.accept <- c2:
		return c1, nil
	case <-l.done:
		_ = c1.Close()
		_ = c2.Close()
		return nil, errors.New("connection refused")
	case <-ctx.Done():
		_ = c1.Close()
		_ = c2.Close()
		return nil, ctx.Err()
	}
}
