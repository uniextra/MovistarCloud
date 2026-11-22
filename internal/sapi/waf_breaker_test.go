package sapi

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestIsWAFBlockResponse(t *testing.T) {
	// 1. CloudFront HTML 403 Block
	respWAF := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Server":       []string{"CloudFront"},
			"Content-Type": []string{"text/html"},
		},
		Body: io.NopCloser(bytes.NewReader([]byte("<html><title>ERROR: The request could not be satisfied</title></html>"))),
	}
	if !isWAFBlockResponse(respWAF) {
		t.Fatal("expected isWAFBlockResponse to return true for CloudFront block")
	}

	// Ensure body is readable after check
	b, err := io.ReadAll(respWAF.Body)
	if err != nil || !bytes.Contains(b, []byte("could not be satisfied")) {
		t.Fatalf("body was not reconstructed properly: %v", err)
	}

	// 2. Normal SAPI JSON 403 (e.g. invalid permissions)
	respSAPI := &http.Response{
		StatusCode: http.StatusForbidden,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader([]byte(`{"error":{"code":"MED-1003"}}`))),
	}
	if isWAFBlockResponse(respSAPI) {
		t.Fatal("expected isWAFBlockResponse to return false for SAPI JSON 403")
	}

	// 3. Normal 200 OK
	respOK := &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: io.NopCloser(bytes.NewReader([]byte(`{"data":{"status":"ok"}}`))),
	}
	if isWAFBlockResponse(respOK) {
		t.Fatal("expected isWAFBlockResponse to return false for 200 OK")
	}
}

func TestWAFBreakerTripping(t *testing.T) {
	now := time.Now()
	b := &wafBreaker{now: func() time.Time { return now }}

	if until := b.blockedUntil(); !until.IsZero() {
		t.Fatal("expected unblocked initially")
	}

	// First strike
	u1 := b.trip("test API")
	if u1 != now.Add(wafCooldownBase) {
		t.Fatalf("expected cooldown %v, got %v", now.Add(wafCooldownBase), u1)
	}
	if until := b.blockedUntil(); until != u1 {
		t.Fatalf("expected blocked until %v, got %v", u1, until)
	}

	// Second strike advances cooldown
	now = now.Add(wafCooldownBase + 1*time.Second)
	u2 := b.trip("test API")
	if u2 != now.Add(2*wafCooldownBase) {
		t.Fatalf("expected doubled cooldown %v, got %v", now.Add(2*wafCooldownBase), u2)
	}

	// Recovery
	b.ok("test API")
	if b.strikes != 0 {
		t.Fatalf("expected 0 strikes after ok(), got %d", b.strikes)
	}
}
