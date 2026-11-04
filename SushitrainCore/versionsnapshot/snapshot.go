// Copyright (C) 2026 Gordon
// SPDX-License-Identifier: MPL-2.0

// Package versionsnapshot transfers complete archived Mac files without relying on
// the host filesystem's ability to represent foreign extended attributes.
package versionsnapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/syncthing/syncthing/lib/fs"
	"github.com/syncthing/syncthing/lib/protocol"
)

const MaxAttributes = 65536
const MaxAttributeBytes = 500_000_000
const MaxManifestBytes = 4 << 20

var ErrIncomplete = errors.New("this version has no complete saved Mac metadata")

type Entry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
type Manifest struct {
	Format     int     `json:"format"`
	FolderID   string  `json:"folderID"`
	Path       string  `json:"path"`
	VersionID  string  `json:"versionID"`
	ModifiedNs int64   `json:"modifiedNs"`
	Mode       uint32  `json:"mode"`
	Content    Entry   `json:"content"`
	Attributes []Entry `json:"attributes"`
}

type reader struct {
	ctx    context.Context
	source io.Reader
}

func (r reader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(b)
}
func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

// Tag includes unfiltered metadata. In particular, a metadata-only archive change
// cannot reuse the validator of a selected snapshot.
func Tag(contentTag string, pd protocol.PlatformData) (string, error) {
	if pd.Darwin == nil {
		return "", ErrIncomplete
	}
	digest, err := AttributeDigest(pd.Darwin.Xattrs)
	if err != nil {
		return "", err
	}
	return `"snapshot-` + Digest([]byte(contentTag+"\x00"+digest)) + `"`, nil

}

// AttributeDigest hashes raw values without creating base64 copies of resource forks.
func AttributeDigest(attributes []protocol.Xattr) (string, error) {
	if err := ValidAttributes(attributes); err != nil {
		return "", err
	}
	attrs := append([]protocol.Xattr(nil), attributes...)
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Name < attrs[j].Name })
	h := sha256.New()
	var length [8]byte
	for _, a := range attrs {
		binary.BigEndian.PutUint64(length[:], uint64(len(a.Name)))
		h.Write(length[:])
		h.Write([]byte(a.Name))
		binary.BigEndian.PutUint64(length[:], uint64(len(a.Value)))
		h.Write(length[:])
		h.Write(a.Value)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func ValidAttributes(attrs []protocol.Xattr) error {
	if len(attrs) > MaxAttributes {
		return errors.New("too many archived attributes")
	}
	seen := map[string]bool{}
	var total int64
	for _, a := range attrs {
		if a.Name == "" || strings.ContainsRune(a.Name, 0) || a.Name == "user.syncthing.platform-history" || seen[a.Name] {
			return errors.New("invalid archived attribute name")
		}
		seen[a.Name] = true
		total += int64(len(a.Value))
		if total > MaxAttributeBytes {
			return errors.New("archived metadata exceeds protocol limit")
		}
	}
	return nil
}
func Write(ctx context.Context, w io.Writer, file fs.File, pd protocol.PlatformData, folder, path, id string) error {
	if pd.Darwin == nil {
		return ErrIncomplete
	}
	attrs := append([]protocol.Xattr(nil), pd.Darwin.Xattrs...)
	if err := ValidAttributes(attrs); err != nil {
		return err
	}
	sort.Slice(attrs, func(i, j int) bool { return attrs[i].Name < attrs[j].Name })
	info, err := file.Stat()
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(hash, reader{ctx, file})
	if err != nil {
		return err
	}
	if n != info.Size() {
		return errors.New("archive changed")
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	manifest := Manifest{Format: 1, FolderID: folder, Path: path, VersionID: id, ModifiedNs: info.ModTime().UnixNano(), Mode: uint32((info.Mode() & 0777)), Content: Entry{Name: "content", Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, Attributes: []Entry{}}
	for _, a := range attrs {
		manifest.Attributes = append(manifest.Attributes, Entry{Name: a.Name, Size: int64(len(a.Value)), SHA256: Digest(a.Value)})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if len(data) > MaxManifestBytes {
		return errors.New("snapshot manifest too large")
	}
	tw := tar.NewWriter(w)
	write := func(name string, size int64, r io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.CopyN(tw, reader{ctx, r}, size)
		return err
	}
	if err = write("manifest.json", int64(len(data)), strings.NewReader(string(data))); err != nil {
		return err
	}
	if err = write("content", n, file); err != nil {
		return err
	}
	for i, a := range attrs {
		if err = write(fmt.Sprintf("xattrs/%d", i), int64(len(a.Value)), bytes.NewReader(a.Value)); err != nil {
			return err
		}
	}
	return tw.Close()
}

// Extract accepts exactly the ordered, regular entries from our format, never
// tar paths or links supplied by a remote peer. It writes an exclusive staging file.
func Extract(ctx context.Context, source io.Reader, destination, folder, path, id string) (manifest Manifest, err error) {
	tr := tar.NewReader(reader{ctx, source})
	header, err := tr.Next()
	if err != nil {
		return manifest, err
	}
	if header.Name != "manifest.json" || header.Typeflag != tar.TypeReg || header.Size > MaxManifestBytes || header.Size < 0 {
		return manifest, errors.New("invalid snapshot manifest")
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		return manifest, err
	}
	if err = json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if manifest.Format != 1 || manifest.FolderID != folder || manifest.Path != path || manifest.VersionID != id || manifest.Content.Name != "content" || manifest.Content.Size < 0 || len(manifest.Attributes) > MaxAttributes {
		return manifest, errors.New("snapshot identity mismatch")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return manifest, err
	}
	defer func() {
		out.Close()
		if err != nil {
			os.Remove(destination)
		}
	}()
	extract := func(name string, e Entry, w io.Writer) error {
		h, err := tr.Next()
		if err != nil {
			return err
		}
		if h.Name != name || h.Typeflag != tar.TypeReg || h.Size != e.Size || e.Size < 0 || len(e.SHA256) != 64 {
			return errors.New("invalid snapshot entry")
		}
		hash := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, hash), tr)
		if err != nil {
			return err
		}
		if n != e.Size || hex.EncodeToString(hash.Sum(nil)) != e.SHA256 {
			return errors.New("snapshot digest mismatch")
		}
		return nil
	}
	if err = extract("content", manifest.Content, out); err != nil {
		return manifest, err
	}
	attrs := make([]protocol.Xattr, 0, len(manifest.Attributes))
	var total int64
	for i, e := range manifest.Attributes {
		total += e.Size
		if e.Size < 0 || total > MaxAttributeBytes {
			return manifest, errors.New("snapshot metadata too large")
		}
		var value bytes.Buffer
		if err = extract(fmt.Sprintf("xattrs/%d", i), e, &value); err != nil {
			return manifest, err
		}
		attrs = append(attrs, protocol.Xattr{Name: e.Name, Value: value.Bytes()})
	}
	if err = ValidAttributes(attrs); err != nil {
		return manifest, err
	}
	if _, err = tr.Next(); err != io.EOF {
		return manifest, errors.New("unexpected snapshot entry")
	}
	err = nil
	if err = ApplyAttributes(destination, attrs); err != nil {
		return manifest, err
	}
	if err = out.Chmod(os.FileMode(manifest.Mode) & 0777); err != nil {
		return manifest, err
	}
	if err = os.Chtimes(destination, timeFromNs(manifest.ModifiedNs), timeFromNs(manifest.ModifiedNs)); err != nil {
		return manifest, err
	}
	if err = out.Sync(); err != nil {
		return manifest, err
	}
	return manifest, nil
}
