package devicefs

import "testing"

func TestParsePathPreservesWhitespace(t *testing.T) {
	path, err := ParsePath("DCIM/100APPLE/ photo .HEIC ")
	if err != nil {
		t.Fatal(err)
	}
	if got := path.String(); got != "DCIM/100APPLE/ photo .HEIC " {
		t.Fatalf("path changed: %q", got)
	}
	if got := path.Name(); got != " photo .HEIC " {
		t.Fatalf("path name changed: %q", got)
	}
}

func TestParsePathRejectsUnsafeValues(t *testing.T) {
	for _, value := range []string{"../file", "DCIM//file", "/DCIM/file", "DCIM/file\x00suffix", `DCIM\file`} {
		if _, err := ParsePath(value); err == nil {
			t.Errorf("ParsePath(%q) succeeded", value)
		}
	}
}

func TestRootPhysicalPath(t *testing.T) {
	path, err := ParsePath("folder/file")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Media().physical(path); err != nil || got != "/folder/file" {
		t.Fatalf("media path = %q", got)
	}
	app, err := AppDocuments("com.example.app")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := app.physical(path); err != nil || got != "/Documents/folder/file" {
		t.Fatalf("app path = %q", got)
	}
}
