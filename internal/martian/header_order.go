// Copyright 2022-2026 Sauce Labs Inc., all rights reserved.

package martian

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
)

// maxRecordedHeadBytes caps how much of a request head the recorder buffers
// before giving up. A client that never terminates its header block must not be
// able to grow this buffer without bound; giving up costs only the order.
const maxRecordedHeadBytes = 1 << 20

var headTerminator = []byte("\r\n\r\n")

// headerOrderRecorder recovers the order in which a client sent its request
// headers. net/http parses headers into a map, which has no order, but the
// order is a client fingerprinting signal, so a proxy that re-originates the
// request needs it to reproduce the client faithfully on the outbound leg.
//
// The recorder sits between the connection and the bufio.Reader that
// http.ReadRequest consumes, and only ever observes bytes that the parser was
// going to read anyway. It performs no reads of its own, so it cannot block,
// short-read, or otherwise change how the request is parsed: the worst it can
// do is fail to record an order.
type headerOrderRecorder struct {
	r io.Reader

	recording bool
	head      bytes.Buffer
	// line is the request line of the recorded head, used to confirm the
	// recording belongs to the request the parser produced.
	line  string
	order []string
}

func newHeaderOrderRecorder(r io.Reader) *headerOrderRecorder {
	return &headerOrderRecorder{r: r}
}

// reset points the recorder at a new underlying reader, discarding any
// in-progress recording. Used when the connection is replaced, as it is when a
// CONNECT is bumped to TLS.
func (rec *headerOrderRecorder) reset(r io.Reader) {
	rec.r = r
	rec.recording = false
	rec.head.Reset()
	rec.line = ""
	rec.order = nil
}

// arm starts recording the next request head. It must be called before the
// parser reads any of that head, including the one-byte readability probe.
func (rec *headerOrderRecorder) arm() {
	rec.recording = true
	rec.head.Reset()
	rec.line = ""
	rec.order = nil
}

func (rec *headerOrderRecorder) Read(p []byte) (int, error) {
	n, err := rec.r.Read(p)
	if rec.recording && n > 0 {
		rec.capture(p[:n])
	}
	return n, err
}

// capture accumulates bytes until the header block ends, then parses the order
// once and stops recording. Bytes past the terminator belong to the body and
// are ignored.
func (rec *headerOrderRecorder) capture(b []byte) {
	if rec.head.Len()+len(b) > maxRecordedHeadBytes {
		rec.recording = false
		rec.head.Reset()
		return
	}
	rec.head.Write(b)

	i := bytes.Index(rec.head.Bytes(), headTerminator)
	if i < 0 {
		return
	}
	rec.line, rec.order = parseHeaderOrder(rec.head.Bytes()[:i])
	rec.recording = false
	rec.head.Reset()
}

// parseHeaderOrder splits a request head into its request line and its header
// names, in the order sent. Names are lowercased because HTTP/1.1 field names
// are case-insensitive and every consumer of the order matches them that way.
func parseHeaderOrder(head []byte) (line string, order []string) {
	lines := strings.Split(string(head), "\r\n")
	if len(lines) == 0 {
		return "", nil
	}
	order = make([]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		// An obs-fold continuation belongs to the previous header, not a new one.
		if l == "" || l[0] == ' ' || l[0] == '\t' {
			continue
		}
		c := strings.IndexByte(l, ':')
		if c <= 0 {
			continue
		}
		order = append(order, strings.ToLower(strings.TrimSpace(l[:c])))
	}
	return lines[0], order
}

// recorded returns the order for req, or nil if nothing was recorded or the
// recording does not belong to req.
//
// The request line is the check: it pins the recording to one specific request,
// so if the tap ever loses sync with the parser -- which would take a pipelining
// client, since otherwise a request head cannot reach the reader before the
// previous response is written -- the order is dropped rather than applied to
// the wrong request.
func (rec *headerOrderRecorder) recorded(req *http.Request) []string {
	if len(rec.order) == 0 {
		return nil
	}
	if rec.line != req.Method+" "+req.RequestURI+" "+req.Proto {
		return nil
	}
	return rec.order
}

type headerOrderContextKey struct{}

// WithHeaderOrder returns ctx carrying order as the client's header order. The
// proxy calls this for every request it parses; it is exported so that code
// downstream of the proxy can be tested without standing one up.
func WithHeaderOrder(ctx context.Context, order []string) context.Context {
	return context.WithValue(ctx, headerOrderContextKey{}, order)
}

// HeaderOrderFromContext returns the order in which the client sent the headers
// of the request carrying ctx, lowercased. It returns nil when the order was not
// recoverable, in which case callers should fall back to their own ordering.
//
// The names are what the client sent, so they include HTTP/1.1-only headers such
// as Host and Connection, and exclude anything a modifier added afterwards.
func HeaderOrderFromContext(ctx context.Context) []string {
	order, _ := ctx.Value(headerOrderContextKey{}).([]string)
	return order
}
