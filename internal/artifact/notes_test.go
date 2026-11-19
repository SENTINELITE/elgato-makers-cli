package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestNotesAgainstMakerEditor(t *testing.T) {
	data, err := os.ReadFile("testdata/maker-editor-notes.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name     string
		Markdown string
		Text     string
		Count    int
	}
	if err := json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			if len(utf16.Encode([]rune(fixture.Text))) != fixture.Count {
				t.Fatal("invalid editor fixture count")
			}
			got, err := ReadNotes(notes(t, fixture.Markdown))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(fixture.Markdown))
			if got.Markdown != fixture.Markdown || got.SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatal("original Markdown or hash changed")
			}
			if got.PlainTextLength != fixture.Count {
				t.Fatalf("plain text length = %d; Maker editor = %d", got.PlainTextLength, fixture.Count)
			}
		})
	}
}

func TestNotesRejectsAmbiguousWhitespaceAndListShape(t *testing.T) {
	for _, tc := range []struct {
		name, markdown, reason string
	}{
		{"bare carriage return", "a\r\nb\rc", "Markdown line 2: tabs and standalone carriage returns"},
		{"spaces between words", "- a\n  - b  c", "Markdown line 2: repeated spaces"},
		{"arbitrary indentation", "a\n  b", "Markdown line 2: indentation"},
		{"overindented bullet", "- a\n   - b", "Markdown line 2: indentation"},
		{"underindented bullet", "- a\n - b", "Markdown line 2: indentation"},
		{"trailing single space", "a \nb", "Markdown line 1: trailing spaces"},
		{"spaces at block end", "a  \n\nb", "Markdown line 1: trailing spaces"},
		{"three trailing spaces", "a   \nb", "Markdown line 1: trailing spaces"},
		{"same line nesting", "- - b", "list items must start with a text paragraph"},
		{"heading first in item", "- # a", "list items must start with a text paragraph"},
		{"empty item", "- a\n-", "list items must start with a text paragraph"},
		{"multiline inline code", "`a\nb`", "multiline inline code"},
		{"multiline code with trimmed newline", "` a\n`", "multiline inline code"},
		{"leading inline break", "\\\na", "leading and consecutive inline breaks"},
		{"consecutive inline breaks", "a\n\\\nb", "leading and consecutive inline breaks"},
		{"table", "| A | B |\n| - | - |\n| x | y |", "Markdown outside the verified Maker subset"},
		{"strikethrough", "~~a~~\nb", "Markdown outside the verified Maker subset"},
		{"space entity next to emphasis", "a **&#32;b**", "repeated text spaces"},
		{"space entity after break", "a\n&#32;b", "whitespace at line edges"},
		{"tab entity", "a&#9;b", "control whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ReadNotes(notes(t, tc.markdown))
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("expected %q; got %v", tc.reason, err)
			}
		})
	}
}

func TestNotesRejectsListDepthThatMakerTruncates(t *testing.T) {
	var markdown strings.Builder
	for depth := range 49 {
		markdown.WriteString(strings.Repeat(" ", 2*depth) + "- a\n")
	}
	got, err := ReadNotes(notes(t, markdown.String()))
	if err != nil || got.PlainTextLength != 341 {
		t.Fatalf("49 levels: got count %d, error %v", got.PlainTextLength, err)
	}
	markdown.WriteString(strings.Repeat(" ", 2*49) + "- a")
	if _, err = ReadNotes(notes(t, markdown.String())); err == nil || !strings.Contains(err.Error(), "list nesting depth of 49") {
		t.Fatalf("50 levels: expected depth rejection; got %v", err)
	}
}
