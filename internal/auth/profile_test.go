package auth

import (
	"strings"
	"testing"
)

func TestProfileAndDeviceID(t *testing.T) {
	reqID := NewRequestID()
	if len(reqID) != 36 || strings.Count(reqID, "-") != 4 {
		t.Fatalf("unexpected request ID format: %s", reqID)
	}

	devID := generateDesktopDeviceID()
	if !strings.HasPrefix(devID, "fol-") {
		t.Fatalf("expected desktop device ID to start with 'fol-', got %s", devID)
	}

	sanitized := SanitizeDeviceID("web-1234567890abcdef")
	if !strings.HasPrefix(sanitized, "fol-") {
		t.Fatalf("expected sanitized desktop device ID to convert to 'fol-', got %s", sanitized)
	}

	stable := desktopDeviceIDFrom("web-1234567890abcdef")
	if stable != sanitized {
		t.Fatalf("expected stable device ID mapping, got %s != %s", stable, sanitized)
	}
}
