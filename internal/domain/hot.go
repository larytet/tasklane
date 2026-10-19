package domain

import "net"

// HotSettings are the settings that can change while the server runs (SIGHUP re-reads the configuration file; see config.Reloader). Everything
// else needs a restart, and a reload says so.
type HotSettings struct {
	PasswordMinLength           int
	MailRawDays, MailBounceDays int
	TrustedProxies              []*net.IPNet
}

// ApplyHot replaces the hot settings; readers see either the old set or the new one for each value.
func (s *Service) ApplyHot(h HotSettings) {
	s.hotMu.Lock()
	s.PasswordMinLength, s.MailRawDays, s.MailBounceDays, s.TrustedProxies = h.PasswordMinLength, h.MailRawDays, h.MailBounceDays, h.TrustedProxies
	s.hotMu.Unlock()
}

func (s *Service) trustedProxies() []*net.IPNet {
	s.hotMu.RLock()
	defer s.hotMu.RUnlock()
	return s.TrustedProxies
}
