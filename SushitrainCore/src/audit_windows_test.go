package sushitrain

import (
	"fmt"
	"github.com/Microsoft/go-winio"
	"os"
	"testing"
)

func auditPipeFixture(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf(`\\.\pipe\Synctrain-audit-test-%d`, os.Getpid())
	listener, err := winio.ListenPipe(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return path
}
func TestAuditRejectsWindowsDevices(t *testing.T) {
	for _, path := range []string{`C:\logs\NUL`, `C:\logs\file:stream`, `\\.\NUL`, `\\?\GLOBALROOT\Device\HarddiskVolume1\file`} {
		if auditDiskPath(path) {
			t.Errorf("accepted %s", path)
		}
	}
	if !auditDiskPath(`C:\logs\audit.log`) {
		t.Fatal("rejected regular path")
	}
}
