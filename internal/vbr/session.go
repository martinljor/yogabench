// Package vbr habla con la REST API de Veeam Backup & Replication y guarda las
// sesiones. El resto de la app nunca ve una password ni un token: solo maneja
// un session_id opaco.
package vbr

import (
	"sync"
	"time"
)

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
