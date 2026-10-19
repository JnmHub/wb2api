package pool

// ProxySlot is a named outbound proxy accounts can be bound to.
type ProxySlot struct {
	ID      string
	URL     string
	Enabled bool
}

// ApplyProxySlots installs the slot table. An account bound to an enabled slot
// goes out through its URL; every other account uses its own legacy proxy
// string. Takes effect on the next request.
func (p *Pool) ApplyProxySlots(slots []ProxySlot) {
	m := make(map[string]string, len(slots))
	for _, s := range slots {
		if s.Enabled && s.URL != "" {
			m[s.ID] = s.URL
		}
	}
	p.slots.Store(&m)
	for _, a := range p.snap.Load().all {
		a.touch()
	}
}

// Proxy is the outbound proxy URL this account uses ("" = direct).
func (a *Account) Proxy() string {
	rec := a.Record()
	if rec.ProxySlot != "" {
		if p := a.owner.Load(); p != nil {
			if m := p.slots.Load(); m != nil {
				if u, ok := (*m)[rec.ProxySlot]; ok {
					return u
				}
			}
		}
	}
	return rec.ProxyLegacy
}
