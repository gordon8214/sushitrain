package files

import (
	"github.com/google/uuid"
	"testing"
)

func TestWindowsCredentialPersistence(t *testing.T) {
	id := "test-" + uuid.NewString()
	store := WindowsCredentials{}
	defer store.DeleteCredential(id)
	if err := store.SetCredential(id, "test secret"); err != nil {
		t.Fatal(err)
	}
	got, err := (WindowsCredentials{}).Credential(id)
	if err != nil || got != "test secret" {
		t.Fatalf("read credential: %q %v", got, err)
	}
	if err := store.DeleteCredential(id); err != nil {
		t.Fatal(err)
	}
	got, err = store.Credential(id)
	if err != nil || got != "" {
		t.Fatalf("deleted credential: %q %v", got, err)
	}
}
