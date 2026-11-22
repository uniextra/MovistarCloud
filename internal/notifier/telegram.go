package notifier

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"movistar-cloud-uploader/internal/logger"
)

type TelegramNotifier struct {
	botToken string
	chatID   string
	client   *http.Client
	mu       sync.Mutex

	lastNotificationAt time.Time
	lastAlerts         map[string]time.Time
	lastError          string
}

var sendRetryDelay = 3 * time.Second
var apiBase = "https://api.telegram.org"

func NewTelegramNotifier(botToken, chatID string) *TelegramNotifier {
	token := strings.TrimSpace(botToken)
	chat := strings.TrimSpace(chatID)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	}
	if chat == "" {
		chat = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	}

	return &TelegramNotifier{
		botToken:   token,
		chatID:     chat,
		client:     newClient(10 * time.Second),
		lastAlerts: make(map[string]time.Time),
	}
}

func (t *TelegramNotifier) IsConfigured() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.botToken != "" && t.chatID != ""
}

func (t *TelegramNotifier) UpdateCredentials(botToken, chatID string) {
	token := strings.TrimSpace(botToken)
	chat := strings.TrimSpace(chatID)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	}
	if chat == "" {
		chat = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.botToken = token
	t.chatID = chat
}

func (t *TelegramNotifier) Status() (bool, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	lastAt := ""
	if !t.lastNotificationAt.IsZero() {
		lastAt = t.lastNotificationAt.Format("2006-01-02 15:04:05")
	}
	return t.botToken != "" && t.chatID != "", lastAt, t.lastError
}

func sanitizeTransportError(err error, token string) string {
	if err == nil {
		return ""
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	msg := err.Error()
	if token != "" {
		msg = strings.ReplaceAll(msg, token, maskSecret(token))
	}
	return msg
}

func maskSecret(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	n := len(s)
	if n <= 6 {
		return "****"
	}
	if n <= 12 {
		return s[:2] + "****" + s[n-2:]
	}
	return s[:4] + "…" + s[n-4:]
}

func (t *TelegramNotifier) SendMessage(text string) error {
	t.mu.Lock()
	token := t.botToken
	chatID := t.chatID
	t.mu.Unlock()

	if token == "" || chatID == "" {
		return fmt.Errorf("telegram not configured")
	}

	payload := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
		"link_preview_options": map[string]bool{
			"is_disabled": true,
		},
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", apiBase, token)
	post := func() (*http.Response, error) {
		req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return t.client.Do(req)
	}

	resp, err := post()
	if err != nil {
		// En fallo de red (conexión caída o rota), se cierran conexiones ociosas y se reintenta tras 3s
		transport.CloseIdleConnections()
		time.Sleep(sendRetryDelay)
		resp, err = post()
	}

	if err != nil {
		safeErr := sanitizeTransportError(err, token)
		t.mu.Lock()
		t.lastError = fmt.Sprintf("Error de red: %s", safeErr)
		t.mu.Unlock()
		logger.Log("[Telegram] ❌ Error conectando con Telegram: %s", safeErr)
		return fmt.Errorf("error de red contactando con Telegram: %s", safeErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		safeBody := strings.ReplaceAll(string(respBody), token, maskSecret(token))
		errMsg := fmt.Sprintf("Telegram respondió HTTP %d: %s", resp.StatusCode, safeBody)
		t.mu.Lock()
		t.lastError = errMsg
		t.mu.Unlock()
		logger.Log("[Telegram] ❌ %s", errMsg)
		return fmt.Errorf("Telegram respondió HTTP %d", resp.StatusCode)
	}

	t.mu.Lock()
	t.lastNotificationAt = time.Now()
	t.lastError = ""
	t.mu.Unlock()

	logger.Log("[Telegram] ✉️ Notificación enviada con éxito a chat %s.", maskSecret(chatID))
	return nil
}

func (t *TelegramNotifier) shouldSendAlert(key string, cooldown time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastAlerts == nil {
		t.lastAlerts = make(map[string]time.Time)
	}
	last, ok := t.lastAlerts[key]
	if ok && time.Since(last) < cooldown {
		return false
	}
	t.lastAlerts[key] = time.Now()
	return true
}

func (t *TelegramNotifier) SendSessionExpiredAlert(provider, phone, details string) error {
	alertKey := fmt.Sprintf("session_expired:%s", phone)
	if !t.shouldSendAlert(alertKey, 1*time.Hour) {
		return nil
	}

	providerName := "Movistar Cloud"
	if strings.EqualFold(provider, "o2") {
		providerName = "O2 Cloud"
	}

	msg := fmt.Sprintf(
		"⚠️ *Sesión de %s caducada*\n\n"+
			"La sesión del dispositivo para la línea *%s* ha expirado o requiere revinculación.\n\n"+
			"Detalle: %s\n"+
			"Hora: %s\n\n"+
			"Por favor, abre el panel Web para revincular tu cuenta.",
		providerName, maskSecret(phone), details, time.Now().Format("2006-01-02 15:04:05"),
	)
	return t.SendMessage(msg)
}

func (t *TelegramNotifier) SendJobCompletedAlert(provider, phone string, total, uploaded, skipped, errorsCount int64, durationSec int64) error {
	providerName := "Movistar Cloud"
	if strings.EqualFold(provider, "o2") {
		providerName = "O2 Cloud"
	}

	durationStr := fmt.Sprintf("%ds", durationSec)
	if durationSec >= 60 {
		durationStr = fmt.Sprintf("%dm %ds", durationSec/60, durationSec%60)
	}

	statusIcon := "✅"
	if errorsCount > 0 {
		statusIcon = "⚠️"
	}

	msg := fmt.Sprintf(
		"%s *Subida finalizada en %s*\n\n"+
			"Cuenta: *%s*\n"+
			"• Total analizados: %d\n"+
			"• Subidos con éxito: %d\n"+
			"• Omitidos (ya en nube): %d\n"+
			"• Errores: %d\n"+
			"• Tiempo transcurrido: %s",
		statusIcon, providerName, maskSecret(phone), total, uploaded, skipped, errorsCount, durationStr,
	)
	return t.SendMessage(msg)
}

func (t *TelegramNotifier) SendTestAlert(provider string) error {
	providerName := "Movistar Cloud"
	if strings.EqualFold(provider, "o2") {
		providerName = "O2 Cloud"
	}

	msg := fmt.Sprintf(
		"🔔 *Notificaciones de %s activas*\n\n"+
			"El sistema de subida automática a la nube se ha configurado correctamente con Telegram.",
		providerName,
	)
	return t.SendMessage(msg)
}
