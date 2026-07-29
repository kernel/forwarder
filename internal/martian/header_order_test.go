// Copyright 2022-2026 Sauce Labs Inc., all rights reserved.

package martian

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/saucelabs/forwarder/internal/martian/martiantest"
	"github.com/saucelabs/forwarder/internal/martian/proxyutil"
)

type drainingTransport struct{}

func (drainingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		io.Copy(io.Discard, req.Body) //nolint:errcheck // draining so the connection stays reusable
		req.Body.Close()
	}
	return proxyutil.NewResponse(200, http.NoBody, req), nil
}

// TestHeaderOrderPreservedThroughMITM is the reason this recorder exists: a
// request bumped to TLS and parsed by net/http must still expose the order in
// which the client sent its headers. The orders used here are real Chrome 150
// shapes -- an XHR POST and a document navigation -- which differ from each
// other, so a proxy cannot substitute one fixed order for both.
func TestHeaderOrderPreservedThroughMITM(t *testing.T) {
	t.Parallel()

	// Deliberately not alphabetical and not Go's write order: the point is that
	// the order survives verbatim rather than being re-derived.
	const xhrPost = "POST /sensor HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Content-Length: 5\r\n" +
		"Sec-Ch-Ua-Platform: \"Linux\"\r\n" +
		"User-Agent: test-agent\r\n" +
		"Sec-Ch-Ua: \"Chromium\"\r\n" +
		"Content-Type: text/plain;charset=UTF-8\r\n" +
		"Sec-Ch-Ua-Mobile: ?0\r\n" +
		"Accept: */*\r\n" +
		"Cookie: a=b\r\n" +
		"\r\n" +
		"hello"

	const navigation = "GET /page HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Sec-Ch-Ua: \"Chromium\"\r\n" +
		"Sec-Ch-Ua-Mobile: ?0\r\n" +
		"Sec-Ch-Ua-Platform: \"Linux\"\r\n" +
		"Upgrade-Insecure-Requests: 1\r\n" +
		"User-Agent: test-agent\r\n" +
		"Accept: text/html\r\n" +
		"\r\n"

	wantXHR := []string{
		"host", "content-length", "sec-ch-ua-platform", "user-agent", "sec-ch-ua",
		"content-type", "sec-ch-ua-mobile", "accept", "cookie",
	}
	wantNav := []string{
		"host", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"upgrade-insecure-requests", "user-agent", "accept",
	}

	var (
		mu   sync.Mutex
		seen = map[string][]string{}
	)
	tm := martiantest.NewModifier()
	tm.RequestFunc(func(req *http.Request) {
		if req.Method == http.MethodConnect {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seen[req.URL.Path] = HeaderOrderFromContext(req.Context())
	})

	ca, mc := certs(t)

	h := testHelper{
		Proxy: func(p *Proxy) {
			p.RoundTripper = drainingTransport{}
			p.MITMConfig = mc
			p.RequestModifier = tm
		},
	}

	c, cancel := h.proxyClient(t)
	t.Cleanup(cancel)

	conn := c.dial(t)
	defer conn.Close()

	req, err := http.NewRequest(http.MethodConnect, "//example.com:443", http.NoBody)
	if err != nil {
		t.Fatalf("http.NewRequest(): got %v, want no error", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("req.Write(): got %v, want no error", err)
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("http.ReadResponse(): got %v, want no error", err)
	}
	res.Body.Close()
	if got, want := res.StatusCode, 200; got != want {
		t.Fatalf("CONNECT res.StatusCode: got %d, want %d", got, want)
	}

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	tlsconn := tls.Client(conn, &tls.Config{ServerName: "example.com", RootCAs: roots})
	defer tlsconn.Close()

	// Both requests go down one bumped connection, so this also covers arming
	// per request: the second must record its own order, not reuse the first.
	br := bufio.NewReader(tlsconn)
	for _, raw := range []string{xhrPost, navigation} {
		if _, err := io.WriteString(tlsconn, raw); err != nil {
			t.Fatalf("write raw request: got %v, want no error", err)
		}
		res, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("http.ReadResponse(): got %v, want no error", err)
		}
		io.Copy(io.Discard, res.Body) //nolint:errcheck // draining
		res.Body.Close()
		if got, want := res.StatusCode, 200; got != want {
			t.Fatalf("res.StatusCode: got %d, want %d", got, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for path, want := range map[string][]string{"/sensor": wantXHR, "/page": wantNav} {
		got := seen[path]
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("header order for %s:\n got %v\nwant %v", path, got, want)
		}
	}
}

func TestParseHeaderOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		head      string
		wantLine  string
		wantOrder []string
	}{
		{
			name:      "lowercases names and keeps order",
			head:      "GET / HTTP/1.1\r\nHost: a\r\nX-Mixed-Case: b",
			wantLine:  "GET / HTTP/1.1",
			wantOrder: []string{"host", "x-mixed-case"},
		},
		{
			name:      "obs-fold continuation is not a new header",
			head:      "GET / HTTP/1.1\r\nHost: a\r\nX-Long: b\r\n\tstill-b\r\nX-Next: c",
			wantLine:  "GET / HTTP/1.1",
			wantOrder: []string{"host", "x-long", "x-next"},
		},
		{
			name:      "line without a colon is skipped",
			head:      "GET / HTTP/1.1\r\nHost: a\r\ngarbage\r\nX-Next: c",
			wantLine:  "GET / HTTP/1.1",
			wantOrder: []string{"host", "x-next"},
		},
		{
			name:      "repeated header is recorded at each position",
			head:      "GET / HTTP/1.1\r\nCookie: a\r\nHost: h\r\nCookie: b",
			wantLine:  "GET / HTTP/1.1",
			wantOrder: []string{"cookie", "host", "cookie"},
		},
		{
			name:      "request line only",
			head:      "GET / HTTP/1.1",
			wantLine:  "GET / HTTP/1.1",
			wantOrder: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			line, order := parseHeaderOrder([]byte(tt.head))
			if line != tt.wantLine {
				t.Errorf("line: got %q, want %q", line, tt.wantLine)
			}
			if strings.Join(order, " ") != strings.Join(tt.wantOrder, " ") {
				t.Errorf("order: got %v, want %v", order, tt.wantOrder)
			}
		})
	}
}

// TestHeaderOrderRecorderRejectsMismatchedRequest covers the safety property:
// if the tap ever loses sync with the parser, the order must be dropped rather
// than attached to a request it did not come from.
func TestHeaderOrderRecorderRejectsMismatchedRequest(t *testing.T) {
	t.Parallel()

	const head = "POST /sensor HTTP/1.1\r\nHost: example.com\r\nAccept: */*\r\n\r\n"

	rec := newHeaderOrderRecorder(strings.NewReader(head))
	rec.arm()
	if _, err := io.Copy(io.Discard, rec); err != nil {
		t.Fatalf("io.Copy(): got %v, want no error", err)
	}

	match := &http.Request{Method: "POST", RequestURI: "/sensor", Proto: "HTTP/1.1"}
	if got := rec.recorded(match); strings.Join(got, " ") != "host accept" {
		t.Errorf("recorded(matching): got %v, want [host accept]", got)
	}

	for _, mismatch := range []*http.Request{
		{Method: "GET", RequestURI: "/sensor", Proto: "HTTP/1.1"},
		{Method: "POST", RequestURI: "/other", Proto: "HTTP/1.1"},
		{Method: "POST", RequestURI: "/sensor", Proto: "HTTP/1.0"},
	} {
		if got := rec.recorded(mismatch); got != nil {
			t.Errorf("recorded(%s %s %s): got %v, want nil",
				mismatch.Method, mismatch.RequestURI, mismatch.Proto, got)
		}
	}
}

func TestHeaderOrderRecorderGivesUpOnOversizedHead(t *testing.T) {
	t.Parallel()

	// A head that never terminates must not grow the buffer without bound.
	head := "GET / HTTP/1.1\r\n" + strings.Repeat("X-Pad: 0123456789\r\n", (maxRecordedHeadBytes/19)+1)

	rec := newHeaderOrderRecorder(strings.NewReader(head))
	rec.arm()
	if _, err := io.Copy(io.Discard, rec); err != nil {
		t.Fatalf("io.Copy(): got %v, want no error", err)
	}

	if rec.head.Len() != 0 {
		t.Errorf("rec.head.Len(): got %d, want 0", rec.head.Len())
	}
	if got := rec.recorded(&http.Request{Method: "GET", RequestURI: "/", Proto: "HTTP/1.1"}); got != nil {
		t.Errorf("recorded(): got %v, want nil", got)
	}
}

// TestHeaderOrderRecorderIsPassive pins that the tap cannot change what the
// parser reads, which is what makes it safe to sit in front of every request.
func TestHeaderOrderRecorderIsPassive(t *testing.T) {
	t.Parallel()

	const body = "GET / HTTP/1.1\r\nHost: example.com\r\n\r\nbody-bytes"

	rec := newHeaderOrderRecorder(strings.NewReader(body))
	rec.arm()

	got, err := io.ReadAll(rec)
	if err != nil {
		t.Fatalf("io.ReadAll(): got %v, want no error", err)
	}
	if string(got) != body {
		t.Errorf("bytes through recorder: got %q, want %q", got, body)
	}
}
