// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	defaultPageSize  = 256
	maxPageSize      = 1024
	longPollMaximum  = 60 * time.Second
	defaultSpoolSize = 8 << 30
	// Staging space per client
	defaultStagingQuota = 32 << 30
	defaultMaxUpload    = 1 << 40
)

// Config configures a service. Zero values select defaults.
type Config struct {
	// Directory for the service database, TLS identity and staged uploads, normally <Syncthing home>/synctrain-gateway
	Home string
	// Directory for the content spool and thumbnails, which are rebuilt on demand and may be removed at any time, such
	// as a cache directory kept out of backups. Home when empty.
	CacheDir string
	// Address to listen on, such as ":20920"
	ListenAddress string
	// Certificate and key for direct TLS. When empty and Proxy is false, a persistent self-signed certificate is
	// created in Home, and clients pin its public key.
	CertFile string
	KeyFile  string
	// Serve plain HTTP for a reverse proxy that terminates TLS. Only for loopback or an otherwise protected network.
	Proxy bool
	// The public key hash clients should pin when a proxy presents a self-signed certificate
	ProxySPKI string
	// Name shown to clients
	ServiceName string
	// Whether the host can move files to the system Trash (folders with the basic-trash filesystem)
	TrashAvailable bool
	// Renders thumbnails; nil disables them
	Thumbnails ThumbnailRenderer
	// Bytes of prepared content kept for reading
	SpoolQuota int64
	// Bytes of staged uploads per client
	StagingQuota  int64
	MaxUploadSize int64
	// How long audit log entries are kept; 0 keeps them indefinitely
	AuditRetention time.Duration
	Logger         *slog.Logger
}

// Service is the remote file service
type Service struct {
	cfg     Config
	backend RemoteFileBackend
	store   *store
	log     *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	notifier       notifier
	pairingChanged notifier
	pairing        pairingLimiter
	folderLocks    sync.Map
	requestSlots   chan struct{}
	publicSlots    chan struct{}
	exportsChanged chan struct{}
	clientRequests clientRequests
	thumbnailSlot  chan struct{}
	// Set once a cursor beyond the change log started a new authority generation since the service started
	restoreDetected atomic.Bool
	// Held for reading by folder loop callbacks that change files (beginPublish), and briefly for writing by Close
	publishing sync.RWMutex

	content *contentManager
	uploads *uploadManager
	ops     *operationRunner

	certMutex  sync.Mutex
	selfSigned bool
	cert       *tls.Certificate
	certMod    time.Time
	spki       string

	listener net.Listener
	server   *http.Server

	// Tests stop operations at journal states with this, to simulate a crash
	journalHook func(opID, state string) error
}

// New opens the service database and prepares the service; Start begins serving
func New(cfg Config, backend RemoteFileBackend) (*Service, error) {
	if cfg.Home == "" {
		return nil, errors.New("the service needs a home directory")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SpoolQuota == 0 {
		cfg.SpoolQuota = defaultSpoolSize
	}
	if cfg.StagingQuota == 0 {
		cfg.StagingQuota = defaultStagingQuota
	}
	if cfg.MaxUploadSize == 0 {
		cfg.MaxUploadSize = defaultMaxUpload
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName, _ = os.Hostname()
	}
	if err := os.MkdirAll(cfg.Home, 0o700); err != nil {
		return nil, err
	}
	// The directory holds the database with the token key, the TLS key and staged uploads: only this user may enter
	if err := makePrivate(cfg.Home); err != nil {
		return nil, err
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = cfg.Home
	} else if filepath.Clean(cfg.CacheDir) != filepath.Clean(cfg.Home) {
		// The spool and thumbnails of a service that kept them in its home, which nothing reads any more
		for _, name := range []string{"content", "thumbnails"} {
			if err := os.RemoveAll(filepath.Join(cfg.Home, name)); err != nil {
				cfg.Logger.Warn("Could not remove the remote file service's old cache", "path", filepath.Join(cfg.Home, name), "error", err)
			}
		}
	}
	st, err := openStore(filepath.Join(cfg.Home, "service.sqlite"))
	if err != nil {
		return nil, err
	}
	// The database holds the key scoped tokens are derived from. SQLite gives its journal files the database's mode.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Chmod(filepath.Join(cfg.Home, "service.sqlite"+suffix), 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			st.close()
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{
		cfg:            cfg,
		backend:        backend,
		store:          st,
		log:            cfg.Logger.With("component", "gateway"),
		ctx:            ctx,
		cancel:         cancel,
		requestSlots:   make(chan struct{}, maxConcurrentRequests),
		publicSlots:    make(chan struct{}, maxPublicRequests),
		exportsChanged: make(chan struct{}, 1),
		thumbnailSlot:  make(chan struct{}, 1),
	}
	if s.content, err = newContentManager(s, filepath.Join(cfg.CacheDir, "content")); err != nil {
		st.close()
		return nil, err
	}
	if s.uploads, err = newUploadManager(s, filepath.Join(cfg.Home, "uploads")); err != nil {
		st.close()
		return nil, err
	}
	s.ops = newOperationRunner(s)
	return s, nil
}

// Start recovers interrupted operations, starts reconciling the exported folders and listens for clients
func (s *Service) Start() error {
	if err := s.ops.recover(s.ctx); err != nil {
		return fmt.Errorf("recovering interrupted operations: %w", err)
	}
	handler := s.routes()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelDebug),
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
	}
	listener, err := net.Listen("tcp", s.cfg.ListenAddress)
	if err != nil {
		return err
	}
	if s.cfg.Proxy {
		if addr, ok := listener.Addr().(*net.TCPAddr); ok && !addr.IP.IsLoopback() {
			s.log.Warn("Serving plain HTTP for a reverse proxy on a non-loopback address; credentials cross the network "+
				"unencrypted unless that network is protected", "address", addr.String())
		}
	}
	if !s.cfg.Proxy {
		if err := s.loadCertificate(); err != nil {
			listener.Close()
			return err
		}
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion:     tls.VersionTLS12,
			GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.currentCertificate() },
			NextProtos:     []string{"h2", "http/1.1"},
		})
	}
	s.listener = listener
	s.server = server

	s.wg.Add(4)
	go func() {
		defer s.wg.Done()
		s.runReconciler(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.runDeselection(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		s.runMaintenance(s.ctx)
	}()
	go func() {
		defer s.wg.Done()
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("Remote file service stopped serving", "error", err)
		}
	}()
	s.log.Info("Remote file service listening", "address", listener.Addr().String(), "tls", !s.cfg.Proxy)
	return nil
}

// Addr returns the address the service listens on
func (s *Service) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Close stops serving, waits for background work and closes the database
func (s *Service) Close() error {
	s.cancel()
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = s.server.Shutdown(ctx)
		cancel()
	}
	s.wg.Wait()
	// Operations stop at their next step; one waiting for a long pull is resumed from its journal at the next start
	// rather than holding up Syncthing's shutdown
	done := make(chan struct{})
	go func() {
		s.ops.wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		s.log.Info("Stopping with operations in progress; they continue at the next start")
	}
	// Callbacks that started changing files finish and record it; later ones see the service stopping
	s.publishing.Lock()
	s.publishing.Unlock()
	return s.store.close()
}

// SPKI returns the base64url SHA-256 of the certificate's public key, for pinning ("" for a proxy without one)
func (s *Service) SPKI() string {
	if s.cfg.Proxy {
		return s.cfg.ProxySPKI
	}
	s.certMutex.Lock()
	defer s.certMutex.Unlock()
	return s.spki
}

// UsesSelfSignedCertificate reports whether clients must pin the certificate's public key
func (s *Service) UsesSelfSignedCertificate() bool {
	return !s.cfg.Proxy && s.selfSigned || s.cfg.Proxy && s.cfg.ProxySPKI != ""
}

func (s *Service) runMaintenance(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		s.uploads.expire(ctx)
		s.content.expire(ctx)
		s.expireSessions(ctx)
		s.pruneThumbnails()
		if err := s.pruneAudit(ctx); err != nil {
			s.log.Warn("Could not prune the audit log", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// expireSessions removes expired snapshot sessions and pairing state
func (s *Service) expireSessions(ctx context.Context) {
	now := s.store.now()
	err := s.store.tx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			query string
			arg   int64
		}{
			{`DELETE FROM snapshots WHERE created_at < ?`, now.Add(-snapshotLifetime).Unix()},
			{`DELETE FROM pairing_sessions WHERE expires_at < ?`, now.Unix()},
			{`DELETE FROM pairing_requests WHERE expires_at < ?`, now.Add(-time.Hour).Unix()},
		} {
			if _, err := tx.Exec(q.query, q.arg); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.log.Warn("Could not expire sessions", "error", err)
	}
}

// Certificates

func (s *Service) loadCertificate() error {
	certFile, keyFile := s.cfg.CertFile, s.cfg.KeyFile
	if certFile == "" {
		certFile = filepath.Join(s.cfg.Home, "https-cert.pem")
		keyFile = filepath.Join(s.cfg.Home, "https-key.pem")
		if _, err := os.Stat(certFile); errors.Is(err, os.ErrNotExist) {
			if err := generateSelfSigned(certFile, keyFile, s.cfg.ServiceName); err != nil {
				return fmt.Errorf("creating a certificate: %w", err)
			}
		}
		s.cfg.CertFile, s.cfg.KeyFile = certFile, keyFile
		s.selfSigned = true
	}
	_, err := s.currentCertificate()
	return err
}

// currentCertificate returns the certificate, reloading it when its file changed (as a certificate renewal does)
func (s *Service) currentCertificate() (*tls.Certificate, error) {
	s.certMutex.Lock()
	defer s.certMutex.Unlock()
	stat, err := os.Stat(s.cfg.CertFile)
	if err != nil {
		if s.cert != nil {
			return s.cert, nil
		}
		return nil, err
	}
	if s.cert != nil && stat.ModTime().Equal(s.certMod) {
		return s.cert, nil
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.CertFile, s.cfg.KeyFile)
	if err != nil {
		if s.cert != nil {
			s.log.Warn("Could not reload the certificate; keeping the previous one", "error", err)
			return s.cert, nil
		}
		return nil, err
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	s.cert, s.certMod, s.spki = &cert, stat.ModTime(), b64.EncodeToString(sum[:])
	return s.cert, nil
}

// generateSelfSigned creates a long-lived self-signed certificate. Clients pin its public key, so its validity
// period and names don't matter to them.
func generateSelfSigned(certFile, keyFile, name string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Synctrain gateway " + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(20, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// A DNS name must be ASCII; a name such as "Gordon’s Mac" can only go into the subject
	if isHostname(name) {
		template.DNSNames = []string{name}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return writeFileAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func isHostname(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// makePrivate restricts a directory of this user's to this user, and refuses one of another user's
func makePrivate(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("the service directory %s belongs to another user (%d)", dir, st.Uid)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// writeFileAtomic replaces a file through a new, uniquely named temporary file, so that nothing placed at a
// predictable name beforehand can receive the data
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
