package sushitrain

import (
	"golang.org/x/sys/windows"
	"os"
	"testing"
	"unsafe"
)

func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	previous, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	original, _, err := previous.DACL()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, original, nil); err != nil {
			t.Error(err)
		}
	})
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("D:P(D;OICI;FRFX;;;" + sid + ")(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func assertPrivatePermissions(t *testing.T, path string, _ os.FileMode) {
	t.Helper()
	sid, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	// File permissions are expressed by ACLs, not Unix mode bits on Windows.
	expected, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;SY)(A;;FA;;;" + sid.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	actual, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	wanted, _, err := expected.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if actual.AceCount != wanted.AceCount {
		t.Fatalf("key grants access to unexpected principals: %s", sd.String())
	}
	for index := uint32(0); index < uint32(actual.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(actual, index, &ace); err != nil {
			t.Fatal(err)
		}
		trustee := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || (trustee != sid.User.Sid.String() && trustee != "S-1-5-18") {
			t.Fatalf("unexpected key access: %s", sd.String())
		}
	}
}
