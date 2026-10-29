package sapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
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
	cfg        config.AccountConfig
	baseURL    string
	uploadURL  string
	httpClient *http.Client
	cookieJar  *cookiejar.Jar
	mu         sync.RWMutex
	refreshMu  sync.Mutex
}

func NewClient(cfg config.AccountConfig) *Client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}

	httpClient := &http.Client{
		Jar:       jar,
		Transport: transport,
		Timeout:   0, // No global timeout to allow huge file uploads
	}

	return &Client{
		cfg:        cfg,
		baseURL:    "https://micloud.movistar.es",
		uploadURL:  "https://upload.micloud.movistar.es",
		httpClient: httpClient,
		cookieJar:  jar,
	}
}

func (c *Client) SetConfig(cfg config.AccountConfig) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
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

func (c *Client) appendValidationKey(u string) string {
	c.mu.RLock()
	vk := c.cfg.ValidationKey
	c.mu.RUnlock()

	if vk == "" {
		return u
	}

	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	if !strings.Contains(u, "validationkey=") {
		return fmt.Sprintf("%s%svalidationkey=%s", u, sep, url.QueryEscape(vk))
	}
	return u
}

func (c *Client) processSAPIResponse(resp *http.Response) {
	if resp == nil || resp.StatusCode >= 400 {
		return
	}

	c.mu.Lock()
	changed := false

	// 1. Check for rotated OAuth authorization header
	if authResp := resp.Header.Get("Authorization"); authResp != "" {
		if rotated := auth.ParseOAuthResponseHeader(authResp); rotated != nil {
			if rotated.AccessToken != "" && rotated.AccessToken != c.cfg.AccessToken {
				c.cfg.AccessToken = rotated.AccessToken
				changed = true
			}
			if rotated.RefreshToken != "" && rotated.RefreshToken != c.cfg.RefreshToken {
				c.cfg.RefreshToken = rotated.RefreshToken
				changed = true
			}
			if rotated.LastRefreshDate > 0 && rotated.LastRefreshDate != c.cfg.LastRefreshDate {
				c.cfg.LastRefreshDate = rotated.LastRefreshDate
				changed = true
			}
			if rotated.ExpiresIn != "" && rotated.ExpiresIn != c.cfg.ExpiresIn {
				c.cfg.ExpiresIn = rotated.ExpiresIn
				changed = true
			}
		}
	}

	// 2. Session cookies
	for _, cookie := range resp.Cookies() {
		if strings.EqualFold(cookie.Name, "validationkey") && cookie.Value != "" && cookie.Value != c.cfg.ValidationKey {
			c.cfg.ValidationKey = cookie.Value
			changed = true
		} else if strings.EqualFold(cookie.Name, "JSESSIONID") && cookie.Value != "" && cookie.Value != c.cfg.JSessionID {
			c.cfg.JSessionID = cookie.Value
			changed = true
		}
	}

	cfgCopy := c.cfg
	c.mu.Unlock()

	if changed && cfgCopy.Phone != "" {
		_ = config.SaveAccountConfig(cfgCopy)
		logger.Log("[OAuth] ✔ Nuevas credenciales/cookies capturadas y guardadas.")
	}
}

func (c *Client) RefreshSession() bool {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()

	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	if cfg.AccessToken == "" && cfg.RefreshToken == "" {
		return false
	}

	logger.Log("[OAuth] 🔄 Renovando sesión SAPI...")

	// 1. Primary attempt: SAPILoginOAuth with current access_token
	if cfg.AccessToken != "" {
		res := auth.SAPILoginOAuth(c.httpClient, cfg.AccessToken, cfg.RefreshToken, cfg.DeviceID, cfg.DeviceName, cfg.LastRefreshDate, cfg.ExpiresIn)
		if res.StatusCode == http.StatusOK && (res.ValidationKey != "" || res.JSessionID != "") {
			c.mu.Lock()
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
		}
	}

	// 2. Fallback: exchange refresh_token with Telefonica OAuth
	if cfg.RefreshToken != "" {
		logger.Log("[OAuth] 🔄 Access token expirado. Canjeando refresh_token en apiseg.telefonica.es...")
		tokens, err := auth.RefreshOAuthToken(cfg.RefreshToken)
		if err == nil && tokens != nil && tokens.AccessToken != "" {
			c.mu.Lock()
			c.cfg.AccessToken = tokens.AccessToken
			if tokens.RefreshToken != "" {
				c.cfg.RefreshToken = tokens.RefreshToken
			}
			c.cfg.LastRefreshDate = time.Now().UnixMilli()
			cfgCopy := c.cfg
			c.mu.Unlock()

			// Persistir inmediatamente los nuevos tokens para proteger contra RTR (Refresh Token Rotation)
			_ = config.SaveAccountConfig(cfgCopy)

			// Con nuevo access_token, inicializar SAPI
			res := auth.SAPILoginOAuth(c.httpClient, tokens.AccessToken, cfgCopy.RefreshToken, cfgCopy.DeviceID, cfgCopy.DeviceName, cfgCopy.LastRefreshDate, cfgCopy.ExpiresIn)
			if res.StatusCode == http.StatusOK {
				c.mu.Lock()
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
			} else {
				logger.Log("[OAuth] ⚠️ Inicialización SAPI devolvió HTTP %d tras refresco: %v", res.StatusCode, res.Error)
			}
		} else if err != nil {
			logger.Log("[OAuth] ❌ Error canjeando refresh_token: %v", err)
		}
	}

	logger.Log("[!] ⚠️ ERROR 401: No se pudo renovar la sesión automáticamente. Requiere revinculación en el panel web.")
	return false
}

func (c *Client) DoRequest(req *http.Request) (*http.Response, error) {
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()

	req.Header.Set("User-Agent", auth.DesktopUserAgent)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
	if cfg.DeviceID != "" {
		req.Header.Set("X-deviceid", cfg.DeviceID)
	}
	if cfg.DeviceName != "" {
		req.Header.Set("X-devicename", cfg.DeviceName)
	}
	if cfg.AccessToken != "" {
		req.Header.Set("Authorization", auth.BuildOAuthHeader(cfg.AccessToken, cfg.RefreshToken, cfg.LastRefreshDate, cfg.ExpiresIn))
	}

	req.URL, _ = url.Parse(c.appendValidationKey(req.URL.String()))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	c.processSAPIResponse(resp)

	if resp.StatusCode == http.StatusUnauthorized {
		// Cannot retry if body cannot be rewinded
		if req.Body != nil && req.GetBody == nil {
			return resp, nil
		}
		_ = resp.Body.Close()

		if c.HasCredentials() && c.RefreshSession() {
			c.mu.RLock()
			newCfg := c.cfg
			c.mu.RUnlock()

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
			newReq.Header.Set("Authorization", auth.BuildOAuthHeader(newCfg.AccessToken, newCfg.RefreshToken, newCfg.LastRefreshDate, newCfg.ExpiresIn))
			newReq.URL, _ = url.Parse(c.appendValidationKey(newReq.URL.String()))

			resp2, err2 := c.httpClient.Do(newReq)
			if err2 == nil {
				c.processSAPIResponse(resp2)
				return resp2, nil
			}
			return resp2, err2
		}
	}

	return resp, nil
}

func (c *Client) GetStorageSpace() (*StorageSpace, error) {
	u := fmt.Sprintf("%s/sapi/user/storage/space?action=get", c.baseURL)
	req, err := http.NewRequest("GET", u, nil)
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

// GetGalleryInventory fetches all existing media items in Movistar Cloud to populate duplicate prevention cache.
func (c *Client) GetGalleryInventory() ([]CloudItem, error) {
	logger.Log("[SAPI] Obteniendo inventario de la Galería en Movistar Cloud para control de duplicados...")

	// Strategy 1: Timeline (/sapi/media/timeline?action=get)
	timelineURL := fmt.Sprintf("%s/sapi/media/timeline?action=get", c.baseURL)
	timelineBody := []byte(`{"data":{"granularity":"daily","sortorder":"creationdate"}}`)

	req, err := http.NewRequest("POST", timelineURL, bytes.NewReader(timelineBody))
	if err == nil {
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.DoRequest(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
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

				// If section.IDs were returned instead of full objects, fetch in chunks of 50
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
						chunkReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/sapi/media?action=get", c.baseURL), bytes.NewReader(chunkBytes))
						chunkReq.Header.Set("Content-Type", "application/json")
						if chunkResp, err := c.DoRequest(chunkReq); err == nil && chunkResp.StatusCode == http.StatusOK {
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
							chunkResp.Body.Close()
							for _, m := range chunkRoot.Data.Media {
								name := m.Name
								size := parseFlexibleInt(m.Size)
								id := parseFlexibleInt(m.ID)
								if name != "" && size > 0 {
									items = append(items, CloudItem{ID: id, Name: name, Size: size})
								}
							}
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

	// Strategy 2: /sapi/media?action=get&limit=5000
	mediaURL := fmt.Sprintf("%s/sapi/media?action=get&limit=5000", c.baseURL)
	mediaBody := []byte(`{"data":{"fields":["name","size","folder","origin"]}}`)
	req2, err := http.NewRequest("POST", mediaURL, bytes.NewReader(mediaBody))
	if err == nil {
		req2.Header.Set("Content-Type", "application/json")
		resp, err := c.DoRequest(req2)
		if err == nil && resp.StatusCode == http.StatusOK {
			defer resp.Body.Close()
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

	logger.Log("[SAPI] Inventario en la nube no devolvió elementos previos (se continuará usando la base de datos local SQLite persistente).")
	return nil, nil
}

// UploadToGallery uploads a media file straight into the Gallery timeline.
// CRITICAL: We deliberately omit 'folderid' to inject directly into the user's Gallery (not a folder).
func (c *Client) UploadToGallery(ctx context.Context, filePath string, captureTime time.Time, mimeType string) (CloudUploadResult, error) {
	fi, err := os.Stat(filePath)
	if err != nil {
		return CloudUploadResult{}, err
	}
	fileName := filepath.Base(filePath)
	fileSize := fi.Size()
	dateStr := captureTime.UTC().Format("20060102T150405Z")

	meta := map[string]interface{}{
		"data": map[string]interface{}{
			"name":             fileName,
			"size":             fileSize,
			"creationdate":     dateStr,
			"modificationdate": dateStr,
			"contenttype":      mimeType,
			// CRITICAL: Do NOT send 'folderid'
		},
	}
	metaBytes, err := json.Marshal(meta)
	if err != nil {
		return CloudUploadResult{}, err
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

		// 1. Data form field (JSON metadata)
		partData, err := mpw.CreateFormField("data")
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if _, err := partData.Write(metaBytes); err != nil {
			_ = pw.CloseWithError(err)
			return
		}

		// 2. File form field (binary stream)
		partFile, err := mpw.CreateFormFile("file", fileName)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		buf := make([]byte, 1024*1024) // 1 MB buffer for high throughput streaming
		if _, err := io.CopyBuffer(partFile, fileHandle, buf); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	uploadEndpoint := fmt.Sprintf("%s/sapi/upload?action=save&acceptasynchronous=true", c.uploadURL)
	req, err := http.NewRequestWithContext(ctx, "POST", uploadEndpoint, pr)
	if err != nil {
		_ = pr.Close()
		return CloudUploadResult{}, err
	}
	req.Header.Set("Content-Type", mpw.FormDataContentType())

	resp, err := c.DoRequest(req)
	if err != nil {
		return CloudUploadResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusAccepted {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return CloudUploadResult{}, fmt.Errorf("upload returned HTTP %d: %s", resp.StatusCode, string(bodyBytes))
	}

	var res struct {
		ID     interface{} `json:"id"`
		Status string      `json:"status"`
		ETag   string      `json:"etag"`
		Data   struct {
			ID     interface{} `json:"id"`
			Status string      `json:"status"`
			ETag   string      `json:"etag"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return CloudUploadResult{}, err
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
