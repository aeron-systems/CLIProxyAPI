package management

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// Auth0 sign-in for the management panel.
//
// The panel sends a session token in place of the management key. The token is
// "sso1.<payload>.<signature>", where payload is base64url JSON {email, exp} and
// signature is HMAC-SHA256 over "sso1.<payload>" with the session secret.

const (
	sessionTokenPrefix  = "sso1."
	auth0FlowCookie     = "cpa_auth0_flow"
	auth0CookiePath     = "/v0/management/auth0"
	auth0CallbackPath   = "/v0/management/auth0/callback"
	auth0FlowTTL        = 10 * time.Minute
	defaultSessionHours = 12
)

var auth0HTTPClient = &http.Client{Timeout: 15 * time.Second}

type auth0Settings struct {
	domain        string
	clientID      string
	clientSecret  string
	sessionSecret []byte
	sessionTTL    time.Duration
	allowed       map[string]struct{}
}

// auth0Config returns the effective Auth0 settings, or nil when sign-in is not fully configured.
func (h *Handler) auth0Config() *auth0Settings {
	if h == nil || h.cfg == nil {
		return nil
	}
	return resolveAuth0Settings(h.cfg.RemoteManagement.Auth0)
}

func resolveAuth0Settings(c config.ManagementAuth0) *auth0Settings {
	s := &auth0Settings{
		domain:       strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(c.Domain), "https://"), "/"),
		clientID:     strings.TrimSpace(c.ClientID),
		clientSecret: strings.TrimSpace(c.ClientSecret),
		allowed:      make(map[string]struct{}, len(c.AllowedEmails)),
	}
	if v := strings.TrimSpace(os.Getenv("MANAGEMENT_AUTH0_CLIENT_SECRET")); v != "" {
		s.clientSecret = v
	}
	secret := strings.TrimSpace(c.SessionSecret)
	if v := strings.TrimSpace(os.Getenv("MANAGEMENT_SESSION_SECRET")); v != "" {
		secret = v
	}
	s.sessionSecret = []byte(secret)
	hours := c.SessionHours
	if hours <= 0 {
		hours = defaultSessionHours
	}
	s.sessionTTL = time.Duration(hours) * time.Hour
	for _, e := range c.AllowedEmails {
		if e = strings.ToLower(strings.TrimSpace(e)); e != "" {
			s.allowed[e] = struct{}{}
		}
	}
	// A short session secret would make tokens guessable; refuse to run with one.
	if s.domain == "" || s.clientID == "" || s.clientSecret == "" || len(s.sessionSecret) < 32 || len(s.allowed) == 0 {
		return nil
	}
	return s
}

func (s *auth0Settings) sign(data string) string {
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write([]byte(data))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

type sessionPayload struct {
	Email string `json:"email"`
	Exp   int64  `json:"exp"`
}

func (s *auth0Settings) issueSession(email string, now time.Time) (string, error) {
	raw, err := json.Marshal(sessionPayload{Email: email, Exp: now.Add(s.sessionTTL).Unix()})
	if err != nil {
		return "", err
	}
	body := sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + s.sign(body), nil
}

// verifySession returns the signed-in email for a valid, unexpired session token
// whose email is still on the allow-list.
func (s *auth0Settings) verifySession(token string, now time.Time) (string, bool) {
	if s == nil || !strings.HasPrefix(token, sessionTokenPrefix) {
		return "", false
	}
	dot := strings.LastIndexByte(token, '.')
	if dot <= len(sessionTokenPrefix) {
		return "", false
	}
	body, sig := token[:dot], token[dot+1:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign(body))) != 1 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body[len(sessionTokenPrefix):])
	if err != nil {
		return "", false
	}
	var p sessionPayload
	if err = json.Unmarshal(raw, &p); err != nil || now.Unix() >= p.Exp {
		return "", false
	}
	if _, ok := s.allowed[strings.ToLower(p.Email)]; !ok {
		return "", false
	}
	return p.Email, true
}

// isSessionToken reports whether the provided credential is a valid Auth0 session token.
func (h *Handler) isSessionToken(provided string) bool {
	_, ok := h.auth0Config().verifySession(provided, time.Now())
	return ok
}

type auth0Flow struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Redirect string `json:"r"`
	Exp      int64  `json:"e"`
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func requestOrigin(c *gin.Context) string {
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	if p := c.GetHeader("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := c.Request.Host
	if fh := c.GetHeader("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return scheme + "://" + host
}

func setFlowCookie(c *gin.Context, value string, maxAge int) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     auth0FlowCookie,
		Value:    value,
		Path:     auth0CookiePath,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(requestOrigin(c), "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// GetAuth0Status tells the panel whether to offer "Sign in with Auth0".
func (h *Handler) GetAuth0Status(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"enabled": h.auth0Config() != nil})
}

// GetAuth0Login starts the Auth0 authorization-code flow (with PKCE).
func (h *Handler) GetAuth0Login(c *gin.Context) {
	s := h.auth0Config()
	if s == nil {
		c.String(http.StatusNotFound, "Auth0 sign-in is not configured")
		return
	}
	state, err1 := randomToken()
	verifier, err2 := randomToken()
	nonce, err3 := randomToken()
	if err1 != nil || err2 != nil || err3 != nil {
		c.String(http.StatusInternalServerError, "could not start sign-in")
		return
	}
	redirectURI := requestOrigin(c) + auth0CallbackPath
	flow := auth0Flow{State: state, Verifier: verifier, Nonce: nonce, Redirect: redirectURI, Exp: time.Now().Add(auth0FlowTTL).Unix()}
	raw, _ := json.Marshal(flow)
	body := base64.RawURLEncoding.EncodeToString(raw)
	setFlowCookie(c, body+"."+s.sign("flow."+body), int(auth0FlowTTL.Seconds()))

	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {s.clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
	}
	c.Redirect(http.StatusFound, "https://"+s.domain+"/authorize?"+q.Encode())
}

func (s *auth0Settings) readFlow(c *gin.Context) (*auth0Flow, bool) {
	cookie, err := c.Cookie(auth0FlowCookie)
	if err != nil {
		return nil, false
	}
	dot := strings.LastIndexByte(cookie, '.')
	if dot <= 0 {
		return nil, false
	}
	body, sig := cookie[:dot], cookie[dot+1:]
	if subtle.ConstantTimeCompare([]byte(sig), []byte(s.sign("flow."+body))) != 1 {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil, false
	}
	var f auth0Flow
	if err = json.Unmarshal(raw, &f); err != nil || time.Now().Unix() >= f.Exp {
		return nil, false
	}
	return &f, true
}

type idTokenClaims struct {
	Iss           string          `json:"iss"`
	Aud           json.RawMessage `json:"aud"`
	Exp           int64           `json:"exp"`
	Nonce         string          `json:"nonce"`
	Email         string          `json:"email"`
	EmailVerified bool            `json:"email_verified"`
}

func (cl *idTokenClaims) hasAudience(clientID string) bool {
	var one string
	if json.Unmarshal(cl.Aud, &one) == nil {
		return one == clientID
	}
	var many []string
	if json.Unmarshal(cl.Aud, &many) == nil {
		for _, a := range many {
			if a == clientID {
				return true
			}
		}
	}
	return false
}

// exchangeCode trades the authorization code for an ID token and returns its claims.
// The ID token comes straight from the token endpoint over TLS, so per OIDC Core
// 3.1.3.7 the TLS server check stands in for verifying its signature.
func (s *auth0Settings) exchangeCode(c *gin.Context, code string, flow *auth0Flow) (*idTokenClaims, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {s.clientID},
		"client_secret": {s.clientSecret},
		"code":          {code},
		"code_verifier": {flow.Verifier},
		"redirect_uri":  {flow.Redirect},
	}
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://"+s.domain+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := auth0HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err = json.Unmarshal(raw, &tok); err != nil || tok.IDToken == "" {
		return nil, fmt.Errorf("no id_token in token response")
	}
	parts := strings.Split(tok.IDToken, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("malformed id_token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed id_token payload")
	}
	var claims idTokenClaims
	if err = json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("malformed id_token claims")
	}
	return &claims, nil
}

// GetAuth0Callback finishes sign-in and hands the panel a session token in the URL fragment,
// which browsers never send to servers.
func (h *Handler) GetAuth0Callback(c *gin.Context) {
	s := h.auth0Config()
	if s == nil {
		c.String(http.StatusNotFound, "Auth0 sign-in is not configured")
		return
	}
	deny := func(status int, msg string) {
		setFlowCookie(c, "", -1)
		c.String(status, msg)
	}
	if e := c.Query("error"); e != "" {
		deny(http.StatusUnauthorized, "Auth0 sign-in failed: "+c.Query("error_description"))
		return
	}
	flow, ok := s.readFlow(c)
	if !ok || subtle.ConstantTimeCompare([]byte(c.Query("state")), []byte(flow.State)) != 1 {
		deny(http.StatusBadRequest, "Sign-in expired or was started elsewhere. Go back to the panel and try again.")
		return
	}
	claims, err := s.exchangeCode(c, c.Query("code"), flow)
	if err != nil {
		log.Warnf("management auth0: code exchange failed: %v", err)
		deny(http.StatusBadGateway, "Could not complete Auth0 sign-in.")
		return
	}
	now := time.Now()
	if claims.Iss != "https://"+s.domain+"/" || !claims.hasAudience(s.clientID) || now.Unix() >= claims.Exp ||
		subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(flow.Nonce)) != 1 {
		log.Warnf("management auth0: id_token failed validation (iss=%q)", claims.Iss)
		deny(http.StatusUnauthorized, "Auth0 sign-in could not be verified.")
		return
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if _, allowed := s.allowed[email]; !allowed || !claims.EmailVerified {
		log.Warnf("management auth0: sign-in refused for %q", email)
		deny(http.StatusForbidden, "This account is not allowed to manage the model proxy.")
		return
	}
	token, err := s.issueSession(email, now)
	if err != nil {
		deny(http.StatusInternalServerError, "could not create session")
		return
	}
	log.Infof("management auth0: %s signed in", email)
	setFlowCookie(c, "", -1)
	c.Header("Cache-Control", "no-store")
	// The panel uses a hash router, so the token lands in its /login route's query.
	c.Redirect(http.StatusFound, "/management.html#/login?sso="+url.QueryEscape(token))
}
