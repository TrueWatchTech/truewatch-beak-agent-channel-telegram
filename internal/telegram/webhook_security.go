package telegram

import (
	"crypto/subtle"
	"fmt"
	"strings"
)

// VerifyWebhookSignature validates the X-Telegram-Bot-Api-Secret-Token header
// against the configured webhook secret using constant-time comparison.
// If configuredSecret is empty, any request is accepted (secret is optional).
func VerifyWebhookSignature(configuredSecret, headerToken string) error {
	if strings.TrimSpace(configuredSecret) == "" {
		// No secret configured — skip verification.
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(configuredSecret), []byte(headerToken)) != 1 {
		return fmt.Errorf("telegram secret token mismatch")
	}
	return nil
}
