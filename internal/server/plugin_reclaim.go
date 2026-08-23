package server

import "strings"

func hasBlockingPluginHolders(holders map[string]bool) bool {
	for holder := range holders {
		if !strings.HasPrefix(strings.TrimSpace(holder), "session:") {
			return true
		}
	}
	return false
}

func (r *Runtime) requestPluginReclaim(leaseKey, generation string) {
	if r == nil {
		return
	}
	if r.pluginLeases != nil && r.pluginLeases.RequestReclaim(leaseKey, generation) {
		return
	}
	if r.controllerLeases != nil {
		r.controllerLeases.RequestReclaim(leaseKey, generation)
	}
}

func (r *Runtime) cancelPluginReclaim(leaseKey, generation string) {
	if r == nil {
		return
	}
	if r.pluginLeases != nil && r.pluginLeases.CancelReclaim(leaseKey, generation) {
		return
	}
	if r.controllerLeases != nil {
		r.controllerLeases.CancelReclaim(leaseKey, generation)
	}
}

func (m *pluginRuntimeManager) RequestReclaim(leaseKey, generation string) bool {
	if m == nil {
		return false
	}
	leaseKey = strings.TrimSpace(leaseKey)
	generation = strings.TrimSpace(generation)
	m.mu.Lock()
	lease := m.leases[leaseKey]
	if lease == nil || lease.Generation != generation || lease.Closing {
		m.mu.Unlock()
		return false
	}
	lease.ReclaimRequested = true
	if hasBlockingPluginHolders(lease.Holders) {
		m.mu.Unlock()
		return true
	}
	lease.Closing = true
	delete(m.leases, leaseKey)
	m.mu.Unlock()
	m.closeLease(lease, "plugin_reclaim")
	return true
}

func (m *pluginRuntimeManager) CancelReclaim(leaseKey, generation string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	lease := m.leases[strings.TrimSpace(leaseKey)]
	if lease == nil || lease.Generation != strings.TrimSpace(generation) || lease.Closing {
		return false
	}
	lease.ReclaimRequested = false
	return true
}

func (m *controllerRuntimeManager) RequestReclaim(leaseKey, generation string) bool {
	if m == nil {
		return false
	}
	leaseKey = strings.TrimSpace(leaseKey)
	generation = strings.TrimSpace(generation)
	m.mu.Lock()
	lease := m.leases[leaseKey]
	if lease == nil || lease.Generation != generation {
		m.mu.Unlock()
		return false
	}
	lease.stateMu.Lock()
	lease.ReclaimRequested = true
	blocked := hasBlockingPluginHolders(lease.holders)
	lease.stateMu.Unlock()
	if blocked {
		m.mu.Unlock()
		return true
	}
	delete(m.leases, leaseKey)
	m.mu.Unlock()
	m.closeControllerLease(lease, "plugin_reclaim")
	return true
}

func (m *controllerRuntimeManager) CancelReclaim(leaseKey, generation string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	lease := m.leases[strings.TrimSpace(leaseKey)]
	m.mu.Unlock()
	if lease == nil || lease.Generation != strings.TrimSpace(generation) {
		return false
	}
	lease.stateMu.Lock()
	lease.ReclaimRequested = false
	lease.stateMu.Unlock()
	return true
}
