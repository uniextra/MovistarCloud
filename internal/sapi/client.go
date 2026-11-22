package sapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-cloud-uploader/internal/auth"
	"movistar-cloud-uploader/internal/config"
	"movistar-cloud-uploader/internal/logger"
)

type StorageSpace struct {
	UsedBytes  int64 `json:"used_bytes"`
	TotalBytes int64 `json:"total_bytes"`
	FreeBytes  int64 `json:"free_bytes"`
	NoLimit    bool  `json:"nolimit"`
}

type CloudItem struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

type CloudUploadResult struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	ETag   string `json:"etag"`
}

type Client struct {
	cfg              config.AccountConfig
	baseURL          string
	uploadURL        string
	httpClient       *http.Client
	mu               sync.RWMutex
	refreshMu        sync.Mutex
	lastRefreshTime  time.Time
	OnSessionExpired func(err error)

	waf       wafBreaker
	wafUpload wafBreaker

	sapiSem        chan struct{}
	pacingInterval time.Duration
	pacingMu       sync.Mutex
	lastReqTime    time.Time

	loginFailures     int
	loginFailingSince time.Time
	loginRetryAt      time.Time
	refreshTransient  bool
}

func NewClient(cfg config.AccountConfig) *Client {
	transport := auth.NewFingerprintedTransport()

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   0, // Sin timeout global para permitir transferencias de ficheros grandes
		Jar:       nil,
	}

	baseURL := auth.GetSAPIBaseURL(cfg.Provider)
	uploadURL := auth.GetUploadBaseURL(cfg.Provider)

	maxConcurrent := 3
	if v := strings.TrimSpace(os.Getenv("SAPI_MAX_CONCURRENT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConcurrent = n
		}
	}

	pacingMs := 50
	if v := strings.TrimSpace(os.Getenv("SAPI_PACING_MS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			pacingMs = n
		}
	}

	return &Client{
		cfg:            cfg,
		baseURL:        baseURL,
		uploadURL:      uploadURL,
		httpClient:     httpClient,
		sapiSem:        make(chan struct{}, maxConcurrent),
		pacingInterval: time.Duration(pacingMs) * time.Millisecond,
	}
}

func (c *Client) SetConfig(cfg config.AccountConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.baseURL = auth.GetSAPIBaseURL(cfg.Provider)
	c.uploadURL = auth.GetUploadBaseURL(cfg.Provider)
	c.loginFailures = 0
	c.loginFailingSince = time.Time{}
	c.loginRetryAt = time.Time{}
	c.refreshTransient = false
}

func (c *Client) GetConfig() config.AccountConfig {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *Client) HasCredentials() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg.RefreshToken != "" || c.cfg.AccessToken != "" || (c.cfg.JSessionID != "" && c.cfg.ValidationKey != "")
}

func (c *Client) paceRequest() {
	c.pacingMu.Lock()
	defer c.pacingMu.Unlock()
	if c.pacingInterval <= 0 {
		return
	}
	now := time.Now()
	if !c.lastReqTime.IsZero() {
		elapsed := now.Sub(c.lastReqTime)
		if elapsed < c.pacingInterval {
			time.Sleep(c.pacingInterval - elapsed)
			now = time.Now()
		}
	}
	c.lastReqTime = now
}

func (c *Client) acquireSlot() {
	if c.sapiSem != nil {
		c.sapiSem <- struct{}{}
	}
	c.paceRequest()
}

func (c *Client) releaseSlot() {
	if c.sapiSem != nil {
		<-c.sapiSem
	}
}

func (c *Client) appendValidationKey(rawURL string) string {
	c.mu.RLock()
	vk := c.cfg.ValidationKey
	c.mu.RUnlock()

	if vk == "" {
		return rawURL
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if strings.HasSuffix(parsed.Path, "/system/information") && auth.IsDesktopProfile() {
		return rawURL
	}
	if parsed.Query().Has("validationkey") {
		return rawURL
	}
	if parsed.RawQuery == "" {
		parsed.RawQuery = "validationkey=" + url.QueryEscape(vk)
	} else {
		parsed.RawQuery += "&validationkey=" + url.QueryEscape(vk)
	}
	return parsed.String()
}

func (c *Client) replaceValidationKey(rawURL string) string {
	c.mu.RLock()
	vk := c.cfg.ValidationKey
	c.mu.RUnlock()
	parsed, err := url.Parse(rawURL)
	if vk == "" || err != nil {
		return rawURL
	}
	q := parsed.Query()
	q.Set("validationkey", vk)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func sessionCookieHeader(jsessionID, validationKey string) string {
	var parts []string
	if jsessionID != "" {
		parts = append(parts, "JSESSIONID="+jsessionID)
	}
	if validationKey != "" && (!auth.IsDesktopProfile() || jsessionID == "") {
		parts = append(parts, "validationKey="+validationKey)
		parts = append(parts, "validationkey="+validationKey)
	}
	return strings.Join(parts, "; ")
}

func (c *Client) processSAPIResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}

	isUploadHost := resp.Request != nil && resp.Request.URL != nil &&
		strings.HasPrefix(strings.ToLower(resp.Request.URL.Hostname()), "upload.")

	var changed bool
	var tokensRotated bool
	c.mu.Lock()

	// 1. Cabecera Authorization devuelta con tokens rotados (incluso en 401)
	if authResp := resp.Header.Get("Authorization"); authResp != "" {
		if rotated := auth.ParseOAuthResponseHeader(authResp); rotated != nil {
			if rotated.AccessToken != "" && rotated.AccessToken != c.cfg.AccessToken {
				c.cfg.AccessToken = rotated.AccessToken
				changed = true
				tokensRotated = true
			}
			if rotated.RefreshToken != "" && rotated.RefreshToken != c.cfg.RefreshToken {
				c.cfg.RefreshToken = rotated.RefreshToken
				changed = true
				tokensRotated = true
			}
			if rotated.LastRefreshDate > 0 && rotated.LastRefreshDate != c.cfg.LastRefreshDate {
				c.cfg.LastRefreshDate = rotated.LastRefreshDate
				changed = true
			} else if rotated.LastRefreshDate <= 0 && rotated.AccessToken != "" && resp.StatusCode < 400 && auth.IsDesktopProfile() {
				c.cfg.LastRefreshDate = time.Now().UnixMilli()
				changed = true
				tokensRotated = true
			}
			if rotated.ExpiresIn != "" && rotated.ExpiresIn != c.cfg.ExpiresIn {
				c.cfg.ExpiresIn = rotated.ExpiresIn
				changed = true
			}
		}
	}

	// 2. Cookies de sesión (solo con código < 400 para no capturar cookies anónimas de HTMLs de error)
	if resp.StatusCode < 400 {
		for _, cookie := range resp.Cookies() {
			if strings.EqualFold(cookie.Name, "validationkey") && cookie.Value != "" && cookie.Value != c.cfg.ValidationKey {
				c.cfg.ValidationKey = cookie.Value
				changed = true
			} else if strings.EqualFold(cookie.Name, "JSESSIONID") && cookie.Value != "" && cookie.Value != c.cfg.JSessionID && !(isUploadHost && c.cfg.JSessionID != "") {
				c.cfg.JSessionID = cookie.Value
				changed = true
			}
		}
	}

	cfgCopy := c.cfg
	c.mu.Unlock()

	if changed && cfgCopy.Phone != "" {
		_ = config.SaveAccountConfig(cfgCopy)
		logger.Log("[OAuth] ✔ Nuevas credenciales/cookies capturadas y guardadas.")
	}
	return changed || tokensRotated
}

func (c *Client) retryRequestWithCurrentConfig(req *http.Request) (*http.Response, error) {
	cfg := c.GetConfig()
	var retryBody io.Reader
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		retryBody = body
	}
	newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), retryBody)
	if err != nil {
		return nil, err
	}
	newReq.Header = req.Header.Clone()
	newReq.Header.Del("Authorization")
	newReq.Header.Del("Cookie")
	if cfg.AccessToken != "" {
		newReq.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
	}
	if cfg.JSessionID != "" || cfg.ValidationKey != "" {
		newReq.Header.Set("Cookie", sessionCookieHeader(cfg.JSessionID, cfg.ValidationKey))
	}
	newReq.URL, _ = url.Parse(c.replaceValidationKey(newReq.URL.String()))
	resp, err := c.httpClient.Do(newReq)
	if err != nil {
		return nil, err
	}
	c.processSAPIResponse(resp)
	return resp, nil
}

func (c *Client) RefreshSession() bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	cfg := c.cfg
	lastRef := c.lastRefreshTime
	c.mu.RUnlock()

	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return false
	}

	// Protección contra RTR (Refresh Token Rotation): deduplicación si se refrescó hace < 30s
	if !lastRef.IsZero() && time.Since(lastRef) < 30*time.Second {
		return true
	}

	// Respetar espera por fallo previo en perfil desktop
	if auth.IsDesktopProfile() {
		c.mu.RLock()
		retryAt := c.loginRetryAt
		c.mu.RUnlock()
		if time.Now().Before(retryAt) {
			c.mu.Lock()
			c.refreshTransient = true
			c.mu.Unlock()
			return false
		}
	}

	logger.Log("[OAuth] 🔄 Renovando sesión SAPI...")
	c.mu.Lock()
	c.refreshTransient = false
	c.mu.Unlock()

	sapiBase := auth.GetSAPIBaseURL(cfg.Provider)

	// 1. Intento primario: SAPILoginOAuth con access_token actual
	if cfg.AccessToken != "" {
		res := auth.SAPILoginOAuth(c.httpClient, sapiBase, cfg.AccessToken, cfg.RefreshToken, cfg.DeviceID, cfg.DeviceName, cfg.LastRefreshDate, cfg.ExpiresIn)
		if res.StatusCode == http.StatusOK && (res.ValidationKey != "" || res.JSessionID != "" || res.RefreshedOAuth != nil) {
			c.mu.Lock()
			c.loginFailures = 0
			c.loginFailingSince = time.Time{}
			c.loginRetryAt = time.Time{}
			c.lastRefreshTime = time.Now()
			if res.ValidationKey != "" {
				c.cfg.ValidationKey = res.ValidationKey
			}
			if res.JSessionID != "" {
				c.cfg.JSessionID = res.JSessionID
			}
			if res.LastRefreshDate > 0 {
				c.cfg.LastRefreshDate = res.LastRefreshDate
			}
			if res.RefreshedOAuth != nil {
				if res.RefreshedOAuth.AccessToken != "" {
					c.cfg.AccessToken = res.RefreshedOAuth.AccessToken
				}
				if res.RefreshedOAuth.RefreshToken != "" {
					c.cfg.RefreshToken = res.RefreshedOAuth.RefreshToken
				}
			}
			cfgCopy := c.cfg
			c.mu.Unlock()

			if cfgCopy.Phone != "" {
				_ = config.SaveAccountConfig(cfgCopy)
			}
			logger.Log("[OAuth] ✔ Sesión SAPI refrescada exitosamente.")
			return true
		} else {
			if auth.IsDesktopProfile() {
				if !c.desktopLoginFailed(&res) {
					if c.OnSessionExpired != nil {
						go c.OnSessionExpired(errors.New("sesión expirada de forma definitiva"))
					}
					return false
				}
			}
		}
	}

	// 2. Fallback: Canje en apiseg.telefonica.es con refresh_token
	if cfg.RefreshToken != "" {
		logger.Log("[OAuth] 🔄 Access token expirado. Canjeando refresh_token en apiseg (%s)...", cfg.Provider)
		tokens, err := auth.RefreshOAuthToken(cfg.RefreshToken, cfg.Provider)
		if err == nil && tokens != nil && tokens.AccessToken != "" {
			c.mu.Lock()
			c.cfg.AccessToken = tokens.AccessToken
			if tokens.RefreshToken != "" {
				c.cfg.RefreshToken = tokens.RefreshToken
			}
			c.cfg.LastRefreshDate = time.Now().UnixMilli()
			c.lastRefreshTime = time.Now()
			cfgCopy := c.cfg
			c.mu.Unlock()

			_ = config.SaveAccountConfig(cfgCopy)

			// Con nuevo access_token, inicializar SAPI
			res := auth.SAPILoginOAuth(c.httpClient, sapiBase, tokens.AccessToken, cfgCopy.RefreshToken, cfgCopy.DeviceID, cfgCopy.DeviceName, cfgCopy.LastRefreshDate, cfgCopy.ExpiresIn)
			if res.StatusCode == http.StatusOK {
				c.mu.Lock()
				c.loginFailures = 0
				c.loginFailingSince = time.Time{}
				c.loginRetryAt = time.Time{}
				if res.ValidationKey != "" {
					c.cfg.ValidationKey = res.ValidationKey
				}
				if res.JSessionID != "" {
					c.cfg.JSessionID = res.JSessionID
				}
				if res.LastRefreshDate > 0 {
					c.cfg.LastRefreshDate = res.LastRefreshDate
				}
				if res.RefreshedOAuth != nil {
					if res.RefreshedOAuth.AccessToken != "" {
						c.cfg.AccessToken = res.RefreshedOAuth.AccessToken
					}
					if res.RefreshedOAuth.RefreshToken != "" {
						c.cfg.RefreshToken = res.RefreshedOAuth.RefreshToken
					}
				}
				cfgCopy2 := c.cfg
				c.mu.Unlock()

				if cfgCopy2.Phone != "" {
					_ = config.SaveAccountConfig(cfgCopy2)
				}
				logger.Log("[OAuth] ✔ Tokens renovados y sesión SAPI inicializada con éxito.")
				return true
			}
		} else if err != nil {
			logger.Log("[OAuth] ❌ Error canjeando refresh_token: %v", err)
		}
	}

	logger.Log("[!] ⚠️ Fallo al renovar sesión SAPI.")
	return false
}

func (c *Client) DoRequest(req *http.Request) (*http.Response, error) {
	freno, que, quien := c.breakerFor(req)
	if until := freno.blockedUntil(); !until.IsZero() {
		return nil, wafBlockedError(until, req.Method, req.URL.Path)
	}

	// Las subidas al host de subidas tienen su propio flujo y no ocupan el slot de la API SAPI
	if freno == &c.wafUpload {
		c.paceRequest()
	} else {
		c.acquireSlot()
		defer c.releaseSlot()
	}

	if until := freno.blockedUntil(); !until.IsZero() {
		return nil, wafBlockedError(until, req.Method, req.URL.Path)
	}

	resp, err := c.doRequestLocked(req)
	if err == nil && resp != nil {
		if isWAFBlockResponse(resp) {
			freno.trip(que)
		} else if resp.StatusCode < 500 {
			freno.ok(quien)
		}
	}
	return resp, err
}

func (c *Client) doRequestLocked(req *http.Request) (*http.Response, error) {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	desktop := auth.IsDesktopProfile()
	req.Header.Set("User-Agent", auth.GetDesktopUserAgent())
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "*/*")
	}

	if desktop {
		if req.Header.Get("Accept-Language") == "" {
			req.Header.Set("Accept-Language", auth.DesktopAcceptLanguage)
		}
		if req.Header.Get("x-request-id") == "" {
			req.Header.Set("x-request-id", auth.NewRequestID())
		}
		if req.Header.Get("Connection") == "" {
			req.Header.Set("Connection", "Keep-Alive")
		}
	} else {
		if req.Header.Get("Accept-Language") == "" {
			req.Header.Set("Accept-Language", "es-ES,es;q=0.9,en;q=0.8")
		}
		if req.Header.Get("X-Requested-With") == "" {
			req.Header.Set("X-Requested-With", "XMLHttpRequest")
		}
	}

	if cfg.DeviceID != "" {
		req.Header.Set("X-deviceid", auth.SanitizeDeviceID(cfg.DeviceID))
	}
	if cfg.AccessToken != "" {
		req.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
	}
	if req.Header.Get("Cookie") == "" && (cfg.ValidationKey != "" || cfg.JSessionID != "") {
		req.Header.Set("Cookie", sessionCookieHeader(cfg.JSessionID, cfg.ValidationKey))
	}

	req.URL, _ = url.Parse(c.appendValidationKey(req.URL.String()))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) && urlErr.Err != nil {
			return nil, fmt.Errorf("SAPI request failed (%s %s): %w", req.Method, req.URL.Path, urlErr.Err)
		}
		return nil, err
	}

	credentialsRotated := c.processSAPIResponse(resp)

	// Si SAPI responde 401 pero incluye cabecera Authorization rotada, reintentamos inmediatamente
	if resp.StatusCode == http.StatusUnauthorized && credentialsRotated && (req.Body == nil || req.GetBody != nil) {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return c.retryRequestWithCurrentConfig(req)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if req.Body != nil && req.GetBody == nil {
			// Streaming upload: renovar sesión para el siguiente reintento del uploader
			if !credentialsRotated && c.HasCredentials() {
				c.RefreshSession()
			}
			return resp, nil
		}

		if c.HasCredentials() {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()

			if c.RefreshSession() {
				resp2, err2 := c.retryRequestWithCurrentConfig(req)
				if err2 == nil && resp2 != nil {
					return resp2, nil
				}
				if err2 != nil {
					return resp2, err2
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
	}

	// Retroceso suave ante 429, 503 o 408
	if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusRequestTimeout) &&
		(req.Body == nil || req.GetBody != nil) {
		const maxBackoffRetries = 4
		for retry := 1; retry <= maxBackoffRetries; retry++ {
			backoff := time.Duration(1<<(retry-1)) * time.Second
			if backoff > 8*time.Second {
				backoff = 8 * time.Second
			}
			jitter := time.Duration((time.Now().UnixNano()%400)+100) * time.Millisecond
			sleepDur := backoff + jitter
			if resp.StatusCode == http.StatusRequestTimeout {
				c.httpClient.CloseIdleConnections()
			}
			logger.Log("[SAPI] ⏳ Rate limit o servicio ocupado (%s %s HTTP %d). Reintentando en %v (%d/%d)...", req.Method, req.URL.Path, resp.StatusCode, sleepDur, retry, maxBackoffRetries)
			_ = resp.Body.Close()
			time.Sleep(sleepDur)

			var retryBody io.Reader
			if req.GetBody != nil {
				rb, err := req.GetBody()
				if err != nil {
					return nil, err
				}
				retryBody = rb
			}
			newReq, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), retryBody)
			if err != nil {
				return nil, err
			}
			newReq.Header = req.Header.Clone()
			respRetry, errRetry := c.httpClient.Do(newReq)
			if errRetry != nil {
				return nil, errRetry
			}
			c.processSAPIResponse(respRetry)
			if respRetry.StatusCode != http.StatusTooManyRequests && respRetry.StatusCode != http.StatusServiceUnavailable {
				return respRetry, nil
			}
			resp = respRetry
		}
	}

	return resp, nil
}

func (c *Client) GetStorageSpace() (*StorageSpace, error) {
	c.mu.RLock()
	baseURL := c.baseURL
	c.mu.RUnlock()

	u := fmt.Sprintf("%s/sapi/user/storage/space?action=get", baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.DoRequest(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("storage space check returned HTTP %d: %s", resp.StatusCode, string(b))
	}

	var root struct {
		Data struct {
			UsedBytes  interface{} `json:"usedbytes"`
			TotalBytes interface{} `json:"totalbytes"`
			FreeBytes  interface{} `json:"freebytes"`
			NoLimit    interface{} `json:"nolimit"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, err
	}

	parseNum := func(v interface{}) int64 {
		switch val := v.(type) {
		case float64:
			return int64(val)
		case string:
			n, _ := strconv.ParseInt(val, 10, 64)
			return n
		case json.Number:
			n, _ := val.Int64()
			return n
		}
		return 0
	}

	res := &StorageSpace{
		UsedBytes:  parseNum(root.Data.UsedBytes),
		TotalBytes: parseNum(root.Data.TotalBytes),
		FreeBytes:  parseNum(root.Data.FreeBytes),
	}

	if nl, ok := root.Data.NoLimit.(bool); ok {
		res.NoLimit = nl
	} else if s, ok := root.Data.NoLimit.(string); ok && strings.EqualFold(s, "true") {
		res.NoLimit = true
	} else if res.TotalBytes == 0 {
		res.NoLimit = true
	}

	return res, nil
}

func (c *Client) GetGalleryInventory() ([]CloudItem, error) {
	logger.Log("[SAPI] Obteniendo inventario de la Galería en Movistar Cloud para control de duplicados...")

	c.mu.RLock()
	baseURL := c.baseURL
	c.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Estrategia 1: Timeline (/sapi/media/timeline?action=get)
	timelineURL := fmt.Sprintf("%s/sapi/media/timeline?action=get", baseURL)
	timelineBody := []byte(`{"data":{"granularity":"daily","sortorder":"creationdate"}}`)

	req, err := http.NewRequestWithContext(ctx, "POST", timelineURL, bytes.NewReader(timelineBody))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.DoRequest(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var tRoot struct {
					Data struct {
						Timeline []struct {
							Media []struct {
								ID   interface{} `json:"id"`
								Name string      `json:"name"`
								Size interface{} `json:"size"`
							} `json:"media"`
							IDs []interface{} `json:"ids"`
						} `json:"timeline"`
					} `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&tRoot); err == nil {
					var items []CloudItem
					var pendingIDs []int64

					for _, section := range tRoot.Data.Timeline {
						for _, m := range section.Media {
							name := m.Name
							size := parseFlexibleInt(m.Size)
							id := parseFlexibleInt(m.ID)
							if name != "" && size > 0 {
								items = append(items, CloudItem{ID: id, Name: name, Size: size})
							}
						}
						for _, idVal := range section.IDs {
							if id := parseFlexibleInt(idVal); id > 0 {
								pendingIDs = append(pendingIDs, id)
							}
						}
					}

					if len(pendingIDs) > 0 {
						for i := 0; i < len(pendingIDs); i += 50 {
							end := i + 50
							if end > len(pendingIDs) {
								end = len(pendingIDs)
							}
							chunk := pendingIDs[i:end]
							var idPayload struct {
								Data struct {
									IDs []map[string]int64 `json:"ids"`
								} `json:"data"`
							}
							for _, id := range chunk {
								idPayload.Data.IDs = append(idPayload.Data.IDs, map[string]int64{"id": id})
							}
							chunkBytes, _ := json.Marshal(idPayload)
							chunkReq, _ := http.NewRequestWithContext(ctx, "POST", fmt.Sprintf("%s/sapi/media?action=get", baseURL), bytes.NewReader(chunkBytes))
							chunkReq.Header.Set("Content-Type", "application/json")
							if chunkResp, err := c.DoRequest(chunkReq); err == nil {
								if chunkResp.StatusCode == http.StatusOK {
									var chunkRoot struct {
										Data struct {
											Media []struct {
												ID   interface{} `json:"id"`
												Name string      `json:"name"`
												Size interface{} `json:"size"`
											} `json:"media"`
										} `json:"data"`
									}
									_ = json.NewDecoder(chunkResp.Body).Decode(&chunkRoot)
									for _, m := range chunkRoot.Data.Media {
										name := m.Name
										size := parseFlexibleInt(m.Size)
										id := parseFlexibleInt(m.ID)
										if name != "" && size > 0 {
											items = append(items, CloudItem{ID: id, Name: name, Size: size})
										}
									}
								}
								_ = chunkResp.Body.Close()
							}
						}
					}

					if len(items) > 0 {
						logger.Log("[SAPI] ✔ Inventario en la nube cargado vía Timeline: %d elementos.", len(items))
						return items, nil
					}
				}
			}
		}
	}

	// Estrategia 2: /sapi/media?action=get&limit=5000
	mediaURL := fmt.Sprintf("%s/sapi/media?action=get&limit=5000", baseURL)
	mediaBody := []byte(`{"data":{"fields":["name","size","folder","origin"]}}`)
	req2, err := http.NewRequestWithContext(ctx, "POST", mediaURL, bytes.NewReader(mediaBody))
	if err == nil {
		req2.Header.Set("Content-Type", "application/json")
		resp, err := c.DoRequest(req2)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var mRoot struct {
					Data struct {
						Media []struct {
							ID   interface{} `json:"id"`
							Name string      `json:"name"`
							Size interface{} `json:"size"`
						} `json:"media"`
					} `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&mRoot); err == nil {
					var items []CloudItem
					for _, m := range mRoot.Data.Media {
						name := m.Name
						size := parseFlexibleInt(m.Size)
						id := parseFlexibleInt(m.ID)
						if name != "" && size > 0 {
							items = append(items, CloudItem{ID: id, Name: name, Size: size})
						}
					}
					if len(items) > 0 {
						logger.Log("[SAPI] ✔ Inventario en la nube cargado vía /sapi/media: %d elementos.", len(items))
						return items, nil
					}
				}
			}
		}
	}

	logger.Log("[SAPI] Inventario en la nube no devolvió elementos previos (se continuará usando la base de datos local SQLite persistente).")
	return nil, nil
}

// UploadToGallery sube un fichero multimedia a la galería con vigilancia de sockets (stallReader)
// y reintentos ante 408, 429, 5xx, 401 y validación MED-1017.
func (c *Client) UploadToGallery(ctx context.Context, filePath string, captureTime time.Time, mimeType string, existingMediaID *int64) (CloudUploadResult, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return CloudUploadResult{}, err
	}
	fileName := filepath.Base(filePath)
	fileSize := fi.Size()
	dateStr := captureTime.UTC().Format("20060102T150405Z")

	metaData := map[string]interface{}{
		"name":             fileName,
		"size":             fileSize,
		"creationdate":     dateStr,
		"modificationdate": dateStr,
		"contenttype":      mimeType,
	}
	if existingMediaID != nil && *existingMediaID > 0 {
		metaData["id"] = *existingMediaID
		logger.Log("[SAPI] 📝 Sobrescritura in-place para ID existente: %d (%s)", *existingMediaID, fileName)
	}

	meta := map[string]interface{}{
		"data": metaData,
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return CloudUploadResult{}, err
	}

	maxAttempts := config.UploadAttempts()
	if maxAttempts < 4 {
		maxAttempts = 4
	}

	for attempt := 0; attempt < maxAttempts; attempt++ {
		select {
		case <-ctx.Done():
			return CloudUploadResult{}, ctx.Err()
		default:
		}

		if attempt > 0 {
			wait := time.Duration(1<<attempt) * time.Second
			if wait > config.UploadRetryMaxWait() {
				wait = config.UploadRetryMaxWait()
			}
			jitter := time.Duration((time.Now().UnixNano()%500)+100) * time.Millisecond
			sleepTime := wait + jitter
			logger.Log("[SAPI] ⏳ Reintentando subida de %s en %v (intento %d/%d)...", fileName, sleepTime, attempt+1, maxAttempts)
			select {
			case <-ctx.Done():
				return CloudUploadResult{}, ctx.Err()
			case <-time.After(sleepTime):
			}
		}

		fileHandle, err := os.Open(filePath)
		if err != nil {
			return CloudUploadResult{}, err
		}

		pr, pw := io.Pipe()
		mpw := multipart.NewWriter(pw)

		go func() {
			defer fileHandle.Close()
			defer pw.Close()
			defer mpw.Close()

			partData, err := mpw.CreateFormField("data")
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			if _, err := partData.Write(metaBytes); err != nil {
				_ = pw.CloseWithError(err)
				return
			}

			partFile, err := mpw.CreateFormFile("file", fileName)
			if err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			buf := make([]byte, 1024*1024)
			if _, err := io.CopyBuffer(partFile, fileHandle, buf); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}()

		c.mu.RLock()
		upURL := c.uploadURL
		c.mu.RUnlock()

		uploadEndpoint := fmt.Sprintf("%s/sapi/upload?action=save&acceptasynchronous=true", upURL)

		reqCtx, reqCancel := context.WithCancel(ctx)
		// Vigilante de subida parada: si el servidor no lee durante 60s, cancela el contexto del envío
		watchedBody := newStallReader(pr, 60*time.Second, func() {
			logger.Log("[SAPI] 🔌 Subida de %s parada: el servidor dejó de recibir datos durante >60s.", fileName)
			reqCancel()
		})

		req, err := http.NewRequestWithContext(reqCtx, "POST", uploadEndpoint, watchedBody)
		if err != nil {
			watchedBody.stop()
			_ = pr.Close()
			reqCancel()
			return CloudUploadResult{}, err
		}
		req.Header.Set("Content-Type", mpw.FormDataContentType())

		resp, err := c.DoRequest(req)
		stalled := watchedBody.stop()
		_ = pr.Close()
		reqCancel()

		if err != nil {
			if stalled {
				c.httpClient.CloseIdleConnections()
				continue
			}
			if reqCtx.Err() != nil && ctx.Err() == nil {
				// Cancelado por timeout local o watchdog
				c.httpClient.CloseIdleConnections()
				continue
			}
			// Si el error fue de red/WAF, continuar reintentando
			logger.Log("[SAPI] ⚠️ Error en subida de %s: %v", fileName, err)
			continue
		}

		bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			continue
		}

		bodyStr := string(bodyBytes)

		// 1. Manejo de 408 Timeout en subida
		if resp.StatusCode == http.StatusRequestTimeout {
			c.httpClient.CloseIdleConnections()
			time.Sleep(2 * time.Second)
			continue
		}

		// 2. Manejo de 401 en subida
		if resp.StatusCode == http.StatusUnauthorized {
			c.httpClient.CloseIdleConnections()
			if c.RefreshSession() {
				time.Sleep(1 * time.Second)
				continue
			}
			return CloudUploadResult{}, fmt.Errorf("upload unauthorized (HTTP 401): %s", bodyStr)
		}

		// 3. Manejo de 429 Too Many Requests o 5xx
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			c.httpClient.CloseIdleConnections()
			continue
		}

		// 4. MED-1017: Media en validación asíncrona
		if strings.Contains(bodyStr, "MED-1017") || strings.Contains(strings.ToLower(bodyStr), "media not validated") {
			medWait := 2500*time.Millisecond + time.Duration(attempt)*1500*time.Millisecond
			logger.Log("[SAPI] ⏳ Servidor validando media asíncronamente (MED-1017). Reintentando en %v (intento %d/%d)...", medWait, attempt+1, maxAttempts)
			select {
			case <-ctx.Done():
				return CloudUploadResult{}, ctx.Err()
			case <-time.After(medWait):
				continue
			}
		}

		// 5. MED-1048 o PAPI-0000: Error temporal de proceso
		if strings.Contains(bodyStr, "MED-1048") || strings.Contains(bodyStr, "PAPI-0000") {
			time.Sleep(3 * time.Second)
			continue
		}

		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
			return CloudUploadResult{}, fmt.Errorf("upload returned HTTP %d: %s", resp.StatusCode, bodyStr)
		}

		var res struct {
			ID     interface{} `json:"id"`
			Status string      `json:"status"`
			ETag   string      `json:"etag"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Data struct {
				ID     interface{} `json:"id"`
				Status string      `json:"status"`
				ETag   string      `json:"etag"`
			} `json:"data"`
		}

		if err := json.Unmarshal(bodyBytes, &res); err != nil {
			return CloudUploadResult{}, err
		}

		if res.Error != nil && res.Error.Code != "" {
			if res.Error.Code == "MED-1017" {
				medWait := 2500*time.Millisecond + time.Duration(attempt)*1500*time.Millisecond
				logger.Log("[SAPI] ⏳ Servidor validando media asíncronamente (MED-1017 en error body). Reintentando en %v...", medWait)
				select {
				case <-ctx.Done():
					return CloudUploadResult{}, ctx.Err()
				case <-time.After(medWait):
					continue
				}
			}
			return CloudUploadResult{}, fmt.Errorf("SAPI error %s: %s", res.Error.Code, res.Error.Message)
		}

		finalID := parseFlexibleInt(res.ID)
		if finalID == 0 {
			finalID = parseFlexibleInt(res.Data.ID)
		}

		status := res.Status
		if status == "" {
			status = res.Data.Status
		}

		etag := res.ETag
		if etag == "" {
			etag = res.Data.ETag
		}

		if finalID == 0 {
			return CloudUploadResult{}, errors.New("upload response did not contain a valid media ID")
		}

		return CloudUploadResult{
			ID:     finalID,
			Status: status,
			ETag:   etag,
		}, nil
	}

	return CloudUploadResult{}, errors.New("subida fallida tras agotar reintentos")
}

func parseFlexibleInt(v interface{}) int64 {
	switch val := v.(type) {
	case float64:
		return int64(val)
	case int64:
		return val
	case int:
		return int64(val)
	case string:
		n, _ := strconv.ParseInt(val, 10, 64)
		return n
	case json.Number:
		n, _ := val.Int64()
		return n
	}
	return 0
}
