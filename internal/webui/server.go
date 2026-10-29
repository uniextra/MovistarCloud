package webui

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"movistar-cloud-uploader/internal/auth"
	"movistar-cloud-uploader/internal/config"
	"movistar-cloud-uploader/internal/logger"
	"movistar-cloud-uploader/internal/sapi"
	"movistar-cloud-uploader/internal/uploader"
)

//go:embed templates/index.html
var indexHTML []byte

//go:embed static/favicon.ico
var faviconICO []byte

type Server struct {
	client     *sapi.Client
	jobManager *uploader.JobManager
	loginMu    sync.Mutex
	loginState map[string]*pendingLogin
}

type pendingLogin struct {
	Provider string
	Verifier string
	State    string
	Time     time.Time
}

func NewServer(client *sapi.Client, jobManager *uploader.JobManager) *Server {
	return &Server{
		client:     client,
		jobManager: jobManager,
		loginState: make(map[string]*pendingLogin),
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/status", s.handleStatus)
	mux.HandleFunc("/verify_phone", s.handleVerifyPhone)
	mux.HandleFunc("/storage_info", s.handleStorageInfo)
	mux.HandleFunc("/folders", s.handleFolders)
	mux.HandleFunc("/start", s.handleStart)
	mux.HandleFunc("/stop", s.handleStop)
	mux.HandleFunc("/logs", s.handleLogs)
	mux.HandleFunc("/start_login", s.handleStartLogin)
	mux.HandleFunc("/complete_login", s.handleCompleteLogin)
	mux.HandleFunc("/unlink", s.handleUnlink)
	mux.HandleFunc("/test_telegram", s.handleTestTelegram)
	mux.HandleFunc("/save_telegram", s.handleSaveTelegram)

	return mux
}

func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(indexHTML)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconICO)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "version": "v2.0.0"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{"version": "v2.0.0"})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stats := s.jobManager.GetStatus()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": stats.State,
		"stats":  stats,
	})
}

func (s *Server) handleVerifyPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Teléfono inválido"})
		return
	}

	cfg := config.LoadAccountConfig(phone)
	hasCredentials := cfg.RefreshToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "")

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":              "ok",
		"phone":               phone,
		"provider":            cfg.Provider,
		"has_credentials":     hasCredentials,
		"device_id":           cfg.DeviceID,
		"telegram_configured": cfg.TelegramBotToken != "" && cfg.TelegramChatID != "",
		"telegram_bot_token":  cfg.TelegramBotToken,
		"telegram_chat_id":    cfg.TelegramChatID,
	})
}

func (s *Server) handleStorageInfo(w http.ResponseWriter, r *http.Request) {
	phone := config.SanitizePhone(r.URL.Query().Get("phone"))
	if phone != "" {
		cfg := config.LoadAccountConfig(phone)
		if cfg.RefreshToken != "" || (cfg.JSessionID != "" && cfg.ValidationKey != "") {
			s.client.SetConfig(cfg)
		}
	}

	if !s.client.HasCredentials() {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"status":    "unconfigured",
			"message":   "Dispositivo no vinculado",
			"used_str":  "-",
			"total_str": "-",
		})
		return
	}

	space, err := s.client.GetStorageSpace()
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	formatBytes := func(b int64) string {
		const unit = 1024
		if b < unit {
			return fmt.Sprintf("%d B", b)
		}
		div, exp := int64(unit), 0
		for n := b / unit; n >= unit; n /= unit {
			div *= unit
			exp++
		}
		return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
	}

	usedStr := formatBytes(space.UsedBytes)
	totalStr := "Ilimitado"
	if !space.NoLimit && space.TotalBytes > 0 {
		totalStr = formatBytes(space.TotalBytes)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":      "ok",
		"used_bytes":  space.UsedBytes,
		"total_bytes": space.TotalBytes,
		"used_str":    usedStr,
		"total_str":   totalStr,
		"nolimit":     space.NoLimit,
	})
}

func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	requestedPath := r.URL.Query().Get("path")
	if requestedPath == "" {
		requestedPath = "/data"
	}

	cleanPath := filepath.Clean(requestedPath)
	// Restrict browsing strictly to /data root
	if !strings.HasPrefix(cleanPath, "/data") {
		cleanPath = "/data"
	}

	entries, err := os.ReadDir(cleanPath)
	if err != nil {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"status":  "ok",
			"path":    cleanPath,
			"folders": []interface{}{},
		})
		return
	}

	type FolderItem struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}

	var folders []FolderItem
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			folders = append(folders, FolderItem{
				Name: e.Name(),
				Path: filepath.ToSlash(filepath.Join(cleanPath, e.Name())),
			})
		}
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"path":    cleanPath,
		"folders": folders,
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone     string `json:"phone"`
		Folder    string `json:"folder"`
		Recursive bool   `json:"recursive"`
		Takeout   bool   `json:"takeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "JSON inválido"})
		return
	}

	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Introduce un teléfono válido"})
		return
	}

	folder := body.Folder
	if folder == "" {
		folder = "/data"
	}

	if err := s.jobManager.StartJob(phone, folder, body.Recursive, body.Takeout); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.jobManager.StopJob()
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"logs":  logger.GetLogs(),
		"stats": s.jobManager.GetStatus(),
	})
}

func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		Provider string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Introduce un teléfono válido"})
		return
	}

	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if provider != "o2" {
		provider = "movistar"
	}

	verifier, state, authURL := auth.BuildOAuthLoginURL(provider)

	s.loginMu.Lock()
	s.loginState[phone] = &pendingLogin{
		Provider: provider,
		Verifier: verifier,
		State:    state,
		Time:     time.Now(),
	}
	s.loginMu.Unlock()

	logger.Log("[OAuth] 🔑 URL de autorización generada para %s (Proveedor: %s)", phone, provider)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"auth_url": authURL,
		"provider": provider,
	})
}

func (s *Server) handleCompleteLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone     string `json:"phone"`
		CodeOrURL string `json:"code_or_url"`
		Provider  string `json:"provider"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	rawCode := body.CodeOrURL

	if phone == "" || rawCode == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "Datos incompletos"})
		return
	}

	s.loginMu.Lock()
	pending, ok := s.loginState[phone]
	s.loginMu.Unlock()

	var verifier string
	provider := strings.ToLower(strings.TrimSpace(body.Provider))
	if ok && pending != nil {
		verifier = pending.Verifier
		if provider == "" {
			provider = pending.Provider
		}
	}
	if provider != "o2" {
		provider = "movistar"
	}

	code := auth.ExtractAuthCode(rawCode)
	if code == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": "No se encontró el código de autorización"})
		return
	}

	logger.Log("[OAuth] 🔄 Canjeando código para %s en apiseg.telefonica.es (%s)...", phone, provider)
	tokens, err := auth.ExchangeCodeForTokens(code, verifier, provider)
	if err != nil {
		logger.Log("[OAuth] ❌ Error en canje: %v", err)
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	existingCfg := config.LoadAccountConfig(phone)
	devID := existingCfg.DeviceID
	if devID == "" {
		devID = auth.GenerateDeviceID()
	}
	logger.Log("[OAuth] ✔ Tokens obtenidos. Inicializando sesión SAPI para dispositivo %s (%s)...", devID, provider)

	sapiBase := auth.GetSAPIBaseURL(provider)
	nowMS := time.Now().UnixMilli()
	sapiRes := auth.SAPILoginOAuth(nil, sapiBase, tokens.AccessToken, tokens.RefreshToken, devID, "PC Windows", nowMS, strconv.Itoa(tokens.ExpiresIn))

	if sapiRes.StatusCode != http.StatusOK && (sapiRes.ValidationKey == "" && sapiRes.JSessionID == "") {
		logger.Log("[OAuth] ❌ Error inicializando sesión SAPI (HTTP %d): %v", sapiRes.StatusCode, sapiRes.Error)
		provLabel := "Movistar Cloud"
		if provider == "o2" {
			provLabel = "O2 Cloud"
		}
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("Error iniciando sesión en %s (HTTP %d). Comprueba que la línea tenga el servicio activo.", provLabel, sapiRes.StatusCode),
		})
		return
	}

	cfg := config.AccountConfig{
		Provider:         provider,
		Phone:            phone,
		DeviceID:         devID,
		DeviceName:       "PC Windows",
		RefreshToken:     tokens.RefreshToken,
		AccessToken:      tokens.AccessToken,
		LastRefreshDate:  nowMS,
		ExpiresIn:        strconv.Itoa(tokens.ExpiresIn),
		TelegramBotToken: existingCfg.TelegramBotToken,
		TelegramChatID:   existingCfg.TelegramChatID,
	}

	if sapiRes.ValidationKey != "" {
		cfg.ValidationKey = sapiRes.ValidationKey
	}
	if sapiRes.JSessionID != "" {
		cfg.JSessionID = sapiRes.JSessionID
	}
	if sapiRes.LastRefreshDate > 0 {
		cfg.LastRefreshDate = sapiRes.LastRefreshDate
	}
	if sapiRes.RefreshedOAuth != nil {
		if sapiRes.RefreshedOAuth.AccessToken != "" {
			cfg.AccessToken = sapiRes.RefreshedOAuth.AccessToken
		}
		if sapiRes.RefreshedOAuth.RefreshToken != "" {
			cfg.RefreshToken = sapiRes.RefreshedOAuth.RefreshToken
		}
	}

	if err := config.SaveAccountConfig(cfg); err != nil {
		logger.Log("[OAuth] ⚠️ Error guardando credenciales: %v", err)
	}

	s.loginMu.Lock()
	delete(s.loginState, phone)
	s.loginMu.Unlock()

	s.client.SetConfig(cfg)
	if cfg.TelegramBotToken != "" && cfg.TelegramChatID != "" {
		s.jobManager.GetNotifier().UpdateCredentials(cfg.TelegramBotToken, cfg.TelegramChatID)
	}

	logger.Log("[OAuth] ✔ ¡Dispositivo vinculado y guardado con éxito para %s (%s)!", phone, provider)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok", "provider": provider})
}

func (s *Server) handleTestTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	botToken := strings.TrimSpace(body.BotToken)
	chatID := strings.TrimSpace(body.ChatID)

	phone := config.SanitizePhone(body.Phone)
	cfg := config.LoadAccountConfig(phone)
	if botToken == "" {
		botToken = cfg.TelegramBotToken
	}
	if chatID == "" {
		chatID = cfg.TelegramChatID
	}

	if botToken == "" || chatID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Token de bot o Chat ID no configurados",
		})
		return
	}

	s.jobManager.GetNotifier().UpdateCredentials(botToken, chatID)
	if err := s.jobManager.GetNotifier().SendTestAlert(cfg.Provider); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleSaveTelegram(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone    string `json:"phone"`
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": "Introduce un teléfono válido",
		})
		return
	}

	cfg := config.LoadAccountConfig(phone)
	cfg.TelegramBotToken = strings.TrimSpace(body.BotToken)
	cfg.TelegramChatID = strings.TrimSpace(body.ChatID)

	if err := config.SaveAccountConfig(cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	s.client.SetConfig(cfg)
	s.jobManager.GetNotifier().UpdateCredentials(cfg.TelegramBotToken, cfg.TelegramChatID)

	logger.Log("[Telegram] ⚙️ Credenciales de Telegram actualizadas para %s.", phone)
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}

func (s *Server) handleUnlink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonResponse(w, http.StatusMethodNotAllowed, map[string]interface{}{"status": "error", "message": "Método no permitido"})
		return
	}

	var body struct {
		Phone string `json:"phone"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	phone := config.SanitizePhone(body.Phone)
	if phone == "" {
		phone = s.client.GetConfig().Phone
	}

	_ = config.DeleteAccountConfig(phone)
	s.client.SetConfig(config.AccountConfig{})
	s.jobManager.StopJob()

	logger.Log("[OAuth] 🗑️ Dispositivo desvinculado y credenciales eliminadas por el usuario.")
	jsonResponse(w, http.StatusOK, map[string]interface{}{"status": "ok"})
}
