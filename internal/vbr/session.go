// Package vbr habla con la REST API de Veeam Backup & Replication y guarda las
// sesiones. El resto de la app nunca ve una password ni un token: solo maneja
// un session_id opaco.
package vbr

import (
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
