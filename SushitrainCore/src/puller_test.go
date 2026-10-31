package sushitrain

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/syncthing/syncthing/lib/protocol"
	"github.com/syncthing/syncthing/lib/syncthing"
)

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// A file of blockCount blocks of which the first cachedCount are in the block cache, so that workers get those without
// an engine. Any other block goes to the puller's model. Each block's data is its index.
func fileInBlockCache(t *testing.T, blockCount int, cachedCount int) protocol.FileInfo {
	t.Helper()
	file := protocol.FileInfo{Blocks: make([]protocol.BlockInfo, blockCount)}
	for i := range blockCount {
		hash := fmt.Appendf(nil, "%s-%d", t.Name(), i)
		file.Blocks[i] = protocol.BlockInfo{Hash: hash, Offset: int64(i), Size: 1}
		if i < cachedCount {
			key := base64.StdEncoding.EncodeToString(hash)
			blockCache.Add(key, []byte{byte(i)})
			t.Cleanup(func() { blockCache.Remove(key) })
		}
	}
	return file
}

// A puller for which there is no engine, as during a restart
func pullerWithoutEngine() *miniPuller {
	return newMiniPuller(nil, func() *syncthing.Internals { return nil })
}

// The result of download, which should not take long: nothing in these tests is slow, so this only runs out on a hang
func resultOf(t *testing.T, download func() error) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- download() }()

	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("downloadInto did not return")
		return nil
	}
}

func TestDownloadIntoWritesBlocksInOrder(t *testing.T) {
	file := fileInBlockCache(t, 16, 16)
	var out bytes.Buffer

	err := resultOf(t, func() error {
		return pullerWithoutEngine().downloadInto(context.Background(), &out, "folder", file)
	})
	if err != nil {
		t.Fatalf("downloadInto: %v", err)
	}

	want := make([]byte, 16)
	for i := range want {
		want[i] = byte(i)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("wrote %v, want %v", out.Bytes(), want)
	}
}

func TestDownloadIntoReturnsWorkerError(t *testing.T) {
	file := fileInBlockCache(t, 4, 0)

	err := resultOf(t, func() error {
		return pullerWithoutEngine().downloadInto(context.Background(), &bytes.Buffer{}, "folder", file)
	})
	if !errors.Is(err, ErrStillLoading) {
		t.Fatalf("got %v, want %v", err, ErrStillLoading)
	}
}

// This is how a download is canceled: the delegate says so, and the next write fails. By then workers have read ahead
// and are waiting to hand over their next block, which nobody is going to read.
func TestDownloadIntoWriteErrorStopsWorkersWaitingToSend(t *testing.T) {
	file := fileInBlockCache(t, 16, 16)
	failure := errors.New("write failed")
	failingWriter := writerFunc(func([]byte) (int, error) { return 0, failure })

	err := resultOf(t, func() error {
		return pullerWithoutEngine().downloadInto(context.Background(), failingWriter, "folder", file)
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want %v", err, failure)
	}
}

// When a write fails nobody reads errChan anymore, yet both workers can still have an error to report
func TestDownloadIntoWriteErrorStopsWorkersWithErrors(t *testing.T) {
	// The first worker gets block 0 and fails on block 2, the second fails on block 1
	file := fileInBlockCache(t, 3, 1)
	failure := errors.New("write failed")

	// Workers fail only while block 0 is being written, so that their errors can't be read instead of that block
	writing := make(chan struct{})
	var failed sync.WaitGroup
	failed.Add(2)
	puller := newMiniPuller(nil, func() *syncthing.Internals {
		<-writing
		failed.Done()
		return nil
	})
	failingWriter := writerFunc(func([]byte) (int, error) {
		close(writing)
		failed.Wait()
		return 0, failure
	})

	err := resultOf(t, func() error {
		return puller.downloadInto(context.Background(), failingWriter, "folder", file)
	})
	if !errors.Is(err, failure) {
		t.Fatalf("got %v, want %v", err, failure)
	}
}

// Workers that find themselves canceled just stop, so the reader needs to notice the cancellation by itself
func TestDownloadIntoReturnsWhenCanceled(t *testing.T) {
	file := fileInBlockCache(t, 16, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelingWriter := writerFunc(func(p []byte) (int, error) {
		cancel()
		return len(p), nil
	})

	err := resultOf(t, func() error {
		return pullerWithoutEngine().downloadInto(ctx, cancelingWriter, "folder", file)
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want %v", err, context.Canceled)
	}
}
