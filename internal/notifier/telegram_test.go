package notifier

import (
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestMaskSecret(t *testing.T) {
	if got := maskSecret("123456789"); got != "12****89" {
		t.Fatalf("expected 12****89, got %s", got)
	}
	if got := maskSecret("short"); got != "****" {
		t.Fatalf("expected ****, got %s", got)
	}
	if got := maskSecret("1234567890abcdef1234"); got != "1234…1234" {
		t.Fatalf("expected 1234…1234, got %s", got)
	}
}

func TestSanitizeTransportError(t *testing.T) {
	token := "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"
	err := &url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + token + "/sendMessage",
		Err: errors.New("connection reset by peer on " + token),
	}

	sanitized := sanitizeTransportError(err, token)
	if strings.Contains(sanitized, token) {
		t.Fatalf("sanitized error still contained raw token: %s", sanitized)
	}
}

func TestAlertDeduplication(t *testing.T) {
	tn := NewTelegramNotifier("test-token", "123456")

	// First alert passes
	if !tn.shouldSendAlert("alert1", 1*time.Hour) {
		t.Fatal("first alert should have passed")
	}

	// Immediate second alert is deduplicated
	if tn.shouldSendAlert("alert1", 1*time.Hour) {
		t.Fatal("second alert within cooldown should have been suppressed")
	}

	// Different key passes
	if !tn.shouldSendAlert("alert2", 1*time.Hour) {
		t.Fatal("different alert key should have passed")
	}
}
