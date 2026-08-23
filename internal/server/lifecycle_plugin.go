package server

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"time"
)

const defaultPluginShutdownGrace = 1500 * time.Millisecond

type lifecyclePluginRegistration struct {
	LeaseKey   string
	Plugin     string
	Generation string
}

type lifecyclePluginPeer struct {
	LeaseKey   string
	Plugin     string
	Generation string
	Token      string
	conn       net.Conn
	encoder    *json.Encoder
	writeMu    sync.Mutex
	done       chan struct{}
	doneOnce   sync.Once
}

func (p *lifecyclePluginPeer) send(value any) error {
	if p == nil || p.encoder == nil {
		return errors.New("Plugin lifecycle peer is unavailable")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.encoder.Encode(value)
}

func (p *lifecyclePluginPeer) closeDone() {
	if p == nil {
		return
	}
	p.doneOnce.Do(func() { close(p.done) })
}

func (m *lifecycleManager) PreparePluginPeer(leaseKey, pluginName, generation string) (socketPath, token string, err error) {
	if m == nil {
		return "", "", nil
	}
	leaseKey = strings.TrimSpace(leaseKey)
	pluginName = strings.TrimSpace(pluginName)
	generation = strings.TrimSpace(generation)
	if leaseKey == "" || pluginName == "" || generation == "" {
		return "", "", errors.New("Plugin lifecycle lease_key, plugin and generation are required")
	}
	m.mu.Lock()
	if m.listener == nil || m.socketPath == "" {
		m.mu.Unlock()
		return "", "", nil
	}
	if m.closing {
		m.mu.Unlock()
		return "", "", errors.New("MCPX lifecycle is shutting down")
	}
	socketPath = m.socketPath
	m.mu.Unlock()

	token, err = newLifecycleID("pt")
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closing {
		return "", "", errors.New("MCPX lifecycle is shutting down")
	}
	m.pluginRegistrations[token] = lifecyclePluginRegistration{LeaseKey: leaseKey, Plugin: pluginName, Generation: generation}
	return socketPath, token, nil
}

func (m *lifecycleManager) CancelPluginPeer(token string) {
	if m == nil || strings.TrimSpace(token) == "" {
		return
	}
	token = strings.TrimSpace(token)
	m.mu.Lock()
	delete(m.pluginRegistrations, token)
	var peer *lifecyclePluginPeer
	for _, candidate := range m.pluginPeers {
		if candidate != nil && candidate.Token == token {
			peer = candidate
			break
		}
	}
	m.mu.Unlock()
	if peer != nil && peer.conn != nil {
		_ = peer.conn.Close()
	}
}

func (m *lifecycleManager) registerPluginPeer(token, leaseKey, pluginName, generation string, conn net.Conn, encoder *json.Encoder) (*lifecyclePluginPeer, error) {
	token = strings.TrimSpace(token)
	leaseKey = strings.TrimSpace(leaseKey)
	pluginName = strings.TrimSpace(pluginName)
	generation = strings.TrimSpace(generation)
	m.mu.Lock()
	defer m.mu.Unlock()
	registration, ok := m.pluginRegistrations[token]
	if !ok || registration.LeaseKey != leaseKey || registration.Plugin != pluginName || registration.Generation != generation {
		return nil, errors.New("invalid Plugin lifecycle registration")
	}
	if existing := m.pluginPeers[leaseKey]; existing != nil {
		select {
		case <-existing.done:
			delete(m.pluginPeers, leaseKey)
		default:
			return nil, errors.New("Plugin lifecycle peer is already connected")
		}
	}
	peer := &lifecyclePluginPeer{
		LeaseKey: leaseKey, Plugin: pluginName, Generation: generation, Token: token,
		conn: conn, encoder: encoder, done: make(chan struct{}),
	}
	m.pluginPeers[leaseKey] = peer
	return peer, nil
}

func (m *lifecycleManager) unregisterPluginPeer(peer *lifecyclePluginPeer) {
	if m == nil || peer == nil {
		return
	}
	m.mu.Lock()
	if m.pluginPeers[peer.LeaseKey] == peer {
		delete(m.pluginPeers, peer.LeaseKey)
	}
	m.mu.Unlock()
	peer.closeDone()
}

func (m *lifecycleManager) RequestPluginShutdown(leaseKey, reason string, grace time.Duration) bool {
	if m == nil || strings.TrimSpace(leaseKey) == "" {
		return false
	}
	if grace <= 0 {
		grace = defaultPluginShutdownGrace
	}
	m.mu.Lock()
	peer := m.pluginPeers[strings.TrimSpace(leaseKey)]
	m.mu.Unlock()
	if peer == nil {
		return false
	}
	deadline := time.Now().Add(grace)
	if peer.conn != nil {
		_ = peer.conn.SetWriteDeadline(deadline)
	}
	err := peer.send(map[string]any{
		"type": "plugin.shutdown", "plugin": peer.Plugin, "lease_key": peer.LeaseKey, "generation": peer.Generation,
		"reason": strings.TrimSpace(reason),
	})
	if peer.conn != nil {
		_ = peer.conn.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		_ = peer.conn.Close()
		return false
	}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-peer.done:
		return true
	case <-timer.C:
		return false
	}
}
