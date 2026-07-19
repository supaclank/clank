package bridge

import (
	"fmt"
	"time"
)

// DeviceRecord is one approved phone in the registry — the bridge's
// authorized_keys line. PubKey (base64url Ed25519) is the identity;
// Name is cosmetic attribution, never an authorization input.
type DeviceRecord struct {
	PubKey   string     `json:"pubkey"`
	Name     string     `json:"name"`
	AddedAt  time.Time  `json:"added_at"`
	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// AddDevice approves a phone's public key, upserting by key: a
// re-approved device gets a fresh record (re-pairing is re-trust).
func (s *Store) AddDevice(pub []byte, name string) error {
	if len(pub) != KeyLen {
		return fmt.Errorf("bridge: device key must be %d bytes, got %d", KeyLen, len(pub))
	}
	rec := DeviceRecord{PubKey: EncodeKey(pub), Name: name, AddedAt: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.state.Devices {
		if d.PubKey == rec.PubKey {
			s.state.Devices[i] = rec
			return s.persistLocked()
		}
	}
	s.state.Devices = append(s.state.Devices, rec)
	return s.persistLocked()
}

// Device looks up an approved phone by public key.
func (s *Store) Device(pub []byte) (DeviceRecord, bool) {
	key := EncodeKey(pub)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.state.Devices {
		if d.PubKey == key {
			return d, true
		}
	}
	return DeviceRecord{}, false
}

// Devices returns the registry, pairing order preserved.
func (s *Store) Devices() []DeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DeviceRecord, len(s.state.Devices))
	copy(out, s.state.Devices)
	return out
}

// RemoveDevice revokes one phone. Reports whether the key was present.
func (s *Store) RemoveDevice(pub []byte) (bool, error) {
	key := EncodeKey(pub)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, d := range s.state.Devices {
		if d.PubKey == key {
			s.state.Devices = append(s.state.Devices[:i], s.state.Devices[i+1:]...)
			return true, s.persistLocked()
		}
	}
	return false, nil
}

// RemoveAllDevices revokes every phone (the host key stays — returning
// phones still recognize the laptop, they just have to re-pair).
func (s *Store) RemoveAllDevices() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.state.Devices)
	if n == 0 {
		return 0, nil
	}
	s.state.Devices = nil
	return n, s.persistLocked()
}

// TouchDevice bumps a device's last_seen. In-memory state is always
// current; disk writes are debounced (touchFlushInterval) because this
// runs on every authenticated request.
func (s *Store) TouchDevice(pub []byte) error {
	key := EncodeKey(pub)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.state.Devices {
		if s.state.Devices[i].PubKey != key {
			continue
		}
		s.state.Devices[i].LastSeen = &now
		if now.Sub(s.lastTouchFlush) < s.touchFlushInterval {
			return nil
		}
		s.lastTouchFlush = now
		return s.persistLocked()
	}
	return fmt.Errorf("bridge: touch unknown device %s", key)
}
