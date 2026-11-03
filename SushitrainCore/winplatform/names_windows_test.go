package winplatform

import "testing"

func TestWindowsComponentNames(t *testing.T) {
	for _, name := range []string{"CON", "con.txt", "LPT¹.log", "COM9", "NUL", "CONIN$", "CONOUT$", "trailing.", "trailing ", "stream:ads", "..", "a/b", "a\\b", "a\x01b"} {
		if ValidComponent(name) {
			t.Errorf("accepted reserved name %q", name)
		}
	}
	for _, name := range []string{"my file.txt", "console.txt", "COM10", "日本語.txt", "emoji 🍣.txt"} {
		if !ValidComponent(name) {
			t.Errorf("refused valid name %q", name)
		}
	}
}
