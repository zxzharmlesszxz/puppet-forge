package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

var testableRandomBase64URL = func(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("secure random generation failed: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func randomBase64URL(size int) (string, error) {
	if size < 0 {
		return "", fmt.Errorf("random token size must be non-negative: %d", size)
	}
	return testableRandomBase64URL(size)
}
