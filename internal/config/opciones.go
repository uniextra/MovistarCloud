package config

import (
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Perfil con el que el cliente se presenta ante SAPI (SAPI_CLIENT_PROFILE).
const (
	ProfileDesktop = "desktop"
	ProfileBrowser = "browser"
)

// ClientProfile devuelve el perfil activo: "desktop" (por defecto), o "browser" con
// SAPI_CLIENT_PROFILE=browser o en modo web (WEB_SESSION=true).
func ClientProfile() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SAPI_CLIENT_PROFILE"))) {
	case "browser", "web", "chrome":
		return ProfileBrowser
	case "desktop", "windows":
		return ProfileDesktop
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("WEB_SESSION")), "true") {
		return ProfileBrowser
	}
	return ProfileDesktop
}

// IsDesktopProfile indica si el cliente imita al cliente oficial de escritorio.
func IsDesktopProfile() bool {
	return ClientProfile() == ProfileDesktop
}

// OfficialClient (SAPI_OFFICIAL_CLIENT, por defecto true): con el perfil desktop,
// imita al cliente oficial de Windows (O2 / Movistar Cloud).
func OfficialClient() bool {
	return IsDesktopProfile() && envBool("SAPI_OFFICIAL_CLIENT", true)
}

// UploadAttempts (SAPI_UPLOAD_ATTEMPTS, por defecto 8): intentos de subida con esperas crecientes.
func UploadAttempts() int { return envInt("SAPI_UPLOAD_ATTEMPTS", 8) }

// UploadRetryMaxWait (SAPI_UPLOAD_RETRY_MAX_SEC, por defecto 60s): tope de espera entre reintentos.
func UploadRetryMaxWait() time.Duration {
	return envSeconds("SAPI_UPLOAD_RETRY_MAX_SEC", 60*time.Second)
}

// UploadResponseTimeout (UPLOAD_RESPONSE_TIMEOUT_SEC, por defecto 300s / 5m): tiempo máximo
// esperando respuesta tras enviar el fichero a upload.*.
func UploadResponseTimeout() time.Duration {
	return envSeconds("UPLOAD_RESPONSE_TIMEOUT_SEC", 300*time.Second)
}

// Keepalive (SAPI_KEEPALIVE, por defecto false): reutiliza conexiones TLS en desktop.
func Keepalive() bool { return envBool("SAPI_KEEPALIVE", false) }

// KeepaliveIdle (SAPI_KEEPALIVE_IDLE_SEC, por defecto 5s): tiempo máximo de conexión ociosa.
func KeepaliveIdle() time.Duration {
	return envSeconds("SAPI_KEEPALIVE_IDLE_SEC", 5*time.Second)
}

// DNSCacheTTL (SAPI_DNS_CACHE_SEC, por defecto 60s; 0 = sin caché): tiempo que se guardan
// las resoluciones DNS de cloud.* y upload.* en memoria para evitar consultas repetitivas.
func DNSCacheTTL() time.Duration {
	v := strings.TrimSpace(os.Getenv("SAPI_DNS_CACHE_SEC"))
	f, err := strconv.ParseFloat(v, 64)
	if v == "" || err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 60 * time.Second
	}
	return time.Duration(f * float64(time.Second))
}

// LoginRetryFirstWait: primera espera ante fallo transitorio de login (30s).
func LoginRetryFirstWait() time.Duration {
	return envSeconds("SAPI_LOGIN_RETRY_FIRST_SEC", 30*time.Second)
}

// LoginRetryMaxWait: espera máxima ante fallo transitorio de login (600s / 10m).
func LoginRetryMaxWait() time.Duration {
	return envSeconds("SAPI_LOGIN_RETRY_MAX_SEC", 600*time.Second)
}

// LoginGiveUpRejections: rechazos de credenciales antes de declarar sesión expirada (4).
func LoginGiveUpRejections() int { return envInt("SAPI_LOGIN_GIVEUP_REJECTIONS", 4) }

// LoginGiveUpAfter: tiempo mínimo con rechazos antes de declarar sesión expirada (1800s / 30m).
func LoginGiveUpAfter() time.Duration {
	return envSeconds("SAPI_LOGIN_GIVEUP_SEC", 1800*time.Second)
}

func envBool(name string, def bool) bool {
	if v, ok := envBoolSet(name); ok {
		return v
	}
	return def
}

func envBoolSet(name string) (value, ok bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

func envSeconds(name string, def time.Duration) time.Duration {
	f, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return def
	}
	return time.Duration(f * float64(time.Second))
}
