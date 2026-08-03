package vbr

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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

func httpClient(verify bool) *http.Client {
	tr := &http.Transport{}
	if !verify { // VBR suele tener cert self-signed
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// Authenticate hace el OAuth2 password grant contra VBR.
func Authenticate(ctx context.Context, host string, port int, user, pass, apiVersion string, verify bool) (access, refresh string, expiresIn int, err error) {
	form := url.Values{"grant_type": {"password"}, "username": {user}, "password": {pass}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, baseURL(host, port)+"/oauth2/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("x-api-version", apiVersion)

	resp, e := httpClient(verify).Do(req)
	if e != nil {
		return "", "", 0, &APIError{502, fmt.Sprintf("No se pudo conectar a %s:%d - %v", host, port, e)}
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
		return "", "", 0, &APIError{502, "Respuesta de token no valida"}
	}
	return t.AccessToken, t.RefreshToken, t.ExpiresIn, nil
}

// Get hace un GET autenticado a la REST API (o devuelve datos demo). Devuelve
// el JSON crudo, listo para reenviar o parsear.
func Get(ctx context.Context, s *Session, path string) (json.RawMessage, error) {
	if s.Demo {
		return demoResponse(path), nil
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL(s.Host, s.Port)+"/"+path, nil)
	req.Header.Set("Authorization", "Bearer "+s.AccessToken)
	req.Header.Set("x-api-version", s.APIVersion)

	resp, e := httpClient(s.VerifySSL).Do(req)
	if e != nil {
		return nil, &APIError{504, fmt.Sprintf("Error consultando %s: %v", path, e)}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return nil, &APIError{401, "Token expirado. Reconectate."}
	}
	if resp.StatusCode != 200 {
		return nil, &APIError{resp.StatusCode, string(body)}
	}
	if !json.Valid(body) {
		return nil, &APIError{502, fmt.Sprintf("Respuesta no-JSON de %s", path)}
	}
	return json.RawMessage(body), nil
}
