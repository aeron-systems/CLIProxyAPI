package management

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

const testSessionSecret = "0123456789abcdef0123456789abcdef"

func testAuth0Config(allowed ...string) config.ManagementAuth0 {
	return config.ManagementAuth0{
		Domain:        "tenant.example.com",
		ClientID:      "client-1",
		ClientSecret:  "shh",
		SessionSecret: testSessionSecret,
		AllowedEmails: allowed,
	}
}

func TestResolveAuth0SettingsRequiresFullConfig(t *testing.T) {
	if resolveAuth0Settings(testAuth0Config("a@b.com")) == nil {
		t.Fatal("expected complete config to resolve")
	}
	short := testAuth0Config("a@b.com")
	short.SessionSecret = "too-short"
	if resolveAuth0Settings(short) != nil {
		t.Fatal("expected short session secret to disable sign-in")
	}
	if resolveAuth0Settings(testAuth0Config()) != nil {
		t.Fatal("expected empty allow-list to disable sign-in")
	}
}

func TestSessionTokenRoundTrip(t *testing.T) {
	s := resolveAuth0Settings(testAuth0Config("Aarun@Example.com"))
	now := time.Now()
	token, err := s.issueSession("aarun@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	if email, ok := s.verifySession(token, now); !ok || email != "aarun@example.com" {
		t.Fatalf("expected valid session, got %q %v", email, ok)
	}
	if _, ok := s.verifySession(token, now.Add(13*time.Hour)); ok {
		t.Fatal("expected expired session to be refused")
	}
	if _, ok := s.verifySession(token[:len(token)-2]+"xx", now); ok {
		t.Fatal("expected tampered signature to be refused")
	}
	forged, _ := json.Marshal(sessionPayload{Email: "aarun@example.com", Exp: now.Add(time.Hour).Unix()})
	if _, ok := s.verifySession(sessionTokenPrefix+base64.RawURLEncoding.EncodeToString(forged)+".bad", now); ok {
		t.Fatal("expected unsigned payload to be refused")
	}
	removed := resolveAuth0Settings(testAuth0Config("someone@else.com"))
	if _, ok := removed.verifySession(token, now); ok {
		t.Fatal("expected session to stop working once the email leaves the allow-list")
	}
}

func TestAuthenticateManagementKeyAcceptsSessionToken(t *testing.T) {
	cfg := &config.Config{}
	cfg.RemoteManagement.AllowRemote = true
	cfg.RemoteManagement.Auth0 = testAuth0Config("aarun@example.com")
	h := &Handler{cfg: cfg, failedAttempts: make(map[string]*attemptInfo), envSecret: "real-key"}

	token, _ := h.auth0Config().issueSession("aarun@example.com", time.Now())
	if ok, status, msg := h.AuthenticateManagementKey("100.64.0.9", false, token); !ok {
		t.Fatalf("expected session token to be accepted: %d %s", status, msg)
	}
	if ok, _, _ := h.AuthenticateManagementKey("100.64.0.9", false, sessionTokenPrefix+"nope.nope"); ok {
		t.Fatal("expected bad session token to be refused")
	}
	if ok, _, _ := h.AuthenticateManagementKey("100.64.0.9", false, "real-key"); !ok {
		t.Fatal("expected management key to keep working")
	}
}

func TestAuth0LoginRedirectsWithPKCE(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.RemoteManagement.Auth0 = testAuth0Config("aarun@example.com")
	h := &Handler{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "https://proxy.example.com/v0/management/auth0/login", nil)
	h.GetAuth0Login(c)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected redirect, got %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Host != "tenant.example.com" || loc.Path != "/authorize" {
		t.Fatalf("unexpected redirect %q", rec.Header().Get("Location"))
	}
	q := loc.Query()
	if q.Get("redirect_uri") != "https://proxy.example.com/v0/management/auth0/callback" || q.Get("code_challenge_method") != "S256" || q.Get("state") == "" {
		t.Fatalf("unexpected authorize params: %v", q)
	}
	if !strings.Contains(rec.Header().Get("Set-Cookie"), auth0FlowCookie+"=") {
		t.Fatal("expected flow cookie")
	}
}

func TestAuth0CallbackRejectsMissingFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.RemoteManagement.Auth0 = testAuth0Config("aarun@example.com")
	h := &Handler{cfg: cfg}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "https://proxy.example.com/v0/management/auth0/callback?code=x&state=y", nil)
	h.GetAuth0Callback(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without flow cookie, got %d", rec.Code)
	}
}
