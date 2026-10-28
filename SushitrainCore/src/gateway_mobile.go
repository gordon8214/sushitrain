// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package sushitrain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/syncthing/syncthing/lib/build"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/protocol"

	"t-shaped.nl/sushitrain/v2/gateway"
)

// RemoteFileService runs the remote file service (Docs/gateway.md) in this client, so that Synctrain on other devices
// (the Files app on iOS) can browse, open and edit this device's folders over HTTPS. Not available on iOS.
type RemoteFileService struct {
	client     *Client
	svc        *gateway.Service
	publicHost string
	publicPort int
}

type remoteFileServiceSettings struct {
	ListenAddress   string `json:"listenAddress"`
	CertFile        string `json:"certFile"`
	KeyFile         string `json:"keyFile"`
	Proxy           bool   `json:"proxy"`
	ProxySPKI       string `json:"proxySPKI"`
	ServiceName     string `json:"serviceName"`
	PublicHost      string `json:"publicHost"`
	PublicPort      int    `json:"publicPort"`
	ContentCacheMiB int64  `json:"contentCacheMiB"`
}

// RemoteThumbnailRenderer renders thumbnails for the remote file service (implemented with QuickLook on macOS)
type RemoteThumbnailRenderer interface {
	SupportsThumbnail(name string) bool
	// RenderThumbnail returns a JPEG of the file at path whose longer side is at most size pixels
	RenderThumbnail(path string, size int) ([]byte, error)
}

var remoteThumbnails struct {
	sync.Mutex
	renderer RemoteThumbnailRenderer
}

// SetRemoteThumbnailRenderer must be called before starting the service for it to offer thumbnails
func SetRemoteThumbnailRenderer(renderer RemoteThumbnailRenderer) {
	remoteThumbnails.Lock()
	defer remoteThumbnails.Unlock()
	remoteThumbnails.renderer = renderer
}

// How long the audit log keeps entries, as on Linux hosts
const remoteAuditRetention = 365 * 24 * time.Hour

type thumbnailAdapter struct {
	renderer RemoteThumbnailRenderer
}

func (a thumbnailAdapter) Features() []string {
	return []string{"thumbnails.image", "thumbnails.video", "thumbnails.pdf"}
}

func (a thumbnailAdapter) Supports(name string) (bool, bool) {
	if !a.renderer.SupportsThumbnail(name) {
		return false, false
	}
	// QuickLook reads videos from local files only; don't fetch whole videos for a thumbnail
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".m4v", ".mov", ".mkv", ".avi", ".webm", ".3gp":
		return true, true
	}
	return true, false
}

func (a thumbnailAdapter) Render(_ context.Context, path string, size int) ([]byte, error) {
	return a.renderer.RenderThumbnail(path, size)
}

// StartRemoteFileService starts the service with settings given as JSON (listenAddress, certFile, keyFile, proxy,
// proxySPKI, serviceName, publicHost, publicPort, contentCacheMiB). The client must have started.
func (clt *Client) StartRemoteFileService(settingsJSON []byte) (*RemoteFileService, error) {
	if build.IsIOS {
		return nil, errors.New("the remote file service is not available on iOS")
	}
	if clt.app == nil || clt.app.Internals == nil || clt.Measurements == nil {
		return nil, ErrStillLoading
	}
	var settings remoteFileServiceSettings
	if err := json.Unmarshal(settingsJSON, &settings); err != nil {
		return nil, err
	}
	if settings.ListenAddress == "" {
		settings.ListenAddress = ":20920"
	}
	systemTrash.RLock()
	trashAvailable := systemTrash.handler != nil
	systemTrash.RUnlock()

	id := protocol.NewDeviceID(clt.cert.Certificate[0])
	backend := &gateway.EngineBackend{
		App:    clt.app,
		Config: clt.config,
		Events: clt.evLogger,
		ID:     id,
		Fetch: func(ctx context.Context, folder string, file protocol.FileInfo, index int) ([]byte, error) {
			data, err := newMiniPuller(clt.Measurements, clt.app.Internals).downloadBlock(ctx, folder, index, file)
			if err == nil && verifyBlockHash(data, file.Blocks[index]) != nil {
				// Never keep a block that failed verification in the shared cache
				blockCache.Remove(base64.StdEncoding.EncodeToString(file.Blocks[index].Hash))
			}
			return data, err
		},
		IsSelective: func(folderID string) bool {
			if folder := clt.FolderWithID(folderID); folder != nil {
				selective, err := folder.IsSelective()
				return err == nil && selective
			}
			return false
		},
	}
	cfg := gateway.Config{
		Home:           filepath.Join(locations.GetBaseDir(locations.ConfigBaseDir), "synctrain-gateway"),
		ListenAddress:  settings.ListenAddress,
		CertFile:       settings.CertFile,
		KeyFile:        settings.KeyFile,
		Proxy:          settings.Proxy,
		ProxySPKI:      settings.ProxySPKI,
		ServiceName:    settings.ServiceName,
		TrashAvailable: trashAvailable,
		SpoolQuota:     settings.ContentCacheMiB << 20,
		AuditRetention: remoteAuditRetention,
		Logger:         slog.Default(),
	}
	remoteThumbnails.Lock()
	if remoteThumbnails.renderer != nil {
		cfg.Thumbnails = thumbnailAdapter{renderer: remoteThumbnails.renderer}
	}
	remoteThumbnails.Unlock()
	svc, err := gateway.New(cfg, backend)
	if err != nil {
		return nil, err
	}
	if err := svc.Start(); err != nil {
		svc.Close()
		return nil, err
	}
	return &RemoteFileService{client: clt, svc: svc, publicHost: settings.PublicHost, publicPort: settings.PublicPort}, nil
}

func verifyBlockHash(data []byte, block protocol.BlockInfo) error {
	sum := sha256.Sum256(data)
	if len(data) != block.Size || !bytes.Equal(sum[:], block.Hash) {
		return errors.New("block does not match its hash")
	}
	return nil
}

func (r *RemoteFileService) Stop() error {
	return r.svc.Close()
}

// Port returns the port the service listens on
func (r *RemoteFileService) Port() int {
	addr := r.svc.Addr()
	if addr == nil {
		return 0
	}
	_, port, _ := net.SplitHostPort(addr.String())
	n, _ := strconv.Atoi(port)
	return n
}

func jsonResult(value any, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func (r *RemoteFileService) StatusJSON() ([]byte, error) {
	return jsonResult(r.svc.Status())
}

func (r *RemoteFileService) ExportsJSON() ([]byte, error) {
	return jsonResult(r.svc.Exports())
}

func (r *RemoteFileService) SetExport(folderID string, allowWrite bool) error {
	return r.svc.SetExport(folderID, allowWrite)
}

func (r *RemoteFileService) RemoveExport(folderID string) error {
	return r.svc.RemoveExport(folderID)
}

func (r *RemoteFileService) ClientsJSON() ([]byte, error) {
	return jsonResult(r.svc.Clients())
}

func (r *RemoteFileService) RevokeClient(clientID string) error {
	return r.svc.RevokeClient(clientID)
}

// SetPermission sets a client's access to a folder: "read", "write" or "" (none)
func (r *RemoteFileService) SetPermission(clientID, folderID, access string) error {
	return r.svc.SetPermission(clientID, folderID, access)
}

// StartPairingJSON starts pairing and returns the session (sessionID, secret, url, expires) as JSON
func (r *RemoteFileService) StartPairingJSON() ([]byte, error) {
	spki := ""
	if r.svc.UsesSelfSignedCertificate() {
		spki = r.svc.SPKI()
	}
	port := r.publicPort
	if port == 0 {
		port = r.Port()
	}
	return jsonResult(r.svc.StartPairing(r.publicHost, port, spki))
}

func (r *RemoteFileService) PendingPairingsJSON() ([]byte, error) {
	return jsonResult(r.svc.PendingPairings())
}

// ApprovePairing approves a request with grants as JSON: {"<folder ID>": "read" | "write"}
func (r *RemoteFileService) ApprovePairing(requestID string, grantsJSON []byte) error {
	var grants map[string]string
	if err := json.Unmarshal(grantsJSON, &grants); err != nil {
		return err
	}
	return r.svc.ApprovePairing(requestID, grants)
}

func (r *RemoteFileService) DenyPairing(requestID string) error {
	return r.svc.DenyPairing(requestID)
}

func (r *RemoteFileService) PendingOperationsJSON() ([]byte, error) {
	return jsonResult(r.svc.PendingOperations())
}
