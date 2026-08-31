// Package vbr habla con la REST API de Veeam Backup & Replication y guarda las
// sesiones. El resto de la app nunca ve una password ni un token: solo maneja
// un session_id opaco.
package vbr

import (
	"encoding/json"
	"sync"
	"time"
)

// HostRes: recursos de performance de un host (ingresados manualmente por el
// usuario en prod, donde la REST no los expone). 0 = desconocido.
type HostRes struct {
	Cores int `json:"cores"`
	RamGB int `json:"ramGB"`
}

// Session es una conexion viva a un VBR (o una sesion demo). Los tokens se
// renuevan solos (ver renewToken en client.go): el usuario no tiene que
// reconectarse a los 25 minutos.
type Session struct {
	Demo       bool
	Host       string
	Port       int
	APIVersion string
	VerifySSL  bool
	CreatedAt  time.Time

	tokMu        sync.Mutex // protege los tokens (los GET corren en paralelo)
	renewMu      sync.Mutex // serializa la renovacion (una sola, no una por GET)
	accessToken  string
	refreshToken string
	expiresAt    time.Time

	mu      sync.Mutex
	hostRes map[string]HostRes // hostId -> cores/ram (manual, opcional)
	// analyzed: lo que el usuario YA analizo en esta sesion (con su veredicto),
	// para volcarlo en el diagnostico y poder reproducirlo/calibrarlo offline sin
	// gastar mas llamadas REST. Key: "job:<id>" | "assessment".
	analyzed map[string]any

	cacheMu sync.Mutex
	cache   map[string]cacheEntry

	// inflight: one request per path even if several parallel GETs hit the same
	// cache miss. On a loaded VBR v1/jobs takes 19 s, so paying it twice at the
	// same time is pure waste (seen in the field: two 19 s fetches back to back).
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall
}

// inflightCall: a fetch in progress that other callers can wait on.
type inflightCall struct {
	done chan struct{}
	body json.RawMessage
	err  error
}

// joinOrLead returns the call for this path and whether the caller must perform
// the fetch (leader) or just wait for it.
func (s *Session) joinOrLead(path string) (*inflightCall, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if c := s.inflight[path]; c != nil {
		return c, false
	}
	c := &inflightCall{done: make(chan struct{})}
	if s.inflight == nil {
		s.inflight = map[string]*inflightCall{}
	}
	s.inflight[path] = c
	return c, true
}

// leadDone publishes the result to everyone waiting on this path.
func (s *Session) leadDone(path string, c *inflightCall, body json.RawMessage, err error) {
	c.body, c.err = body, err
	s.inflightMu.Lock()
	delete(s.inflight, path)
	s.inflightMu.Unlock()
	close(c.done)
}

// cacheEntry: a GET response kept for a short while (see cacheable in client.go).
type cacheEntry struct {
	at   time.Time
	body json.RawMessage
}

// cacheGet returns a cached response if it is still fresh.
func (s *Session) cacheGet(path string) (json.RawMessage, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	e, ok := s.cache[path]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil, false
	}
	return e.body, true
}

func (s *Session) cachePut(path string, body json.RawMessage) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = map[string]cacheEntry{}
	}
	s.cache[path] = cacheEntry{at: time.Now(), body: body}
}

// SetAnalyzed guarda un resultado ya calculado (se sobreescribe por key).
func (s *Session) SetAnalyzed(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.analyzed == nil {
		s.analyzed = map[string]any{}
	}
	s.analyzed[key] = v
}

// AnalyzedAll devuelve una copia de lo analizado en la sesion.
func (s *Session) AnalyzedAll() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]any, len(s.analyzed))
	for k, v := range s.analyzed {
		out[k] = v
	}
	return out
}

// SetTokens guarda el par de tokens y cuando expira el access token.
// expiresIn<=0 = sin dato: asumimos una vida corta y refrescamos por 401.
func (s *Session) SetTokens(access, refresh string, expiresIn int) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.accessToken, s.refreshToken = access, refresh
	if expiresIn > 0 {
		s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	} else {
		s.expiresAt = time.Time{}
	}
}

// token devuelve el access token vigente y si conviene renovarlo ya (queda
// menos de un minuto de vida).
func (s *Session) token() (tok string, stale bool) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	return s.accessToken, !s.expiresAt.IsZero() && time.Now().After(s.expiresAt.Add(-time.Minute))
}

// SetHostRes guarda (o borra si cores y ram son 0) los recursos de un host.
func (s *Session) SetHostRes(hostID string, r HostRes) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hostRes == nil {
		s.hostRes = map[string]HostRes{}
	}
	if r.Cores == 0 && r.RamGB == 0 {
		delete(s.hostRes, hostID)
		return
	}
	s.hostRes[hostID] = r
}

// HostResAll devuelve una copia del mapa de recursos manuales.
func (s *Session) HostResAll() map[string]HostRes {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]HostRes, len(s.hostRes))
	for k, v := range s.hostRes {
		out[k] = v
	}
	return out
}

// Store guarda las sesiones en memoria (como el prototipo Python). En una
// version productiva esto iria a un vault/persistencia.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

// New guarda la sesion con un id aleatorio y lo devuelve.
func (st *Store) New(s *Session) string {
	id := randID()
	st.mu.Lock()
	st.sessions[id] = s
	st.mu.Unlock()
	return id
}

func (st *Store) Get(id string) (*Session, bool) {
	st.mu.RLock()
	s, ok := st.sessions[id]
	st.mu.RUnlock()
	return s, ok
}

func (st *Store) Delete(id string) {
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
}

func (st *Store) Count() int {
	st.mu.RLock()
	n := len(st.sessions)
	st.mu.RUnlock()
	return n
}
