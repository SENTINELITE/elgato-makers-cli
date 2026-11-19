package artifact

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifest = `{"Name":"Synthetic","UUID":"com.example.synthetic","Version":"1.0.0.4","Software":{"MinimumVersion":"6.9"},"OS":[{"Platform":"mac","MinimumVersion":"12"}]}`

func archive(t *testing.T, entries map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synthetic.streamDeckPlugin")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for name, content := range entries {
		e, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		e.Write([]byte(content))
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return p
}
func TestInspectExactArchive(t *testing.T) {
	p := archive(t, map[string]string{"com.example.synthetic.sdPlugin/manifest.json": manifest, "com.example.synthetic.sdPlugin/plugin": "synthetic"})
	v, err := Inspect(p)
	if err != nil {
		t.Fatal(err)
	}
	if v.Manifest.Version != "1.0.0.4" {
		t.Fatal("four-component version rejected")
	}
	b, _ := os.ReadFile(p)
	h := sha256.Sum256(b)
	if v.SHA256 != hex.EncodeToString(h[:]) || v.Size != int64(len(b)) {
		t.Fatal("wrong archive metadata")
	}
}
func TestInspectRejectsBadContracts(t *testing.T) {
	for _, entries := range []map[string]string{{"manifest.json": manifest}, {"com.example.synthetic.sdPlugin/manifest.json": manifest, "elsewhere/file": "bad"}, {"com.example.synthetic.sdPlugin/manifest.json": strings.Replace(manifest, "1.0.0.4", "1.0.0.4.5", 1)}, {"com.example.synthetic.sdPlugin/manifest.json": strings.Replace(manifest, "com.example.synthetic", "com.example.other", 1)}, {"com.example.synthetic.sdPlugin/manifest.json": manifest, "../outside": "bad"}, {"com.example.synthetic.sdPlugin/manifest.json": manifest, "com.example.synthetic.sdPlugin/nested/manifest.json": manifest}} {
		if _, err := Inspect(archive(t, entries)); err == nil {
			t.Fatal("accepted invalid contract")
		}
	}
}
func notes(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "notes.md")
	os.WriteFile(p, []byte(s), 0600)
	return p
}
func TestCapturedMakerPlainTextCount(t *testing.T) {
	s := "**Updates:**\r\n\r\n- Added Warden to Random Weapon keys & Weapon Dial actions, including rifle filtering & weapon artwork."
	v, err := ReadNotes(notes(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if v.PlainTextLength != 115 {
		t.Fatalf("got %d want 115", v.PlainTextLength)
	}
	if v.Markdown != s {
		t.Fatal("changed original notes")
	}
	h := sha256.Sum256([]byte(s))
	if v.SHA256 != hex.EncodeToString(h[:]) {
		t.Fatal("wrong hash")
	}
}
func TestNotesUTF16AndInline(t *testing.T) {
	v, err := ReadNotes(notes(t, "**Hi** 😀 &amp; [world](https://example.com)"))
	if err != nil {
		t.Fatal(err)
	}
	if v.PlainTextLength != 13 {
		t.Fatalf("got %d", v.PlainTextLength)
	}
}
func TestNotesRejectsUnverifiedMarkdown(t *testing.T) {
	for _, s := range []string{"<b>hi</b>", "![image](https://example.com/image)", "```\ncode\n```", "hello  world", "hello\tworld", "hello ", "`code\nspan`", "a&#32;&#32;b"} {
		if _, err := ReadNotes(notes(t, s)); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestTiptapBlockSeparators(t *testing.T) {
	for _, c := range []struct {
		markdown string
		count    int
	}{{"a\n\nb", 4}, {"- a", 5}, {"- a\n- b", 10}, {"# h\n\np", 4}, {"**hi** `x`", 4}} {
		v, err := ReadNotes(notes(t, c.markdown))
		if err != nil {
			t.Fatal(err)
		}
		if v.PlainTextLength != c.count {
			t.Fatalf("%q: got %d, want %d", c.markdown, v.PlainTextLength, c.count)
		}
	}
}
