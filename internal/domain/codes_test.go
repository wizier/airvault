package domain

import (
	"os"
	"regexp"
	"testing"
)

// The SPA shows a code it has no text for as a generic failure.
func TestEveryCodeHasAText(t *testing.T) {
	codes := matches(t, "codes.go", `Code = "([a-z0-9_]+)"`)
	if len(codes) == 0 {
		t.Fatal("codes.go declares no codes")
	}
	texts := map[string]bool{}
	for _, key := range matches(t, "../../web/src/lib/error-text.ts", `(?m)^  ([a-z0-9_]+):`) {
		texts[key] = true
	}
	for _, code := range codes {
		if !texts[code] {
			t.Errorf("code %q has no text in web/src/lib/error-text.ts", code)
		}
	}
}

// matches is what pattern's group captures in the file at path.
func matches(t *testing.T, path, pattern string) []string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, match := range regexp.MustCompile(pattern).FindAllStringSubmatch(string(source), -1) {
		found = append(found, match[1])
	}
	return found
}
