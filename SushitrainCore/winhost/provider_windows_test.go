// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0
package winhost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unsafe"

	"github.com/google/uuid"
	"t-shaped.nl/sushitrain/v2/adminsock"
	"t-shaped.nl/sushitrain/v2/files"
	"t-shaped.nl/sushitrain/v2/gateway/gatewaytest"
)

// This test uses the production provider and host RPC, with the shared real
// gateway fixture. Only shell registration is replaced by direct CFAPI registration
// so an unpackaged test executable can own an isolated temporary sync root.
func TestCloudFilesRoundTrip(t *testing.T) {
	provider := os.Getenv("SYNCTRAIN_TEST_PROVIDER")
	if provider == "" {
		t.Fatal("SYNCTRAIN_TEST_PROVIDER must name the built native provider")
	}
	engine := gatewaytest.NewEngine(t, "docs", nil)
	original := bytes.Repeat([]byte("Windows hydration range verification\n"), 90000)
	engine.Write("original.txt", string(original))
	var photo bytes.Buffer
	if err := png.Encode(&photo, image.NewRGBA(image.Rect(0, 0, 800, 600))); err != nil {
		t.Fatal(err)
	}
	engine.Write("photo.png", photo.String())
	engine.Write("survivor/readonly.txt", "retained through unregister")
	g := gatewaytest.NewGateway(t, engine, "")
	g.Export(true)
	base := t.TempDir()
	state := filepath.Join(base, "state")
	root := filepath.Join(base, "cloud")
	h, err := Open(state, root, base)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	s, err := files.Open(files.Config{StateDir: filepath.Join(state, "Files"), CacheDir: filepath.Join(state, "Cache"), Credentials: &files.FileCredentials{Path: filepath.Join(state, "test-credentials.json")}, AllowLocalGateway: true, CacheBudget: 1})
	if err != nil {
		t.Fatal(err)
	}
	s.SetInvalidator(func(files.Changes) { h.revision.Add(1) })
	h.files = s
	h.settings.Mode = "files"
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	paired := make(chan error, 1)
	var serviceID string
	go func() {
		gateway, err := s.Pair(ctx, g.PairingLink(), nil)
		serviceID = gateway.ServiceID
		paired <- err
	}()
	g.ApproveNext(map[string]string{"docs": "write"})
	if err := <-paired; err != nil {
		t.Fatal(err)
	}
	folder, err := s.AddFolder(ctx, serviceID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	pipe := `\\.\pipe\Synctrain-Test-` + uuid.NewString()
	server := adminsock.NewServer()
	h.Register(server)
	var blockCapture atomic.Bool
	var replayCapture atomic.Bool
	var caseCollisions atomic.Bool
	var collisionDeletes atomic.Int64
	server.Register("files.children", func(ctx context.Context, data json.RawMessage) (any, error) {
		var params request
		if err := json.Unmarshal(data, &params); err != nil {
			return nil, err
		}
		result, err := h.call(ctx, "files.children", params)
		if err != nil || !caseCollisions.Load() {
			return result, err
		}
		// Model a case-sensitive Mac/Linux gateway on this case-insensitive
		// Windows fixture. Conflicts arrive before the already projected names.
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		var entries []map[string]any
		if err := json.Unmarshal(encoded, &entries); err != nil {
			return nil, err
		}
		var collisions []map[string]any
		for _, entry := range entries {
			name, _ := entry["Name"].(string)
			if name != "photo.png" && name != "survivor" {
				continue
			}
			duplicate := make(map[string]any, len(entry))
			for key, value := range entry {
				duplicate[key] = value
			}
			duplicate["Name"] = strings.ToUpper(name[:1]) + name[1:]
			duplicate["Item"] = entry["Item"].(float64) + 1000000
			duplicate["Node"] = entry["Node"].(float64) + 1000000
			collisions = append(collisions, duplicate)
		}
		return append(collisions, entries...), nil
	})
	server.Register("files.delete", func(ctx context.Context, data json.RawMessage) (any, error) {
		var params request
		if err := json.Unmarshal(data, &params); err != nil {
			return nil, err
		}
		if caseCollisions.Load() {
			collisionDeletes.Add(1)
		}
		return h.call(ctx, "files.delete", params)
	})
	captureEntered, releaseCapture := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-releaseCapture:
		default:
			close(releaseCapture)
		}
	}()
	server.Register("files.stage", func(ctx context.Context, data json.RawMessage) (any, error) {
		var params request
		if err := json.Unmarshal(data, &params); err != nil {
			return nil, err
		}
		if params.Name == "conflict.txt" && blockCapture.CompareAndSwap(true, false) {
			close(captureEntered)
			select {
			case <-releaseCapture:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if params.Name == "replay.txt" && replayCapture.CompareAndSwap(true, false) {
			accepted, err := h.stage(ctx, params)
			if err != nil {
				return nil, err
			}
			if err := s.Wait(ctx, accepted.Entry.Item); err != nil {
				return nil, err
			}
			// Model a committed capture whose response was lost, followed by a
			// remote deletion before the provider retries the same receipt.
			if err := os.Remove(filepath.Join(engine.FolderPath, "replay.txt")); err != nil {
				return nil, err
			}
			g.Sync()
			if err := s.Refresh(ctx, folder.ID); err != nil {
				return nil, err
			}
			return h.stage(ctx, params)
		}
		return h.call(ctx, "files.stage", params)
	})
	if err := server.Listen(ctx, pipe, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	id, err := s.ClientIdentity()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(base, "ready")
	shutdown := filepath.Join(base, "shutdown")
	logPath := filepath.Join(base, "provider.log")
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(provider, "--root", root, "--pipe", pipe, "--client", id, "--ready", ready, "--test", "true", "--shutdown", shutdown, "--quiesce", filepath.Join(base, "quiesce"), "--detach", filepath.Join(base, "detach"), "--approval", filepath.Join(state, "provider-clean.json"))
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	providerExited := false
	go func() { exited <- cmd.Wait() }()
	defer func() {
		if !providerExited {
			_ = os.WriteFile(shutdown, nil, 0600)
			select {
			case err := <-exited:
				if err != nil {
					t.Errorf("provider exit: %v", err)
				}
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
				t.Error("provider did not stop within five seconds")
			}
		}
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("Native provider log:\n%s", data)
			checkpoint, _ := os.ReadFile(filepath.Join(state, "projection.json"))
			t.Logf("Checkpoint: %s", checkpoint)
			captures, _ := os.ReadDir(filepath.Join(state, "Captured"))
			t.Logf("Captures: %v", captures)
			edits, editErr := s.Edits()
			t.Logf("Pending edits: %+v (%v)", edits, editErr)
			remote, _ := os.ReadDir(engine.FolderPath)
			for _, entry := range remote {
				if strings.HasPrefix(entry.Name(), "replay") {
					data, err := os.ReadFile(filepath.Join(engine.FolderPath, entry.Name()))
					t.Logf("Remote replay file %q: %q (%v)", entry.Name(), data, err)
				}
			}
		}
	}()
	wait := func(what string, condition func() bool) {
		t.Helper()
		gatewaytest.WaitFor(t, 8*time.Second, what, condition)
	}
	wait("provider readiness", func() bool { _, err := os.Stat(ready); return err == nil })
	projected := filepath.Join(root, folder.DirName, "original.txt")
	wait("initial projection", func() bool { _, err := os.Stat(projected); return err == nil })
	read := make(chan error, 1)
	go func() {
		data, err := os.ReadFile(projected)
		if err == nil && !bytes.Equal(data, original) {
			err = fmt.Errorf("hydrated %d bytes, expected %d", len(data), len(original))
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Explorer hydration did not complete within eight seconds")
	}
	t.Log("Hydration verified through native CFAPI callbacks")
	wait("native cache budget dehydration", func() bool { size, _, err := nativePlaceholderStorage(projected); return err == nil && size == 0 })
	entry, err := s.Resolve(filepath.ToSlash(filepath.Join(folder.DirName, "original.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(ctx, "files.keep", request{Item: entry.Item, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wait("kept file hydrated beyond budget", func() bool {
		size, pin, err := nativePlaceholderStorage(projected)
		return err == nil && size >= int64(len(original)) && pin == 1
	})
	if _, err := h.call(ctx, "files.keep", request{Item: entry.Item, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCacheBudget(files.DefaultCacheBudget); err != nil {
		t.Fatal(err)
	}

	imagePath := filepath.Join(root, folder.DirName, "photo.png")
	wait("image placeholder", func() bool { _, err := os.Stat(imagePath); return err == nil })
	probeCtx, cancelProbe := context.WithTimeout(ctx, 8*time.Second)
	// CommandContext owns cancellation; the native probe exercises the actual
	// COM thumbnail implementation and WIC conversion without package activation.
	probe := exec.CommandContext(probeCtx, provider, "--probe-thumbnail", imagePath, "--pipe", pipe)
	output, probeErr := probe.CombinedOutput()
	cancelProbe()
	if probeErr != nil {
		t.Fatalf("thumbnail handler: %v %s", probeErr, output)
	}
	t.Logf("Native thumbnail handler: %s", output)
	var collisionEntries []files.Entry
	for _, name := range []string{"photo.png", "survivor", "survivor/readonly.txt"} {
		entry, err := s.Resolve(folder.DirName + "/" + name)
		if err != nil {
			t.Fatal(err)
		}
		collisionEntries = append(collisionEntries, entry)
	}
	caseCollisions.Store(true)
	h.revision.Add(1)
	wait("case collisions reported without replacing existing items", func() bool {
		var issues []json.RawMessage
		return json.Unmarshal(h.projectionIssues(), &issues) == nil && len(issues) == 2
	})
	for i, name := range []string{"photo.png", "survivor", "survivor/readonly.txt"} {
		actual, err := nativePlaceholderEntry(filepath.Join(root, folder.DirName, filepath.FromSlash(name)))
		if err != nil || actual.Item != collisionEntries[i].Item || actual.Node != collisionEntries[i].Node {
			t.Fatalf("case collision changed %s identity: %+v %v", name, actual, err)
		}
	}
	if collisionDeletes.Load() != 0 {
		t.Fatalf("case collision generated %d remote deletions", collisionDeletes.Load())
	}
	caseCollisions.Store(false)
	h.revision.Add(1)
	wait("case collision report clears after resolution", func() bool { return string(h.projectionIssues()) == "[]" })
	created := filepath.Join(root, folder.DirName, "created.txt")
	if err := os.WriteFile(created, []byte("created in Explorer"), 0600); err != nil {
		t.Fatal(err)
	}
	wait("native creation reaching gateway", func() bool {
		data, _ := os.ReadFile(filepath.Join(engine.FolderPath, "created.txt"))
		return string(data) == "created in Explorer"
	})
	if err := os.WriteFile(created, []byte("edited in Explorer"), 0600); err != nil {
		t.Fatal(err)
	}
	wait("native edit reaching gateway", func() bool {
		data, _ := os.ReadFile(filepath.Join(engine.FolderPath, "created.txt"))
		return string(data) == "edited in Explorer"
	})
	preserved, err := os.Stat(created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created, []byte("second in Explorer"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(created, preserved.ModTime(), preserved.ModTime()); err != nil {
		t.Fatal(err)
	}
	wait("same-size edit with preserved timestamp", func() bool {
		data, _ := os.ReadFile(filepath.Join(engine.FolderPath, "created.txt"))
		return string(data) == "second in Explorer"
	})
	renamed := filepath.Join(root, folder.DirName, "renamed.txt")
	if err := os.Rename(created, renamed); err != nil {
		t.Fatal(err)
	}
	wait("native rename reaching gateway", func() bool {
		data, _ := os.ReadFile(filepath.Join(engine.FolderPath, "renamed.txt"))
		return string(data) == "second in Explorer"
	})
	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	wait("native delete reaching gateway", func() bool {
		_, err := os.Stat(filepath.Join(engine.FolderPath, "renamed.txt"))
		return os.IsNotExist(err)
	})
	originalEntry, err := s.Resolve(filepath.ToSlash(filepath.Join(folder.DirName, "original.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(ctx, "files.keep", request{Item: originalEntry.Item, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wait("pin before remote update", func() bool { _, pin, err := nativePlaceholderStorage(projected); return err == nil && pin == 1 })
	engine.Write("original.txt", "changed remotely")
	g.Sync()
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	wait("remote edit refreshing placeholder", func() bool {
		data, err := os.ReadFile(projected)
		return err == nil && string(data) == "changed remotely"
	})
	wait("remote update retaining native and shared pin", func() bool {
		_, pin, err := nativePlaceholderStorage(projected)
		entry, ok, getErr := s.Item(originalEntry.Item)
		return err == nil && ok && getErr == nil && pin == 1 && entry.Keep
	})
	// Keep a read-only materialized child through real unregistration and an
	// offline parent rename. Recovery must preserve both content and identity.
	survivorPath := filepath.Join(root, folder.DirName, "survivor", "readonly.txt")
	survivorEntry, err := s.Resolve(filepath.ToSlash(filepath.Join(folder.DirName, "survivor", "readonly.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.call(ctx, "files.keep", request{Item: survivorEntry.Item, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	wait("materialized recovery child", func() bool {
		data, err := os.ReadFile(survivorPath)
		_, pin, pinErr := nativePlaceholderStorage(survivorPath)
		return err == nil && pinErr == nil && pin == 1 && string(data) == "retained through unregister"
	})
	if err := os.Chmod(survivorPath, 0444); err != nil {
		t.Fatal(err)
	}
	// Restart the real provider with its durable checkpoint and existing root.
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-exited
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	// Model package unregistration dropping a dehydrated placeholder. Its
	// absence must be repopulated, never submitted as a remote deletion.
	photoPath := filepath.Join(root, folder.DirName, "photo.png")
	encodedRoot, err := windows.UTF16PtrFromString(root)
	if err != nil {
		t.Fatal(err)
	}
	unregister := windows.NewLazySystemDLL("CldApi.dll").NewProc("CfUnregisterSyncRoot")
	result, _, _ := unregister.Call(uintptr(unsafe.Pointer(encodedRoot)))
	if int32(result) < 0 {
		t.Fatalf("unregister: 0x%x", result)
	}
	if err := os.Remove(photoPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	newParent := filepath.Join(root, folder.DirName, "new-parent")
	if err := os.Mkdir(newParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, folder.DirName, "survivor"), filepath.Join(newParent, "offline-renamed")); err != nil {
		t.Fatal(err)
	}
	// Fault fixture: creation was checkpointed immediately before its RPC was
	// interrupted. Its real surviving NTFS directory still has no item identity.
	pendingDirectory := filepath.Join(root, folder.DirName, "pending-directory")
	if err := os.Mkdir(pendingDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	encodedDirectory, err := windows.UTF16PtrFromString(pendingDirectory)
	if err != nil {
		t.Fatal(err)
	}
	directoryHandle, err := windows.CreateFile(encodedDirectory, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(directoryHandle, &info)
	windows.CloseHandle(directoryHandle)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(state, "projection.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint []map[string]any
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		t.Fatal(err)
	}
	rootEntry, err := s.Resolve(folder.DirName)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = append(checkpoint, map[string]any{"Entry": map[string]any{"Kind": 1, "ClientID": id}, "Path": folder.DirName + "/pending-directory", "Key": fmt.Sprintf("%d-%d", info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow)), "Stamp": "", "USN": "0", "Create": map[string]any{"Parent": rootEntry.Item, "Name": "pending-directory", "Directory": true, "ID": uuid.NewString(), "ClientID": id}})
	data, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.writeAtomic("projection.json", data); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string(nil), cmd.Args[1:]...), "--recover-registration", "true")
	cmd = exec.Command(provider, args...)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited = make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	wait("provider restart", func() bool { _, err := os.Stat(ready); return err == nil })
	wait("package cleanup repopulated without remote deletion", func() bool {
		_, localErr := os.Stat(photoPath)
		_, remoteErr := os.Stat(filepath.Join(engine.FolderPath, "photo.png"))
		return localErr == nil && remoteErr == nil
	})
	wait("read-only survivor and offline parent rename recovered", func() bool {
		data, err := os.ReadFile(filepath.Join(engine.FolderPath, "new-parent", "offline-renamed", "readonly.txt"))
		_, oldErr := os.Stat(filepath.Join(engine.FolderPath, "survivor"))
		return err == nil && string(data) == "retained through unregister" && os.IsNotExist(oldErr)
	})
	wait("interrupted directory creation recovered", func() bool {
		info, err := os.Stat(filepath.Join(engine.FolderPath, "pending-directory"))
		return err == nil && info.IsDir()
	})
	wait("pin preserved across provider restart", func() bool {
		_, pin, err := nativePlaceholderStorage(projected)
		entry, ok, getErr := s.Item(originalEntry.Item)
		return err == nil && ok && getErr == nil && pin == 1 && entry.Keep
	})
	// Remote renames and deletions are reflected in the Explorer tree.
	if err := os.Rename(filepath.Join(engine.FolderPath, "original.txt"), filepath.Join(engine.FolderPath, "remote-renamed.txt")); err != nil {
		t.Fatal(err)
	}
	g.Sync()
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	remoteRenamed := filepath.Join(root, folder.DirName, "remote-renamed.txt")
	wait("remote rename reaching Explorer", func() bool { _, err := os.Stat(remoteRenamed); return err == nil })
	// A capture remains authorized by its surviving item even when a remote
	// move has removed the parent recorded when that capture was made.
	current, err := s.Resolve(folder.DirName + "/remote-renamed.txt")
	if err != nil {
		t.Fatalf("moved item: %+v %v", current, err)
	}
	movedCapture := uuid.NewString() + ".snapshot"
	movedCapturePath := filepath.Join(state, "Captured", movedCapture)
	if err := os.WriteFile(movedCapturePath, []byte("changed remotely"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(movedCapturePath, current.ModTime, current.ModTime); err != nil {
		t.Fatal(err)
	}
	movedRequest := request{Item: current.Item, Node: current.Node, Parent: -1, LocalFolder: folder.ID, Captured: movedCapture, Recovery: true}
	if result, err := h.stage(ctx, movedRequest); err != nil || result.Entry.Item != current.Item || result.Conflict {
		t.Fatalf("capture after its original parent disappeared: %+v %v", result, err)
	}
	if err := s.AcknowledgeSnapshot(captureKey(movedRequest)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(engine.FolderPath, "remote-renamed.txt")); err != nil {
		t.Fatal(err)
	}
	g.Sync()
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	wait("remote delete reaching Explorer", func() bool { _, err := os.Stat(remoteRenamed); return os.IsNotExist(err) })
	replayCapture.Store(true)
	if err := os.WriteFile(filepath.Join(root, folder.DirName, "replay.txt"), []byte("nonempty replay capture retained"), 0600); err != nil {
		t.Fatal(err)
	}
	wait("deleted receipt replay preserving all captured bytes", func() bool {
		names, _ := os.ReadDir(engine.FolderPath)
		for _, name := range names {
			if strings.HasPrefix(name.Name(), "replay.sync-conflict") {
				data, err := os.ReadFile(filepath.Join(engine.FolderPath, name.Name()))
				return err == nil && string(data) == "nonempty replay capture retained"
			}
		}
		return false
	})

	engine.Write("conflict.txt", "base")
	g.Sync()
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	conflicting := filepath.Join(root, folder.DirName, "conflict.txt")
	wait("conflict base hydrated", func() bool { data, err := os.ReadFile(conflicting); return err == nil && string(data) == "base" })
	blockCapture.Store(true)
	if err := os.WriteFile(conflicting, []byte("local edit retained"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-captureEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("native capture did not reach the controlled revision boundary")
	}
	engine.Write("conflict.txt", "remote edit retained")
	g.Sync()
	if err := s.Refresh(ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	close(releaseCapture)
	wait("conflict capture renamed without losing local data", func() bool {
		names, _ := os.ReadDir(filepath.Join(engine.FolderPath))
		for _, name := range names {
			if strings.Contains(name.Name(), "sync-conflict") {
				data, err := os.ReadFile(filepath.Join(engine.FolderPath, name.Name()))
				if err == nil && string(data) == "local edit retained" {
					return true
				}
			}
		}
		return false
	})
	if data, err := os.ReadFile(filepath.Join(engine.FolderPath, "conflict.txt")); err != nil || string(data) != "remote edit retained" {
		t.Fatalf("remote side of conflict: %q %v", data, err)
	}
	wait("native conflict copy present", func() bool {
		names, _ := os.ReadDir(filepath.Join(root, folder.DirName))
		for _, name := range names {
			if strings.Contains(name.Name(), "sync-conflict") {
				data, err := os.ReadFile(filepath.Join(root, folder.DirName, name.Name()))
				if err == nil && string(data) == "local edit retained" {
					return true
				}
			}
		}
		return false
	})
	// Detachment uses a shared-core proof and never uploads intentional cleanup.
	if err := os.WriteFile(filepath.Join(base, "quiesce"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait("quiesced provider", func() bool { _, err := os.Stat(filepath.Join(base, "quiesce.ready")); return err == nil })
	if err := h.waitForFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(base, "quiesce.ready")); err != nil {
		t.Fatal(err)
	}
	wait("final native reconciliation after draining", func() bool { _, err := os.Stat(filepath.Join(base, "quiesce.ready")); return err == nil })
	h.OnModeChange = func(mode, id, root string) error {
		if mode != "" {
			return nil
		}
		if err := os.WriteFile(filepath.Join(base, "detach"), nil, 0600); err != nil {
			return err
		}
		select {
		case err := <-exited:
			providerExited = true
			return err
		case <-time.After(5 * time.Second):
			data, _ := os.ReadFile(filepath.Join(base, "detach.error"))
			return fmt.Errorf("native detach failed: %s", data)
		}
	}
	if err := h.notifyMode(""); err != nil {
		t.Fatal(err)
	}
	children, err := os.ReadDir(root)
	if err != nil || len(children) != 0 {
		t.Fatalf("detached root: %v %v", children, err)
	}
	if folders, err := s.Folders(); err != nil || len(folders) != 1 {
		t.Fatalf("native cleanup changed selected folders: %+v %v", folders, err)
	}
	// Removing a selection deletes children first. Their removal changes the
	// parent's timestamp; that must not strand an empty Explorer directory.
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(provider, "--root", root, "--pipe", pipe, "--client", id, "--ready", ready, "--test", "true", "--shutdown", shutdown)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	providerExited = false
	exited = make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	wait("provider repopulated after detachment", func() bool { _, err := os.Stat(filepath.Join(root, folder.DirName, "photo.png")); return err == nil })
	wait("recreated placeholder retains its saved pin", func() bool {
		_, pin, err := nativePlaceholderStorage(filepath.Join(root, folder.DirName, "new-parent", "offline-renamed", "readonly.txt"))
		return err == nil && pin == 1
	})
	if _, err := h.call(ctx, "files.remove", request{Item: folder.ID}); err != nil {
		t.Fatal(err)
	}
	wait("removing a selection cleans empty parent directories", func() bool { entries, err := os.ReadDir(root); return err == nil && len(entries) == 0 })
	// A new selection can reuse the same Explorer path, but it must never
	// inherit a dirty capture that belonged to the removed selection.
	replacement, err := s.AddFolder(ctx, serviceID, "docs")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.Resolve(replacement.DirName)
	if err != nil {
		t.Fatal(err)
	}
	captured := uuid.NewString() + ".snapshot"
	capturePath := filepath.Join(state, "Captured", captured)
	if err := os.WriteFile(capturePath, []byte("preserved old selection edit"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = h.stage(ctx, request{Item: originalEntry.Item, Parent: parent.Item, LocalFolder: folder.ID, Captured: captured, Name: "original.txt"})
	if err == nil || !strings.Contains(err.Error(), "original folder was removed") {
		t.Fatalf("cross-selection capture was not rejected: %v", err)
	}
	if data, err := os.ReadFile(capturePath); err != nil || string(data) != "preserved old selection edit" {
		t.Fatalf("rejected capture was not preserved: %q %v", data, err)
	}
	if edits, err := s.Edits(); err != nil || len(edits) != 0 {
		t.Fatalf("rejected capture queued edits: %+v %v", edits, err)
	}
}

func nativePlaceholderEntry(path string) (files.Entry, error) {
	var entry files.Entry
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return entry, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return entry, err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]byte, 8192)
	var returned uint32
	result, _, _ := windows.NewLazySystemDLL("CldApi.dll").NewProc("CfGetPlaceholderInfo").Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(unsafe.Pointer(&returned)))
	if uint32(result)&0x80000000 != 0 {
		return entry, fmt.Errorf("placeholder identity: %#x", result)
	}
	// CF_PLACEHOLDER_STANDARD_INFO's variable identity follows the fixed
	// sizes, states, file IDs, and identity length on both target ABIs.
	var layout struct {
		Sizes    [4]int64
		States   [2]uint32
		IDs      [2]int64
		Length   uint32
		Identity byte
	}
	offset := unsafe.Offsetof(layout.Identity)
	length := *(*uint32)(unsafe.Pointer(&buffer[unsafe.Offsetof(layout.Length)]))
	if uintptr(returned) < offset || uintptr(length) > uintptr(returned)-offset {
		return entry, errors.New("invalid native placeholder identity length")
	}
	err = json.Unmarshal(buffer[offset:offset+uintptr(length)], &entry)
	return entry, err
}

func nativePlaceholderStorage(path string) (int64, uint32, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint64, 1024)
	var returned uint32
	result, _, _ := windows.NewLazySystemDLL("CldApi.dll").NewProc("CfGetPlaceholderInfo").Call(uintptr(handle), 1, uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)*8), uintptr(unsafe.Pointer(&returned)))
	if uint32(result)&0x80000000 != 0 {
		return 0, 0, fmt.Errorf("placeholder info: %#x", result)
	}
	return int64(buffer[0]), uint32(buffer[4]), nil
}

// A package upgrade may recover the exact prior tree, never a strict subset or
// a checkpoint containing ambiguous duplicate paths.
func TestLegacyRootInventory(t *testing.T) {
	provider := os.Getenv("SYNCTRAIN_TEST_PROVIDER")
	if provider == "" {
		t.Fatal("SYNCTRAIN_TEST_PROVIDER must name the built native provider")
	}
	root := t.TempDir()
	var snapshot []map[string]any
	for _, name := range []string{"first.txt", "second.txt"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		encoded, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		file, err := windows.CreateFile(encoded, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		var info windows.ByHandleFileInformation
		err = windows.GetFileInformationByHandle(file, &info)
		windows.CloseHandle(file)
		if err != nil {
			t.Fatal(err)
		}
		key := fmt.Sprintf("%d-%d", info.VolumeSerialNumber, uint64(info.FileIndexHigh)<<32|uint64(info.FileIndexLow))
		snapshot = append(snapshot, map[string]any{"Entry": map[string]any{"ClientID": "inventory-test"}, "Path": name, "Key": key})
	}
	pipe := `\\.\pipe\Synctrain-Test-` + uuid.NewString()
	server := adminsock.NewServer()
	server.Register("projection.load", func(context.Context, json.RawMessage) (any, error) { return snapshot, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Listen(ctx, pipe, false); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	probe := func(want string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, provider, "--root", root, "--pipe", pipe, "--client", "inventory-test", "--test", "true", "--probe-legacy-root", "true")
		out, err := cmd.CombinedOutput()
		if err != nil || string(out) != want {
			t.Fatalf("root ownership: %q %v; want %s", out, err, want)
		}
	}
	probe("owned")
	if err := os.Remove(filepath.Join(root, "second.txt")); err != nil {
		t.Fatal(err)
	}
	probe("unowned")
	snapshot = snapshot[:1]
	snapshot = append(snapshot, snapshot[0])
	probe("unowned")
}

// A native save must settle before removing its index, even after the shared
// upload queue has committed. Unrelated folders do not block the removal.
func TestFolderRemovalWaitsForNativeCaptures(t *testing.T) {
	h := &Host{State: t.TempDir()}
	for _, field := range []string{"Capture", "Create", "AwaitingSync", "NeedsCapture"} {
		t.Run(field, func(t *testing.T) {
			value := any(true)
			if field == "Capture" || field == "Create" {
				value = map[string]any{"ID": "pending"}
			}
			data, err := json.Marshal([]map[string]any{{"Entry": files.Entry{Folder: 7}, field: value}})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.writeAtomic("projection.json", data); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := h.waitForProjectionFolder(ctx, 7); err == nil {
				t.Fatal("pending native save allowed folder removal")
			}
			if err := h.waitForProjectionFolder(ctx, 8); err != nil {
				t.Fatal("unrelated folder blocked:", err)
			}
		})
	}
	if err := h.writeAtomic("projection.json", []byte(`[{"Entry":{"Folder":7},"AwaitingSync":false}]`)); err != nil {
		t.Fatal(err)
	}
	if err := h.waitForProjectionFolder(context.Background(), 7); err != nil {
		t.Fatal("settled capture blocked removal:", err)
	}
}
