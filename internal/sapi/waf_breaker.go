package sapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"movistar-cloud-uploader/internal/logger"
)

// Freno ante el bloqueo del cortafuegos (AWS WAF / CloudFront) de Movistar / O2 Cloud.
//
// Cuando CloudFront empieza a responder "403 ERROR - The request could not be satisfied",
// el bloqueo es por IP: cada petición adicional que se envía alarga la sanción del WAF.
// Contamos con frenos independientes por host:
//   - API (cloud.o2online.es / micloud.movistar.es)
//   - Subidas (upload.*)
//
// Al recibir un 403 de CloudFront, ese host se pausa durante wafCooldownBase (10m, duplicándose
// en cada bloqueo sucesivo hasta 2h) devolviendo ErrWAFBlocked sin golpear la red.

var ErrWAFBlocked = errors.New("Movistar / O2 Cloud: cortafuegos (WAF) bloqueando; llamadas en pausa")

var (
	wafCooldownBase = 10 * time.Minute
	wafCooldownMax  = 2 * time.Hour
)

type wafBreaker struct {
	mu      sync.Mutex
	until   time.Time
	strikes int
	now     func() time.Time
}

func (b *wafBreaker) clock() time.Time {
	if b.now != nil {
		return b.now()
	}
	return time.Now()
}

func (b *wafBreaker) blockedUntil() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.clock().Before(b.until) {
		return b.until
	}
	return time.Time{}
}

func (b *wafBreaker) trip(que string) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.clock()
	if now.Before(b.until) {
		return b.until
	}
	b.strikes++
	d := wafCooldownBase
	for i := 1; i < b.strikes && d < wafCooldownMax; i++ {
		d *= 2
	}
	if d > wafCooldownMax {
		d = wafCooldownMax
	}
	b.until = now.Add(d)
	logger.Log("[WAF] 🛑 CloudFront ha respondido 403: el cortafuegos bloquea esta IP. Pauso %s durante %s (hasta las %s, bloqueo %d). Reintentar antes alarga el bloqueo.",
		que, d, b.until.Local().Format("15:04"), b.strikes)
	return b.until
}

func (b *wafBreaker) ok(quien string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.strikes > 0 {
		logger.Log("[WAF] ✅ %s vuelve a responder con normalidad.", quien)
	}
	b.strikes = 0
}

func isWAFBlockResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	cloudfront := strings.Contains(strings.ToLower(resp.Header.Get("Server")), "cloudfront") ||
		strings.Contains(strings.ToLower(resp.Header.Get("X-Cache")), "cloudfront")
	ctype := strings.ToLower(resp.Header.Get("Content-Type"))
	if resp.Body == nil {
		return cloudfront && !strings.Contains(ctype, "json")
	}
	head, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if bytes.Contains(head, []byte("could not be satisfied")) {
		return true
	}
	return cloudfront && strings.Contains(ctype, "html")
}

func (c *Client) breakerFor(req *http.Request) (*wafBreaker, string, string) {
	if req != nil && req.URL != nil && strings.HasPrefix(req.URL.Path, "/sapi/upload") {
		if u, err := url.Parse(c.uploadURL); err == nil && u.Host == req.URL.Host {
			return &c.wafUpload, "las SUBIDAS a la nube", "El servidor de subidas"
		}
	}
	return &c.waf, "las llamadas a la API (las subidas continúan)", "La API de SAPI"
}

func (c *Client) WAFBlockedUntil() time.Time {
	return c.waf.blockedUntil()
}

func (c *Client) WAFUploadBlockedUntil() time.Time {
	return c.wafUpload.blockedUntil()
}

func (c *Client) ClearWAFPause() {
	for _, b := range []*wafBreaker{&c.waf, &c.wafUpload} {
		b.mu.Lock()
		b.until = time.Time{}
		b.mu.Unlock()
	}
}

func wafBlockedError(until time.Time, method, p string) error {
	return fmt.Errorf("%w (hasta las %s; no se envía %s %s)", ErrWAFBlocked, until.Local().Format("15:04"), method, p)
}
