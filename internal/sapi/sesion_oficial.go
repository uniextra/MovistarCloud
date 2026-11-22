package sapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"movistar-cloud-uploader/internal/auth"
	"movistar-cloud-uploader/internal/config"
	"movistar-cloud-uploader/internal/logger"
)

// Un login fallido en SAPI se trata inicialmente como transitorio y se reintenta
// con espera exponencial; solo se da la sesión por caducada si sigue fallando durante
// al menos LoginGiveUpAfter (30 min) y al menos LoginGiveUpRejections veces (4).
// Caídas de red, 5xx, 429, o el 403 del cortafuegos WAF nunca dan la sesión por caducada.

func desktopLoginWait(n int) time.Duration {
	wait, max := config.LoginRetryFirstWait(), config.LoginRetryMaxWait()
	for i := 1; i < n && wait < max; i++ {
		wait *= 2
	}
	if wait > max {
		wait = max
	}
	return wait
}

func (c *Client) desktopLoginFailed(res *auth.SAPILoginResult) bool {
	now := time.Now()
	definitive := res != nil && isCredentialRejection(res)

	c.mu.Lock()
	if c.loginFailures == 0 {
		c.loginFailingSince = now
	}
	c.loginFailures++
	n := c.loginFailures
	since := c.loginFailingSince
	wait := desktopLoginWait(n)
	giveUp := definitive && n >= config.LoginGiveUpRejections() && now.Sub(since) >= config.LoginGiveUpAfter()
	if !giveUp {
		c.loginRetryAt = now.Add(wait)
		c.refreshTransient = true
	}
	c.mu.Unlock()

	if giveUp {
		logger.Log("[OAuth] ⚠️ Movistar / O2 rechaza el login de forma definitiva desde hace %s (%d intentos).", now.Sub(since).Round(time.Minute), n)
		return false
	}
	logger.Log("[OAuth] ⏳ Login en SAPI fallido (%s); se conserva la sesión y se reintenta en %s (fallo %d).", describeLoginFailure(res), wait, n)
	return true
}

func isCredentialRejection(res *auth.SAPILoginResult) bool {
	if res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusForbidden {
		return false
	}
	edge := strings.Contains(strings.ToLower(res.RawResponse), "cloudfront")
	if res.StatusCode == http.StatusForbidden && edge {
		return false // cortafuegos WAF
	}
	if edge && strings.TrimSpace(res.RawResponse) == "" {
		return false // 401 vacío del borde
	}
	return true
}

func describeLoginFailure(res *auth.SAPILoginResult) string {
	if res == nil {
		return "sin respuesta"
	}
	if res.StatusCode == 0 {
		if res.Error != nil {
			return "sin respuesta: " + res.Error.Error()
		}
		return "sin respuesta"
	}
	return fmt.Sprintf("HTTP %d", res.StatusCode)
}
