package vbr

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"yogabench/internal/dbg"
)

// APIError transporta un status HTTP + mensaje para devolver claro al frontend.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

func randID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func baseURL(host string, port int) string {
	return fmt.Sprintf("https://%s:%d/api", host, port)
}

// getTimeout: a loaded VBR answers v1/jobs in 9-12 s and sometimes never; hanging
// the UI for a minute on each attempt is worse than failing and degrading (the
// analysis works without the job list, and the job selector falls back to the
// session history). Kept well above the 12 s observed in the field.
const getTimeout = 30 * time.Second

// jobsTimeout: v1/jobs is the one endpoint that legitimately takes long on a
// loaded VBR — measured in the field: 6.6 s quiet, 18.5 s busy, and over 30 s
// under load (six timeouts in a row). Everything that depends on it (topology
// edges, the job selector, per-job analysis) degrades without it, so it gets a
// bigger budget than the rest.
const jobsTimeout = 75 * time.Second

func budgetFor(path string) time.Duration {
	if strings.HasPrefix(path, "v1/jobs") {
		return jobsTimeout
	}
	return getTimeout
}

func httpClient(verify bool) *http.Client {
	tr := &http.Transport{}
	if !verify { // VBR suele tener cert self-signed
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr} // backstop; per-request budget in doGet
}

// Authenticate hace el OAuth2 password grant contra VBR.
func Authenticate(ctx context.Context, host string, port int, user, pass, apiVersion string, verify bool) (access, refresh string, expiresIn int, err error) {
	return tokenGrant(ctx, host, port, apiVersion, verify,
		url.Values{"grant_type": {"password"}, "username": {user}, "password": {pass}})
}

// tokenGrant hace un POST a /oauth2/token (password o refresh_token grant).
func tokenGrant(ctx context.Context, host string, port int, apiVersion string, verify bool, form url.Values) (access, refresh string, expiresIn int, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL(host, port)+"/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-api-version", apiVersion)

	resp, e := httpClient(verify).Do(req)
	if e != nil {
		return "", "", 0, &APIError{502, fmt.Sprintf("Could not connect to %s:%d - %v", host, port, e)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// Reenviamos el detalle real de Veeam (ej: version de API no soportada).
		return "", "", 0, &APIError{resp.StatusCode, string(body)}
	}
	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", "", 0, &APIError{502, "Invalid token response"}
	}
	return t.AccessToken, t.RefreshToken, t.ExpiresIn, nil
}

// renewToken renueva el access token con el refresh_token, una sola vez aunque
// varios GET en paralelo se topen con el mismo 401. used = el token que fallo:
// si otra goroutine ya renovo, no repetimos.
func renewToken(ctx context.Context, s *Session, used string) error {
	s.renewMu.Lock()
	defer s.renewMu.Unlock()
	if cur, _ := s.token(); cur != used {
		return nil // ya lo renovo otra request
	}
	s.tokMu.Lock()
	rt := s.refreshToken
	s.tokMu.Unlock()
	if rt == "" {
		return &APIError{401, "Session expired. Reconnect."}
	}
	access, refresh, expiresIn, err := tokenGrant(ctx, s.Host, s.Port, s.APIVersion, s.VerifySSL,
		url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}})
	if err != nil {
		log.Printf("REST: token renewal failed: %v", err) // no token
		return &APIError{401, "Session expired. Reconnect."}
	}
	s.SetTokens(access, refresh, expiresIn)
	log.Printf("REST: access token renewed (valid %ds)", expiresIn) // no token
	return nil
}

// cacheTTL / cacheable: infrastructure and job configuration change slowly, and a
// single click asks for them several times (the analysis, the topology and the
// recommendations all need the job list). On a loaded VBR `v1/jobs` can take 12 s
// or time out, so paying it once per minute instead of once per caller is the
// difference between a usable UI and a hung one. Sessions are deliberately NOT
// cached: they change on every run and the analysis depends on them being fresh.
const cacheTTL = 60 * time.Second

// staleTTL: how old a cached copy may be and still serve as a fallback when the
// fresh fetch times out.
const staleTTL = 30 * time.Minute

func cacheable(path string) bool {
	return strings.HasPrefix(path, "v1/jobs") || strings.HasPrefix(path, "v1/backupInfrastructure/")
}

// Get hace un GET autenticado a la REST API (o devuelve datos demo). Renueva el
// token solo (antes de que venza, y si igual sale 401 reintenta una vez).
// Devuelve el JSON crudo, listo para reenviar o parsear.
func Get(ctx context.Context, s *Session, path string) (json.RawMessage, error) {
	if s.Demo {
		return demoResponse(path), nil
	}
	if !cacheable(path) {
		return fetchOnce(ctx, s, path)
	}
	if body, ok := s.cacheGet(path); ok {
		dbg.Logf("GET %s -> cached (%dB)", path, len(body))
		return body, nil
	}
	// Single-flight: si otra request ya esta trayendo este path, esperamos su
	// resultado en vez de pedirlo de nuevo (v1/jobs tarda 19 s en un VBR cargado).
	call, lead := s.joinOrLead(path)
	if !lead {
		dbg.Logf("GET %s -> waiting for the in-flight request", path)
		<-call.done
		return call.body, call.err
	}
	body, err := fetchOnce(ctx, s, path)
	if err == nil {
		s.cachePut(path, body)
	} else if stale, ok := s.cacheGetStale(path); ok {
		// The fetch failed but an older copy exists: job/infrastructure config
		// changes slowly, so data from minutes ago beats an empty diagram and an
		// empty job selector.
		log.Printf("REST GET %s: failed (%v) — serving the previous copy", path, err)
		body, err = stale, nil
	}
	s.leadDone(path, call, body, err)
	return body, err
}

// fetchOnce: un GET con renovacion de token y un reintento ante 401.
func fetchOnce(ctx context.Context, s *Session, path string) (json.RawMessage, error) {
	tok, stale := s.token()
	if stale { // esta por vencer: lo renovamos antes de gastar el request
		if err := renewToken(ctx, s, tok); err != nil {
			return nil, err
		}
		tok, _ = s.token()
	}
	body, status, err := doGet(ctx, s, path, tok)
	if err != nil {
		return nil, err
	}
	if status == 401 { // venció antes de lo previsto: renovar y reintentar
		if e := renewToken(ctx, s, tok); e != nil {
			return nil, e
		}
		if tok2, _ := s.token(); tok2 != tok {
			body, status, err = doGet(ctx, s, path, tok2)
			if err != nil {
				return nil, err
			}
		}
	}
	if status == 401 {
		log.Printf("REST GET %s: HTTP 401 after renewal", path)
		return nil, &APIError{401, "Session expired. Reconnect."}
	}
	if status != 200 {
		log.Printf("REST GET %s: HTTP %d: %s", path, status, strings.TrimSpace(string(body)))
		return nil, &APIError{status, string(body)}
	}
	if !json.Valid(body) {
		log.Printf("REST GET %s: non-JSON response", path)
		return nil, &APIError{502, fmt.Sprintf("Non-JSON response from %s", path)}
	}
	return json.RawMessage(body), nil
}

// doGet: un intento de GET con el token dado. Devuelve el body y el status.
func doGet(ctx context.Context, s *Session, path, token string) ([]byte, int, error) {
	// Per-attempt budget: the retry after a token renewal gets a fresh one.
	budget := budgetFor(path)
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(s.Host, s.Port)+"/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-api-version", s.APIVersion)

	start := time.Now()
	resp, e := httpClient(s.VerifySSL).Do(req)
	if e != nil {
		if ctx.Err() == context.DeadlineExceeded {
			log.Printf("REST GET %s: timed out after %s (the VBR is loaded)", path, budget)
			return nil, 0, &APIError{504, fmt.Sprintf("%s timed out after %s: the VBR did not answer in time.", path, budget)}
		}
		log.Printf("REST GET %s: no response: %v", path, e)
		return nil, 0, &APIError{504, fmt.Sprintf("Error querying %s: %v", path, e)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	dbg.Logf("GET %s -> %d (%dms, %dB)", path, resp.StatusCode, time.Since(start).Milliseconds(), len(body))
	return body, resp.StatusCode, nil
}
