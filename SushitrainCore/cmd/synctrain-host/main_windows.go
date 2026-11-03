// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"golang.org/x/sys/windows"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"t-shaped.nl/sushitrain/v2/adminsock"
	core "t-shaped.nl/sushitrain/v2/src"
	"t-shaped.nl/sushitrain/v2/winhost"
	"t-shaped.nl/sushitrain/v2/winplatform"
)

func main() {
	if err := run(); err != nil {
		slog.Error("Synctrain stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sid, err := winplatform.UserSID()
	if err != nil {
		return err
	}
	state := flag.String("state", filepath.Join(os.Getenv("LOCALAPPDATA"), "Synctrain"), "durable state directory")
	root := flag.String("root", filepath.Join(home, "Synctrain"), "Cloud Files location (NTFS)")
	pipe := flag.String("pipe", `\\.\pipe\Synctrain-`+sid, "private administration pipe")
	gate := flag.String("gate", "", "supervisor startup gate")
	worker := flag.Bool("worker", false, "run the supervised worker")
	call := flag.String("call", "", "call an administration method and print JSON")
	params := flag.String("params", "{}", "JSON parameters for --call")
	flag.Parse()
	if *call != "" {
		var out json.RawMessage
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		target := *pipe
		if *call == "quit" {
			if err := adminsock.CallContext(ctx, target+"-Supervisor", *call, json.RawMessage(*params), &out); err == nil {
				fmt.Println(string(out))
				return nil
			}
		}
		if err := adminsock.CallContext(ctx, *pipe, *call, json.RawMessage(*params), &out); err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	if err := os.MkdirAll(*state, 0700); err != nil {
		return err
	}
	if err := winplatform.PrivateDirectory(*state); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(*state, "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	slog.SetDefault(slog.New(slog.NewTextHandler(log, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if !*worker {
		lock := flock.New(filepath.Join(*state, "supervisor.lock"))
		ok, err := lock.TryLock()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		defer lock.Unlock()
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		control := adminsock.NewServer()
		control.Register("quit", func(context.Context, json.RawMessage) (any, error) {
			go func() { time.Sleep(100 * time.Millisecond); cancel() }()
			return nil, nil
		})
		if err := control.Listen(ctx, *pipe+"-Supervisor", false); err != nil {
			return err
		}
		defer control.Close()
		var crashes []time.Time
		for {
			err = supervise(ctx, exe, *state, *root, *pipe, log)
			if err == nil || ctx.Err() != nil {
				return nil
			}
			now := time.Now()
			recent := crashes[:0]
			for _, t := range crashes {
				if now.Sub(t) < time.Hour {
					recent = append(recent, t)
				}
			}
			crashes = append(recent, now)
			if len(crashes) > 5 {
				return fmt.Errorf("stopped restarting after five crashes in an hour: %w", err)
			}
			wait := time.Duration(10<<uint(len(crashes)-1)) * time.Second
			slog.Error("Restarting after a crash", "after", wait, "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
		}
	}
	// Do not start a backend or children until the supervisor owns the worker's job.
	if *gate != "" {
		name, err := windows.UTF16PtrFromString(*gate)
		if err != nil {
			return err
		}
		event, err := windows.OpenEvent(windows.SYNCHRONIZE, false, name)
		if err != nil {
			return err
		}
		result, err := windows.WaitForSingleObject(event, 15000)
		windows.CloseHandle(event)
		if err != nil {
			return err
		}
		if result != windows.WAIT_OBJECT_0 {
			return fmt.Errorf("supervisor did not release startup gate: %d", result)
		}
	}

	executable, err := os.Executable()
	if err != nil {
		return err
	}
	core.RegisterTrashHandler(recycleHandler{provider: filepath.Join(filepath.Dir(executable), "Synctrain.Provider.exe")})
	host, err := winhost.Open(*state, *root, home)
	if err != nil {
		return err
	}
	defer host.Close()
	server := adminsock.NewServer()
	host.Register(server)
	server.Register("quit", func(context.Context, json.RawMessage) (any, error) {
		go func() { time.Sleep(100 * time.Millisecond); cancel() }()
		return nil, nil
	})
	defer server.Close()
	provider := &providerProcess{state: *state, root: *root, pipe: *pipe, output: log, failed: make(chan error, 1)}
	defer provider.stop()
	host.OnModeChange = provider.change
	host.BeforeModeChange = provider.quiesce
	host.AfterModeChange = provider.resume
	host.OnFatalError = func(err error) {
		select {
		case provider.failed <- err:
		default:
		}
	}

	if err := host.InitializeProvider(func() error { return server.Listen(ctx, *pipe, false) }); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-provider.failed:
		return err
	}
}

// A job per worker makes a crashed worker and all of its children one lifetime.
// The unnamed, non-inheritable job handle belongs only to the supervisor.
func supervise(ctx context.Context, exe, state, root, pipe string, output *os.File) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(job)
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return err
	}
	guid, err := windows.GenerateGUID()
	if err != nil {
		return err
	}
	name := `Local\Synctrain-start-` + guid.String()
	eventName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	event, err := windows.CreateEvent(nil, 1, 0, eventName)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(event)
	cmd := exec.Command(exe, "--worker", "--gate", name, "--state", state, "--root", root, "--pipe", pipe)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, process)
		windows.CloseHandle(process)
	}
	if err == nil {
		err = windows.SetEvent(event)
	}
	if err != nil {
		_ = cmd.Process.Kill()
		<-done
		return err
	}
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		quit, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = adminsock.CallContext(quit, pipe, "quit", nil, nil)
		cancel()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
		}
		_ = windows.TerminateJobObject(job, 1)
		<-done
		return nil
	}
}

type providerRun struct {
	cmd         *exec.Cmd
	done        chan struct{}
	err         error
	intentional atomic.Bool
	failure     sync.Once
}
type providerProcess struct {
	mu                sync.Mutex
	state, root, pipe string
	output            *os.File
	run               *providerRun
	failed            chan error
}

func (p *providerProcess) stopLocked() {
	if p.run == nil {
		return
	}
	p.run.intentional.Store(true)
	_ = p.run.cmd.Process.Kill()
	<-p.run.done
	p.run = nil
}
func (p *providerProcess) stop() { p.mu.Lock(); defer p.mu.Unlock(); p.stopLocked() }
func (p *providerProcess) publishFailure(run *providerRun, err error) {
	run.failure.Do(func() {
		select {
		case p.failed <- err:
		default:
		}
	})
}
func (p *providerProcess) change(mode, clientID, root string) (changeErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.root = root
	if p.run != nil && mode == "" {
		run := p.run
		defer func() {
			if changeErr != nil {
				run.intentional.Store(false)
				select {
				case <-run.done:
					p.publishFailure(run, fmt.Errorf("native ownership was lost during aborted detach: %w", changeErr))
				default:
				}
			}
		}()
		path := filepath.Join(p.state, "provider.detach")
		_ = os.Remove(path + ".error")
		if err := os.WriteFile(path, nil, 0600); err != nil {
			return err
		}
		defer os.Remove(path)
		// This is an intentional provider exit, not a crash to restart.
		p.run.intentional.Store(true)
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-p.run.done:
				err := p.run.err
				p.run = nil
				if err != nil {
					return fmt.Errorf("detaching the Files location: %w", err)
				}
				return nil
			case <-timer.C:
				p.run.intentional.Store(false)
				return errors.New("the Files location could not detach within 30 seconds")
			case <-tick.C:
				if data, err := os.ReadFile(path + ".error"); err == nil {
					p.run.intentional.Store(false)
					return fmt.Errorf("the Files location stays connected: %s", data)
				}
			}
		}
	}
	p.stopLocked()
	if mode != "files" {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ready := filepath.Join(p.state, "provider.ready")
	for _, suffix := range []string{"", ".ready", ".error"} {
		_ = os.Remove(filepath.Join(p.state, "provider.quiesce") + suffix)
	}
	if err := os.Remove(ready); err != nil && !os.IsNotExist(err) {
		return err
	}
	run := &providerRun{cmd: exec.Command(filepath.Join(filepath.Dir(exe), "Synctrain.Provider.exe"), "--root", p.root, "--pipe", p.pipe, "--ready", ready, "--client", clientID, "--quiesce", filepath.Join(p.state, "provider.quiesce"), "--detach", filepath.Join(p.state, "provider.detach"), "--approval", filepath.Join(p.state, "provider-clean.json")), done: make(chan struct{})}
	run.cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	run.cmd.Stdout, run.cmd.Stderr = p.output, p.output
	if err := run.cmd.Start(); err != nil {
		return err
	}
	p.run = run
	go func() {
		run.err = run.cmd.Wait()
		close(run.done)
		if !run.intentional.Load() {
			p.publishFailure(run, fmt.Errorf("Cloud Files provider exited unexpectedly: %v", run.err))
		}
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-run.done:
			p.run = nil
			return fmt.Errorf("Cloud Files provider failed to start: %v", run.err)
		case <-timer.C:
			p.stopLocked()
			return fmt.Errorf("Cloud Files provider did not become ready; see %s", filepath.Join(p.state, "host.log"))
		case <-tick.C:
			if _, err := os.Stat(ready); err == nil {
				return nil
			}
		}
	}
}

// Quiesce while the old backend still admits RPC. Captures can hydrate through
// the host, and the subsequent mode drain includes every completed capture.
func (p *providerProcess) quiesce(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.run == nil {
		return nil
	}
	path := filepath.Join(p.state, "provider.quiesce")
	_ = os.Remove(path + ".ready")
	_ = os.Remove(path + ".error")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		return err
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.Remove(path)
		}
	}()
	timer := time.NewTimer(50 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("Explorer could not finish capturing local changes; see host.log")
		case <-p.run.done:
			return fmt.Errorf("provider stopped while capturing changes: %v", p.run.err)
		case <-tick.C:
			if data, err := os.ReadFile(path + ".error"); err == nil {
				return fmt.Errorf("Explorer could not capture local changes: %s", data)
			}
			if _, err := os.Stat(path + ".ready"); err == nil {
				succeeded = true
				return nil
			}
		}
	}
}

func (p *providerProcess) resume() {
	for _, suffix := range []string{"", ".ready", ".error"} {
		_ = os.Remove(filepath.Join(p.state, "provider.quiesce") + suffix)
	}
}

type recycleHandler struct{ provider string }

func (r recycleHandler) Trash(path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, r.provider, "--recycle", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("moving to Recycle Bin: %w: %s", err, output)
	}
	return nil
}
