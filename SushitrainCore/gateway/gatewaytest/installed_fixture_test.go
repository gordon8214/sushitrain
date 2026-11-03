//go:build !windows && installedintegration

package gatewaytest

import (
	"bytes"
	"encoding/json"
	"github.com/syncthing/syncthing/lib/config"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWindowsInstalledGateway(t *testing.T) {
	output := os.Getenv("SYNCTRAIN_ACCEPTANCE_DIRECTORY")
	host := os.Getenv("SYNCTRAIN_ACCEPTANCE_HOST")
	if output == "" || host == "" {
		t.Fatal("acceptance directory and LAN host are required")
	}
	root, err := os.MkdirTemp(output, "acceptance-source-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	if err := os.Mkdir(filepath.Join(root, ".stfolder"), 0700); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(t, "windows-acceptance", func(cfg *config.FolderConfiguration) { cfg.Path = root })
	engine.FolderPath = root
	engine.Write("Acceptance.txt", "Installed Synctrain Cloud Files verification")
	engine.Write("Background.txt", "Fetched while the window was hidden")
	var photo bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 800, 600))
	for y := 0; y < 600; y++ {
		for x := 0; x < 800; x++ {
			picture.SetRGBA(x, y, color.RGBA{uint8(x * 255 / 800), uint8(y * 255 / 600), 160, 255})
		}
	}
	if err := png.Encode(&photo, picture); err != nil {
		t.Fatal(err)
	}
	engine.Write("Preview.png", photo.String())
	g := NewGateway(t, engine, "")
	g.Stop()
	g.Host = host
	g.Start()
	g.Export(true)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	share := func(path string) string {
		return `\\Mac\Home\` + strings.ReplaceAll(strings.TrimPrefix(path, home+"/"), "/", `\`)
	}
	confirmed := filepath.Join(output, "gateway-edit-confirmed")
	metadata, _ := json.Marshal(map[string]string{"Link": g.PairingLink(), "ShareRoot": share(root), "Confirmed": share(confirmed), "Done": share(filepath.Join(output, "gateway-fixture-done"))})
	if err := os.WriteFile(filepath.Join(output, "gateway-fixture.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	g.ApproveNext(map[string]string{"windows-acceptance": "write"})
	WaitFor(t, 150*time.Second, "installed product acceptance", func() bool {
		// Confirm the write on the gateway's own filesystem, independent of
		// Parallels shared-folder data caching in the Windows guest.
		if data, err := os.ReadFile(filepath.Join(root, "Acceptance.txt")); err == nil && string(data) == "Edited through the installed provider" {
			if err := os.WriteFile(confirmed, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		_, err := os.Stat(filepath.Join(output, "gateway-fixture-done"))
		return err == nil
	})
}
