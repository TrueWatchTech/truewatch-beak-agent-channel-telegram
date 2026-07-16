package telegram

import (
	"crypto/subtle"
	"fmt"
	"strings"
)

// VerifyWebhookSignature validates the X-Telegram-Bot-Api-Secret-Token header
// against the configured webhook secret using constant-time comparison.
func VerifyWebhookSignature(configuredSecret, headerToken string) error {
	if strings.TrimSpace(configuredSecret) == "" {
		return fmt.Errorf("telegram webhook secret is not configured")
	}
	if subtle.ConstantTimeCompare([]byte(configuredSecret), []byte(headerToken)) != 1 {
		return fmt.Errorf("telegram secret token mismatch")
	}
	return nil
}
