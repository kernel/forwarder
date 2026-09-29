// Copyright 2025 Sauce Labs Inc., all rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package martian

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func newTestProxyConn(t *testing.T, data []byte, closeAfter bool) *proxyConn {
	t.Helper()
	client, srv := net.Pipe()
	t.Cleanup(func() { client.Close(); srv.Close() })
	go func() {
		_, _ = client.Write(data)
		_, _ = client.Write([]byte("NEXT REQUEST\r\n")) // follow-up data after replay
		if closeAfter {
			_ = client.Close()
		}
	}()
	return &proxyConn{
		Proxy: &Proxy{BaseContext: context.Background()},
		brw:   bufio.NewReadWriter(bufio.NewReader(srv), bufio.NewWriter(srv)),
		conn:  srv,
	}
}

func readRequestOrFatal(t *testing.T, pc *proxyConn) *http.Request {
	t.Helper()
	pc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	req, err := pc.readRequest()
	if err != nil {
		t.Fatalf("readRequest: %v", err)
	}
	return req
}

func TestReadRequestCapturesHeaderOrder(t *testing.T) {
	raw := "GET http://example.com/ HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Cache-Control: max-age=0\r\n" +
		"sec-ch-ua: \"Chromium\"\r\n" +
		"User-Agent: Test/1.0\r\n" +
		"cookie: a=1\r\n" +
		"cookie: b=2\r\n" +
		"\r\n"
	pc := newTestProxyConn(t, []byte(raw), false)
	req := readRequestOrFatal(t, pc)

	if got := ContextHeaderOrder(req.Context()); !equalStrings(got, []string{
		"host", "cache-control", "sec-ch-ua", "user-agent", "cookie", "cookie",
	}) {
		t.Errorf("header order = %v, want [host cache-control sec-ch-ua user-agent cookie cookie]", got)
	}
	if req.Host != "example.com" || req.Header.Get("User-Agent") != "Test/1.0" || len(req.Header.Values("cookie")) != 2 {
		t.Errorf("request not parsed correctly: %+v", req)
	}
	// The follow-up bytes written after the head must survive the replay: the
	// buffered reader must be positioned right after the request head.
	line, err := pc.brw.Reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read follow-up: %v", err)
	}
	if strings.TrimSpace(line) != "NEXT REQUEST" {
		t.Errorf("follow-up line = %q, want %q", line, "NEXT REQUEST")
	}
}

func TestReadRequestHeaderOrderWithBody(t *testing.T) {
	raw := "POST http://example.com/ HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Content-Length: 5\r\n" +
		"X-First: 1\r\n" +
		"X-Second: 2\r\n" +
		"\r\n" +
		"hello"
	pc := newTestProxyConn(t, []byte(raw), false)
	req := readRequestOrFatal(t, pc)

	if got := ContextHeaderOrder(req.Context()); !equalStrings(got, []string{"host", "content-length", "x-first", "x-second"}) {
		t.Errorf("header order = %v, want [host content-length x-first x-second]", got)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
}

func TestReadRequestHeaderOrderLongLine(t *testing.T) {
	// A header line longer than the bufio buffer exercises the ErrBufferFull
	// fragment path: the name must still be captured in one piece.
	long := strings.Repeat("a", 16<<10)
	raw := "GET http://example.com/ HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Cookie: " + long + "\r\n" +
		"X-After: 1\r\n" +
		"\r\n"
	pc := newTestProxyConn(t, []byte(raw), false)
	req := readRequestOrFatal(t, pc)

	if got := ContextHeaderOrder(req.Context()); !equalStrings(got, []string{"host", "cookie", "x-after"}) {
		t.Errorf("header order = %v, want [host cookie x-after]", got)
	}
	if got := req.Header.Get("Cookie"); got != long {
		t.Errorf("cookie value not parsed intact (len=%d, want %d)", len(got), len(long))
	}
}

func TestReadRequestHeaderOrderMalformedHead(t *testing.T) {
	// Truncated head: order capture gives up, the parse must fail as usual.
	raw := "GET / HTTP/1.1\r\nHost: x\r\n"
	pc := newTestProxyConn(t, []byte(raw), true)
	pc.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := pc.readRequest(); err == nil {
		t.Fatal("expected parse error for truncated head")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
