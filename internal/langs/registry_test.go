// file: internal/langs/registry_test.go
package langs

import (
	"testing"
	"testing/fstest"
)

func TestAddingALanguageIsDataOnly(t *testing.T) {
	// A brand new language, introduced with nothing but bytes. No Go code changes.
	fsys := fstest.MapFS{
		"l/rust.yaml": &fstest.MapFile{Data: []byte(`
id: rust175
display: "Rust 1.75"
image: arena/rust175:local
source_file: main.rs
compile: ["rustc", "-O", "-o", "/box/prog", "/box/main.rs"]
run: ["/box/prog"]
time_multiplier: 1.0
`)},
	}
	r := NewRegistry()
	if err := r.LoadFS(fsys, "l"); err != nil {
		t.Fatal(err)
	}
	m, ok := r.Get("rust175")
	if !ok {
		t.Fatal("rust175 not registered")
	}
	if !m.NeedsCompile() {
		t.Fatal("expected a compiled language")
	}
	if m.TimeMultiplier != 1.0 {
		t.Fatalf("multiplier = %v", m.TimeMultiplier)
	}
}

func TestDisabledManifestIsSkipped(t *testing.T) {
	fsys := fstest.MapFS{"l/x.yaml": &fstest.MapFile{Data: []byte(
		"id: x\nimage: i\nsource_file: s\nrun: [\"/x\"]\nenabled: false\n")}}
	if err := NewRegistry().LoadFS(fsys, "l"); err == nil {
		t.Fatal("expected an error when every manifest is disabled")
	}
}
