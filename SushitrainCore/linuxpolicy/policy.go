// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package linuxpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/protocol"
	"t-shaped.nl/sushitrain/v2/adminsock"
)

type Settings struct {
	ResourcePriority              string
	Metered, Cellular, PowerSaver bool
	Manual                        map[string]bool
}
type Environment struct{ NetworkKnown, CellularKnown, PowerKnown, Metered, Cellular, PowerSaver bool }
type Policy struct {
	config.Wrapper
	mu          sync.Mutex
	change      sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	path        string
	settings    Settings
	environment Environment
	reason      string
}

func Open(cfg config.Wrapper, path string) (*Policy, error) {
	p := &Policy{Wrapper: cfg, path: path, settings: Settings{Manual: map[string]bool{}}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &p.settings); err != nil {
			return nil, fmt.Errorf("connectivity policy is damaged: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if p.settings.Manual == nil {
		p.settings.Manual = map[string]bool{}
	}
	for _, d := range cfg.RawCopy().Devices {
		if d.DeviceID != cfg.MyID() {
			if _, ok := p.settings.Manual[d.DeviceID.String()]; !ok {
				p.settings.Manual[d.DeviceID.String()] = d.Paused
			}
		}
	}
	return p, nil
}
func (p *Policy) persistLocked() error {
	data, err := json.MarshalIndent(p.settings, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p.path), ".policy-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), p.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(p.path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func Reason(s Settings, e Environment) string {
	if s.Metered && !e.NetworkKnown || s.Cellular && !e.CellularKnown {
		return "Waiting for network policy status"
	}
	if s.PowerSaver && !e.PowerKnown {
		return "Waiting for power policy status"
	}
	if s.Metered && e.Metered {
		return "Paused on a metered network"
	}
	if s.Cellular && e.Cellular {
		return "Paused on a cellular network"
	}
	if s.PowerSaver && e.PowerSaver {
		return "Paused in Power Saver"
	}
	return ""
}
func (p *Policy) enforceLocked(c *config.Configuration) {
	p.reason = Reason(p.settings, p.environment)
	for i := range c.Devices {
		d := &c.Devices[i]
		if d.DeviceID != p.MyID() {
			d.Paused = p.settings.Manual[d.DeviceID.String()] || p.reason != ""
		}
	}
}

// Modify captures user choices from every configuration writer, including REST,
// before applying the temporary connectivity fence.
func (p *Policy) Modify(change config.ModifyFunction) (config.Waiter, error) {
	p.change.Lock()
	defer p.change.Unlock()
	var before config.Configuration
	var next map[string]bool
	waiter, err := p.Wrapper.Modify(func(c *config.Configuration) {
		p.mu.Lock()
		defer p.mu.Unlock()
		before = c.Copy()
		change(c)
		next = map[string]bool{}
		for k, v := range p.settings.Manual {
			next[k] = v
		}
		for _, d := range c.Devices {
			previous, _, known := before.Device(d.DeviceID)
			if !known || previous.Paused != d.Paused {
				next[d.DeviceID.String()] = d.Paused
			}
		}
		old := p.settings.Manual
		p.settings.Manual = next
		p.enforceLocked(c)
		p.settings.Manual = old
	})
	if err != nil {
		return waiter, err
	}
	waiter.Wait()
	p.mu.Lock()
	old := p.settings.Manual
	p.settings.Manual = next
	err = p.persistLocked()
	if err != nil {
		p.settings.Manual = old
	}
	p.mu.Unlock()
	if err != nil {
		rollback, e := p.Wrapper.Modify(func(c *config.Configuration) { *c = before })
		if e == nil {
			rollback.Wait()
		}
		return waiter, errors.Join(err, e)
	}
	return waiter, nil
}

func (p *Policy) apply() error {
	p.change.Lock()
	defer p.change.Unlock()
	return p.applyLocked()
}
func (p *Policy) applyLocked() error {
	waiter, err := p.Wrapper.Modify(func(c *config.Configuration) { p.mu.Lock(); defer p.mu.Unlock(); p.enforceLocked(c) })
	if err != nil {
		return err
	}
	waiter.Wait()
	return p.Wrapper.Save()
}
// SetDevicePause receives explicit REST Pause/Resume intent from the engine API.
// Configuration replacements continue through Modify, where echoed effective
// automatic pauses must not become manual pause requests.
func (p *Policy) SetDevicePause(devices []protocol.DeviceID, paused bool) error {
	ids := make([]string, 0, len(devices))
	for _, device := range devices {
		ids = append(ids, device.String())
	}
	return p.SetPaused(ids, paused)
}

func (p *Policy) SetPaused(ids []string, paused bool) error {
	p.change.Lock()
	defer p.change.Unlock()
	for _, id := range ids {
		parsed, err := protocol.DeviceIDFromString(id)
		if err != nil || parsed == p.MyID() {
			return errors.New("invalid remote device ID")
		}
		if _, ok := p.Wrapper.Device(parsed); !ok {
			return errors.New("the device no longer exists")
		}
	}
	p.mu.Lock()
	old := map[string]bool{}
	for k, v := range p.settings.Manual {
		old[k] = v
	}
	for _, id := range ids {
		p.settings.Manual[id] = paused
	}
	err := p.persistLocked()
	if err != nil {
		p.settings.Manual = old
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if err = p.applyLocked(); err != nil {
		p.mu.Lock()
		p.settings.Manual = old
		saveErr := p.persistLocked()
		p.mu.Unlock()
		return errors.Join(err, saveErr)
	}
	return nil
}

func (p *Policy) Register(a *adminsock.Server) {
	a.Register("policy.get", func(context.Context, json.RawMessage) (any, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return map[string]any{"Metered": p.settings.Metered, "Cellular": p.settings.Cellular, "PowerSaver": p.settings.PowerSaver, "Reason": p.reason, "NetworkKnown": p.environment.NetworkKnown, "CellularKnown": p.environment.CellularKnown, "PowerKnown": p.environment.PowerKnown, "ResourcePriority": p.settings.ResourcePriority}, nil
	})
	a.Register("resources.set", func(ctx context.Context, b json.RawMessage) (any, error) {
		p.change.Lock()
		defer p.change.Unlock()
		var v struct{ Priority string }
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		if err := SetResourcePriority(ctx, v.Priority); err != nil {
			return nil, err
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		old := p.settings.ResourcePriority
		p.settings.ResourcePriority = v.Priority
		if err := p.persistLocked(); err != nil {
			p.settings.ResourcePriority = old
			if old == "" {
				old = "normal"
			}
			return nil, errors.Join(err, SetResourcePriority(ctx, old))
		}
		return nil, nil
	})
	a.Register("policy.save", func(_ context.Context, b json.RawMessage) (any, error) {
		p.change.Lock()
		defer p.change.Unlock()
		var s Settings
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, err
		}
		p.mu.Lock()
		old := p.settings
		s.Manual = old.Manual
		s.ResourcePriority = old.ResourcePriority
		p.settings = s
		err := p.persistLocked()
		if err != nil {
			p.settings = old
		}
		p.mu.Unlock()
		if err == nil {
			err = p.applyLocked()
		}
		return nil, err
	})
}

// Start evaluates the initial state before the engine connects. The same daemon
// continues monitoring without any GTK process or signed-in graphical session.
func (p *Policy) Start(ctx context.Context) error {
	ctx, p.cancel = context.WithCancel(ctx)
	p.done = make(chan struct{})
	p.mu.Lock()
	priority := p.settings.ResourcePriority
	p.mu.Unlock()
	if priority != "" {
		if err := SetResourcePriority(ctx, priority); err != nil {
			slog.Warn("Could not restore service resource priority", "error", err)
		}
	}
	p.poll(ctx)
	if err := p.apply(); err != nil {
		p.cancel()
		close(p.done)
		return err
	}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if p.poll(ctx) {
					_ = p.apply()
				}
			}
		}
	}()
	return nil
}
func (p *Policy) poll(ctx context.Context) bool {
	e := ReadEnvironment(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := e != p.environment
	p.environment = e
	return changed
}
func ReadEnvironment(ctx context.Context) Environment {
	var e Environment
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := connectBus(ctx, dbus.SystemBusPrivate)
	if err != nil {
		return e
	}
	defer conn.Close()
	get := func(service, path, iface, property string) (dbus.Variant, error) {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		var v dbus.Variant
		err := conn.Object(service, dbus.ObjectPath(path)).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, iface, property).Store(&v)
		return v, err
	}
	if v, err := get("org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager", "org.freedesktop.NetworkManager", "Metered"); err == nil {
		n, ok := v.Value().(uint32)
		e.NetworkKnown = ok && n != 0
		e.Metered = n == 1 || n == 3
	}
	if v, err := get("org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager", "org.freedesktop.NetworkManager", "PrimaryConnectionType"); err == nil {
		kind, known := v.Value().(string)
		e.CellularKnown = known
		e.Cellular = kind == "gsm" || kind == "cdma"
	}
	for _, service := range []struct{ name, path, iface string }{{"org.freedesktop.UPower.PowerProfiles", "/org/freedesktop/UPower/PowerProfiles", "org.freedesktop.UPower.PowerProfiles"}, {"net.hadess.PowerProfiles", "/net/hadess/PowerProfiles", "net.hadess.PowerProfiles"}} {
		if v, err := get(service.name, service.path, service.iface, "ActiveProfile"); err == nil {
			profile, ok := v.Value().(string)
			e.PowerKnown = ok
			e.PowerSaver = profile == "power-saver"
			break
		}
	}
	return e
}

func (p *Policy) Stop() {
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}
