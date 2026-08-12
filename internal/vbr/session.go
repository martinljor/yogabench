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

// Session es una conexion viva a un VBR (o una sesion demo).
type Session struct {
	Demo         bool
	Host         string
	Port         int
	APIVersion   string
	VerifySSL    bool
	AccessToken  string
	RefreshToken string
	CreatedAt    time.Time

	mu      sync.Mutex
	hostRes map[string]HostRes // hostId -> cores/ram (manual, opcional)
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
