// Copyright 2025 Sauce Labs Inc., all rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package martian

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsClosedConnErrorHTTP2ClosedBody reads the request body after the handler
// closed it, so the HTTP/2 server returns its real errClosedBody. If the server
// rewords that error, isClosedConnError stops matching and this test fails.
func TestIsClosedConnErrorHTTP2ClosedBody(t *testing.T) {
	errCh := make(chan error, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		r.Body.Close()
		_, err := r.Body.Read(make([]byte, 1))
		errCh <- err
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	res, err := srv.Client().Post(srv.URL, "text/plain", strings.NewReader("payload"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.ProtoMajor != 2 {
		t.Fatalf("got %s, want HTTP/2", res.Proto)
	}

	readErr := <-errCh
	if readErr == nil {
		t.Fatal("reading a closed request body succeeded")
	}
	if !isClosedConnError(readErr) {
		t.Errorf("isClosedConnError(%q) = false, want true", readErr)
	}
	if wrapped := fmt.Errorf("copy request body: %w", readErr); !isClosedConnError(wrapped) {
		t.Errorf("isClosedConnError(%q) = false, want true", wrapped)
	}
}
