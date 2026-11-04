package merchanthttp

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const MaxBodyBytes = 256 * 1024

type Config struct {
	Secret string
	ShopID string
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var noncePattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)

func ConfigFromEnv() (Config, error) {
	cfg := Config{Secret: strings.TrimSpace(os.Getenv("MERCHANT_FULFILLMENT_SECRET")), ShopID: strings.TrimSpace(os.Getenv("MERCHANT_FULFILLMENT_SHOP_ID"))}
	if cfg.Secret == "" && cfg.ShopID == "" {
		return cfg, nil
	}
	if len([]byte(cfg.Secret)) < 32 || !identifier.MatchString(cfg.ShopID) || strings.Contains(cfg.ShopID, cfg.Secret) {
		return Config{}, errors.New("merchant fulfillment requires a 32-byte independent secret and a valid shop ID")
	}
	return cfg, nil
}

type authenticator struct {
	secret []byte
	mu     sync.Mutex
	nonces map[string]int64
}

func newAuthenticator(secret string) *authenticator {
	return &authenticator{secret: []byte(secret), nonces: map[string]int64{}}
}

func (a *authenticator) middleware(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	deny := func(code int) { c.AbortWithStatusJSON(code, gin.H{"status": "failed", "error": "request rejected"}) }
	if c.Request.ContentLength > MaxBodyBytes {
		deny(http.StatusRequestEntityTooLarge)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, MaxBodyBytes))
	if err != nil {
		deny(http.StatusRequestEntityTooLarge)
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(body))
	timestamp, nonce, signature := c.GetHeader("X-Fulfillment-Timestamp"), c.GetHeader("X-Fulfillment-Nonce"), c.GetHeader("X-Fulfillment-Signature")
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	now := time.Now().Unix()
	if err != nil || ts < now-300 || ts > now+300 || !noncePattern.MatchString(nonce) {
		deny(http.StatusUnauthorized)
		return
	}
	provided, err := hex.DecodeString(signature)
	if err != nil || len(provided) != sha256.Size {
		deny(http.StatusUnauthorized)
		return
	}
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%x", c.Request.Method, c.Request.URL.RequestURI(), timestamp, nonce, bodyHash)
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(message))
	if !hmac.Equal(provided, mac.Sum(nil)) {
		deny(http.StatusUnauthorized)
		return
	}
	a.mu.Lock()
	for key, expires := range a.nonces {
		if expires < now {
			delete(a.nonces, key)
		}
	}
	_, replayed := a.nonces[nonce]
	full := len(a.nonces) >= 10000
	if !replayed && !full {
		a.nonces[nonce] = ts + 300
	}
	a.mu.Unlock()
	if replayed {
		deny(http.StatusUnauthorized)
		return
	}
	if full {
		deny(http.StatusServiceUnavailable)
		return
	}
	c.Next()
}
