package config

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"os"
	"strings"
)

type AuthConfig struct {
	Username     string
	Password     string
	PasswordHash string
	AuthDisabled bool
	RequireAuth  bool
}

func LoadWebUIAuthConfig() AuthConfig {
	user := strings.TrimSpace(os.Getenv("WEBUI_USER"))
	pass := os.Getenv("WEBUI_PASSWORD")
	hash := strings.TrimSpace(os.Getenv("WEBUI_PASSWORD_HASH"))
	disabled := strings.EqualFold(strings.TrimSpace(os.Getenv("AUTH_DISABLED")), "true")
	requireAuth := strings.EqualFold(strings.TrimSpace(os.Getenv("REQUIRE_AUTH")), "true")

	return AuthConfig{
		Username:     user,
		Password:     pass,
		PasswordHash: strings.TrimPrefix(strings.ToLower(hash), "sha256:"),
		AuthDisabled: disabled,
		RequireAuth:  requireAuth,
	}
}

func (a AuthConfig) HasCredentials() bool {
	return a.Username != "" && (a.Password != "" || a.PasswordHash != "")
}

func (a AuthConfig) IsEnabled() bool {
	if a.AuthDisabled {
		return false
	}
	return a.HasCredentials()
}

func (a AuthConfig) Validate(user, pass string) bool {
	if !a.IsEnabled() {
		return true
	}

	cleanUser := user
	if !strings.Contains(a.Username, `\`) {
		if idx := strings.LastIndex(cleanUser, `\`); idx != -1 {
			cleanUser = cleanUser[idx+1:]
		}
	}
	if !strings.Contains(a.Username, "@") {
		if idx := strings.Index(cleanUser, "@"); idx != -1 {
			cleanUser = cleanUser[:idx]
		}
	}

	userHash := sha256.Sum256([]byte(cleanUser))
	expectedUserHash := sha256.Sum256([]byte(a.Username))
	userMatch := subtle.ConstantTimeCompare(userHash[:], expectedUserHash[:]) == 1

	var passMatch bool
	if a.PasswordHash != "" {
		expectedHashBytes, err := hex.DecodeString(a.PasswordHash)
		if err == nil && len(expectedHashBytes) == 32 {
			passHash := sha256.Sum256([]byte(pass))
			passMatch = subtle.ConstantTimeCompare(passHash[:], expectedHashBytes) == 1
		}
	} else {
		passHash := sha256.Sum256([]byte(pass))
		expectedPassHash := sha256.Sum256([]byte(a.Password))
		passMatch = subtle.ConstantTimeCompare(passHash[:], expectedPassHash[:]) == 1
	}

	return userMatch && passMatch
}
