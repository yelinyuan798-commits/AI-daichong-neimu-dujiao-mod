package merchantfulfillment_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	merchantapp "github.com/dujiao-next/internal/modules/merchantfulfillment/application"
	merchanthttp "github.com/dujiao-next/internal/modules/merchantfulfillment/transport/http"
	"github.com/gin-gonic/gin"
)

const testSecret = "public-fixture-secret-never-use-in-production-32bytes"

func router(service merchanthttp.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	merchanthttp.RegisterRoutes(r.Group("/api/v1"), service, merchanthttp.Config{Secret: testSecret, ShopID: "test-shop"})
	return r
}

func sign(req *http.Request, body []byte, nonce string, timestamp int64) {
	ts := fmt.Sprint(timestamp)
	hash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%x", req.Method, req.URL.RequestURI(), ts, nonce, hash)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(message))
	req.Header.Set("X-Fulfillment-Timestamp", ts)
	req.Header.Set("X-Fulfillment-Nonce", nonce)
	req.Header.Set("X-Fulfillment-Signature", fmt.Sprintf("%x", mac.Sum(nil)))
}

func request(r http.Handler, method, path string, body []byte, nonce string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	sign(req, body, nonce, time.Now().Unix())
	result := httptest.NewRecorder()
	r.ServeHTTP(result, req)
	return result
}

func TestSignedHTTPHealthListAndDelivery(t *testing.T) {
	f := setup(t)
	r := router(f.service)
	base := "/api/v1/merchant-fulfillment"
	health := request(r, "GET", base+"/health", nil, strings.Repeat("1", 32))
	if health.Code != 200 || !strings.Contains(health.Body.String(), `"protocol":"uecb-fulfillment-v1"`) {
		t.Fatal("health protocol failed")
	}
	if health.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private responses must not be cached")
	}
	list := request(r, "GET", base+"/orders?limit=1", nil, strings.Repeat("2", 32))
	if list.Code != 200 || !strings.Contains(list.Body.String(), `"next_cursor"`) {
		t.Fatal("signed query failed")
	}
	body, _ := json.Marshal(f.request())
	sent := request(r, "POST", base+"/deliveries", body, strings.Repeat("3", 32))
	if sent.Code != 200 || !strings.Contains(sent.Body.String(), `"status":"sent"`) {
		t.Fatalf("delivery failed: %s", sent.Body.String())
	}
	if strings.Contains(sent.Body.String(), "PUBLIC-TEST-CARD") {
		t.Fatal("HTTP response leaked card")
	}
	replayed := request(r, "POST", base+"/deliveries", body, strings.Repeat("4", 32))
	if replayed.Body.String() != sent.Body.String() {
		t.Fatal("different nonce should replay original receipt")
	}
	changed := f.request()
	changed.Amount = "0.01"
	body, _ = json.Marshal(changed)
	rejected := request(r, "POST", base+"/deliveries", body, strings.Repeat("5", 32))
	if rejected.Code != 200 || !strings.Contains(rejected.Body.String(), `"status":"failed"`) {
		t.Fatal("known rejection must be failed with HTTP 200")
	}
}

func TestSignatureRejectsReplayTamperingAndExpiredTimestamp(t *testing.T) {
	cases := []string{"query", "body", "method", "expired", "future", "signature", "replay"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			r := router(f.service)
			body := []byte(`{}`)
			req := httptest.NewRequest("POST", "/api/v1/merchant-fulfillment/deliveries?cursor=1", bytes.NewReader(body))
			ts := time.Now().Unix()
			if name == "expired" {
				ts -= 301
			}
			if name == "future" {
				ts += 301
			}
			sign(req, body, strings.Repeat("a", 32), ts)
			switch name {
			case "query":
				req.URL.RawQuery = "cursor=2"
			case "body":
				req.Body = http.NoBody
			case "method":
				req.Method = "GET"
				req.URL.Path = "/api/v1/merchant-fulfillment/health"
			case "signature":
				req.Header.Set("X-Fulfillment-Signature", strings.Repeat("0", 64))
			case "replay":
				first := httptest.NewRequest(req.Method, req.URL.String(), bytes.NewReader(body))
				first.Header = req.Header.Clone()
				r.ServeHTTP(httptest.NewRecorder(), first)
			}
			response := httptest.NewRecorder()
			r.ServeHTTP(response, req)
			if response.Code != 401 {
				t.Fatalf("expected unauthorized, got %d", response.Code)
			}
			if countDeliveries(t, f) != 0 {
				t.Fatal("invalid signature wrote delivery")
			}
		})
	}
}

func TestHTTPBodyLimitAndMalformedBodyDoNotEchoInput(t *testing.T) {
	f := setup(t)
	r := router(f.service)
	for i, body := range [][]byte{bytes.Repeat([]byte("x"), merchanthttp.MaxBodyBytes+1), []byte(`{"private":"PUBLIC-TEST-PRIVATE-MARKER"`), []byte(`{"private":"PUBLIC-TEST-PRIVATE-MARKER"}`)} {
		response := request(r, "POST", "/api/v1/merchant-fulfillment/deliveries", body, fmt.Sprintf("%032x", i+1))
		if response.Code != 400 && response.Code != 413 {
			t.Fatalf("expected safe input rejection: %d", response.Code)
		}
		if strings.Contains(response.Body.String(), "PUBLIC-TEST-PRIVATE-MARKER") {
			t.Fatal("request echoed")
		}
	}
	if countDeliveries(t, f) != 0 {
		t.Fatal("invalid body created delivery")
	}
}

type failingService struct{ merchanthttp.Service }

func (f failingService) Deliver(merchantapp.Delivery) (merchantapp.Receipt, error) {
	return merchantapp.Receipt{}, errors.New("database-private-internal-detail")
}

func TestStorageFailureIsUnknownWithoutInternalDetails(t *testing.T) {
	f := setup(t)
	r := router(failingService{Service: f.service})
	body, _ := json.Marshal(f.request())
	response := request(r, "POST", "/api/v1/merchant-fulfillment/deliveries", body, strings.Repeat("b", 32))
	if response.Code != 500 || !strings.Contains(response.Body.String(), `"status":"unknown"`) || strings.Contains(response.Body.String(), "database-private") {
		t.Fatal("ambiguous storage error was not safely represented")
	}
}

func TestBridgeEnvironmentRequiresBothIndependentSettings(t *testing.T) {
	t.Setenv("MERCHANT_FULFILLMENT_SECRET", "")
	t.Setenv("MERCHANT_FULFILLMENT_SHOP_ID", "")
	cfg, err := merchanthttp.ConfigFromEnv()
	if err != nil || cfg.Secret != "" {
		t.Fatal("unset bridge must stay disabled")
	}
	t.Setenv("MERCHANT_FULFILLMENT_SHOP_ID", "test-shop")
	if _, err := merchanthttp.ConfigFromEnv(); err == nil {
		t.Fatal("missing secret accepted")
	}
	t.Setenv("MERCHANT_FULFILLMENT_SECRET", "short")
	if _, err := merchanthttp.ConfigFromEnv(); err == nil {
		t.Fatal("short secret accepted")
	}
	t.Setenv("MERCHANT_FULFILLMENT_SECRET", testSecret)
	if _, err := merchanthttp.ConfigFromEnv(); err != nil {
		t.Fatal("valid settings rejected")
	}
	t.Setenv("MERCHANT_FULFILLMENT_SHOP_ID", testSecret)
	if _, err := merchanthttp.ConfigFromEnv(); err == nil {
		t.Fatal("secret must not appear in public shop identity")
	}
}
