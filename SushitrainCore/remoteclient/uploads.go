// Copyright (C) 2026 Gordon
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at https://mozilla.org/MPL/2.0/.

package remoteclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"t-shaped.nl/sushitrain/v2/remoteapi"
)

// UploadChunkSize is how much one PATCH sends, each with its own checksum, so that an interrupted request costs at
// most this much again
const UploadChunkSize = 8 << 20

// StatusChecksumMismatch is tus's status for a chunk whose checksum doesn't match its bytes
const StatusChecksumMismatch = 460

// TusOptions is what the gateway's tus endpoint supports
type TusOptions struct {
	Versions           []string
	Extensions         []string
	MaxSize            int64
	ChecksumAlgorithms []string
}

// UploadOptions asks the gateway about its uploads (tus discovery)
func (c *Client) UploadOptions(ctx context.Context) (TusOptions, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, "OPTIONS", []string{"v1", "uploads"}, nil, withNothing, nil)
	if err != nil {
		return TusOptions{}, err
	}
	resp, err := c.do(req, KindOther)
	if err != nil {
		return TusOptions{}, err
	}
	resp.Body.Close()
	list := func(header string) []string {
		var values []string
		for _, v := range strings.Split(resp.Header.Get(header), ",") {
			if v = strings.TrimSpace(v); v != "" {
				values = append(values, v)
			}
		}
		return values
	}
	maxSize, _ := strconv.ParseInt(resp.Header.Get("Tus-Max-Size"), 10, 64)
	return TusOptions{Versions: list("Tus-Version"), Extensions: list("Tus-Extension"), MaxSize: maxSize,
		ChecksumAlgorithms: list("Tus-Checksum-Algorithm")}, nil
}

// UploadDigest is how an operation names an upload's content: base64url of its SHA-256
func UploadDigest(sum [sha256.Size]byte) string {
	return remoteapi.B64.EncodeToString(sum[:])
}

// CreateUpload creates a staged upload in the folder for content of the length whose whole SHA-256 is digest. An
// empty file is complete at once.
func (c *Client) CreateUpload(ctx context.Context, folderID string, length int64, digest [sha256.Size]byte) (
	remoteapi.Upload, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, "POST", []string{"v1", "uploads"}, nil, withCredential, http.NoBody)
	if err != nil {
		return remoteapi.Upload{}, err
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Upload-Length", strconv.FormatInt(length, 10))
	req.Header.Set("Upload-Metadata", "folder "+base64.StdEncoding.EncodeToString([]byte(folderID))+",digest "+
		base64.StdEncoding.EncodeToString(digest[:]))
	resp, err := c.do(req, KindOther)
	if err != nil {
		return remoteapi.Upload{}, err
	}
	data, err := readBody(resp, maxControlBody)
	if err != nil {
		return remoteapi.Upload{}, err
	}
	var upload remoteapi.Upload
	if json.Unmarshal(data, &upload) == nil && upload.Upload != "" && upload.Token != "" {
		return upload, nil
	}
	// The same in headers, as tus describes it
	location := resp.Header.Get("Location")
	upload = remoteapi.Upload{Upload: location[strings.LastIndex(location, "/")+1:],
		Token: resp.Header.Get("Synctrain-Upload-Token"), Length: length, Expires: resp.Header.Get("Upload-Expires")}
	if upload.Upload == "" || upload.Token == "" {
		return remoteapi.Upload{}, protocolError("the upload was created without its ID and token")
	}
	return upload, nil
}

// UploadOffset returns how much of the upload the gateway has (KindNotFound: unknown, terminated or expired)
func (c *Client) UploadOffset(ctx context.Context, uploadID, token string) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, "HEAD", []string{"v1", "uploads", uploadID}, nil, withToken(token), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	resp, err := c.do(req, KindNotFound)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	offset, err := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		return 0, protocolError("the upload's offset is missing")
	}
	return offset, nil
}

// How long a chunk may go without any of it being sent, or without an answer after it was: an idle timeout, as the
// Swift client's, so that a slow link still gets each chunk through however long it takes
const stallTimeout = 2 * time.Minute

// progressReader tells of each read of a request's body
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
}

// AppendUpload sends chunk at offset, with its checksum, so that a request cut off appends nothing; it returns the
// new offset. A 409 (the gateway has another offset) and a 460 (checksum mismatch) come back as errors with that
// Status. It is given up once it makes no progress for stallTimeout.
func (c *Client) AppendUpload(ctx context.Context, uploadID, token string, offset int64, chunk []byte) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stalled := time.AfterFunc(stallTimeout, cancel)
	defer stalled.Stop()
	body := &progressReader{r: bytes.NewReader(chunk), progress: func() { stalled.Reset(stallTimeout) }}
	req, err := c.newRequest(ctx, "PATCH", []string{"v1", "uploads", uploadID}, nil, withToken(token), body)
	if err != nil {
		return 0, err
	}
	req.ContentLength = int64(len(chunk))
	sum := sha256.Sum256(chunk)
	req.Header.Set("Tus-Resumable", tusVersion)
	req.Header.Set("Content-Type", "application/offset+octet-stream")
	req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
	req.Header.Set("Upload-Checksum", "sha256 "+base64.StdEncoding.EncodeToString(sum[:]))
	resp, err := c.do(req, KindNotFound)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	next, err := strconv.ParseInt(resp.Header.Get("Upload-Offset"), 10, 64)
	if err != nil {
		return 0, protocolError("the upload's new offset is missing")
	}
	return next, nil
}

// TerminateUpload removes an upload from the gateway (tus termination)
func (c *Client) TerminateUpload(ctx context.Context, uploadID, token string) error {
	ctx, cancel := context.WithTimeout(ctx, controlTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, "DELETE", []string{"v1", "uploads", uploadID}, nil, withToken(token), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	resp, err := c.do(req, KindNotFound)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// SendUpload sends the rest of src (of the upload's length) to an upload created with CreateUpload, starting from
// the offset the gateway has, in checksummed chunks, with the credential (the upload's token stops working when it is
// terminated, so a gone upload would look like a revoked client). A chunk the gateway refuses with another offset or a checksum
// mismatch is retried from the offset it reports, a few times. A KindNotFound error means the upload is gone (it
// expired or was terminated) and must be created again. progress, if not nil, is told each new offset.
func (c *Client) SendUpload(ctx context.Context, upload remoteapi.Upload, src io.ReaderAt, progress func(int64)) error {
	offset, err := c.UploadOffset(ctx, upload.Upload, "")
	if err != nil {
		return err
	}
	buffer := make([]byte, min(UploadChunkSize, max(upload.Length, 1)))
	// Over the whole upload, so that a refusal that keeps coming back ends it
	retries := 0
	for offset < upload.Length {
		n := int(min(int64(len(buffer)), upload.Length-offset))
		read, err := src.ReadAt(buffer[:n], offset)
		if read != n {
			if err == nil || errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("reading what to upload at %d: %w", offset, err)
		}
		next, err := c.AppendUpload(ctx, upload.Upload, "", offset, buffer[:n])
		var e *Error
		switch {
		case errors.As(err, &e) && e.Code == remoteapi.CodeUploadMismatch:
			// The whole upload didn't match its digest, and the gateway emptied it: sending it again from the same
			// content can't do better
			return err
		case errors.As(err, &e) && (e.Status == http.StatusConflict || e.Status == StatusChecksumMismatch) &&
			retries < 5:
			retries++
			if offset, err = c.UploadOffset(ctx, upload.Upload, ""); err != nil {
				return err
			}
			continue
		case err != nil:
			return err
		}
		if next <= offset || next > upload.Length {
			return protocolError("the upload's offset went from %d to %d", offset, next)
		}
		offset = next
		if progress != nil {
			progress(offset)
		}
	}
	return nil
}
