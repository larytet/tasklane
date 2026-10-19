package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/larytet/tasklane/internal/api"
	"github.com/larytet/tasklane/internal/config"
	"github.com/larytet/tasklane/internal/domain"
)

// reloadOpts are the options the server was started with; a reload resolves the configuration again with them.
var reloadOpts config.Options

func parseProxies(list []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range list {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		} else if ip := net.ParseIP(c); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		} else {
			return nil, fmt.Errorf("trusted_proxies: %q is not an address or a network", c)
		}
	}
	return out, nil
}

// applyReload takes up the hot settings of a new configuration and returns the message for the log. The caller has the new configuration
// from a Reloader, which has already refused an invalid one.
func applyReload(svc *domain.Service, srv *api.Server, cfg config.Config, res config.ReloadResult) (string, error) {
	proxies, err := parseProxies(cfg.TrustedProxies)
	if err != nil {
		return "", err // the new list is not usable: nothing is applied
	}
	svc.ApplyHot(domain.HotSettings{PasswordMinLength: int(cfg.PasswordMinLength), MailRawDays: cfg.MailRawDays, MailBounceDays: cfg.MailBounceDays, TrustedProxies: proxies})
	srv.SetMinClient(cfg.MinClient)
	msg := "configuration reloaded"
	if len(res.Applied) > 0 {
		msg += "; applied: " + strings.Join(res.Applied, ", ")
	} else {
		msg += "; nothing to apply"
	}
	if len(res.RestartOnly) > 0 {
		msg += "; changed but needing a restart: " + strings.Join(res.RestartOnly, ", ")
	}
	return msg, nil
}

// watchReload re-reads the configuration on SIGHUP. An invalid file is reported and the running settings stay.
func watchReload(svc *domain.Service, srv *api.Server) {
	rl, err := config.NewReloader(reloadOpts)
	if err != nil {
		log.Printf("reload on SIGHUP is not available: %v", err)
		return
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			cfg, res, err := rl.Reload()
			if err != nil {
				log.Printf("reload: %v", err)
				continue
			}
			msg, err := applyReload(svc, srv, cfg, res)
			if err != nil {
				log.Printf("reload: %v (the running settings are kept)", err)
				continue
			}
			log.Print(msg)
		}
	}()
}
