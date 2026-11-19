package artifact

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/SENTINELITE/elgato-makers-cli/internal/model"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	goldhtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
)

// ReadNotes preserves the original Markdown bytes and counts the text in the
// verified Maker editor subset. See docs/markdown-editor.md for that contract.
func ReadNotes(file string) (model.Notes, error) {
	result := model.Notes{}
	abs, err := filepath.Abs(file)
	if err != nil {
		return result, err
	}
	result.Path = abs
	f, err := os.Open(file)
	if err != nil {
		return result, errors.New("cannot read release notes")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return result, errors.New("cannot read release notes")
	}
	if len(b) > 1024*1024 {
		return result, errors.New("release notes exceed 1 MiB")
	}
	if !utf8.Valid(b) {
		return result, errors.New("release notes must be UTF-8")
	}
	result.Markdown = string(b)
	h := sha256.Sum256(b)
	result.SHA256 = hex.EncodeToString(h[:])

	source := []byte(strings.ReplaceAll(result.Markdown, "\r\n", "\n"))
	result.PlainTextLength, err = notesPlainTextLength(source)
	return result, err
}

type notesCounter struct {
	source       []byte
	indent       map[int]int
	hardBreak    map[int]bool
	seenBlock    bool
	atLineStart  bool
	lastWasSpace bool
	length       int
	escaped      bytes.Buffer
	writer       *bufio.Writer
}

func notesPlainTextLength(source []byte) (int, error) {
	lines := strings.Split(string(source), "\n")
	for i, line := range lines {
		if strings.ContainsAny(line, "\t\r") {
			return 0, fmt.Errorf("Markdown line %d: tabs and standalone carriage returns are unsupported", i+1)
		}
		if strings.Contains(strings.Trim(line, " "), "  ") {
			return 0, fmt.Errorf("Markdown line %d: repeated spaces are unsupported", i+1)
		}
	}
	counter := notesCounter{
		source:    source,
		indent:    make(map[int]int),
		hardBreak: make(map[int]bool),
	}
	counter.writer = bufio.NewWriter(&counter.escaped)
	// Parse these extensions so their nodes can be rejected. Maker parses
	// them as formatting even though they are outside our verified subset.
	doc := goldmark.New(goldmark.WithExtensions(extension.Table, extension.Strikethrough)).
		Parser().Parse(text.NewReader(source))
	if err := counter.walk(doc, 0, 0); err != nil {
		return 0, err
	}
	offset := 0
	for i, line := range lines {
		content := strings.TrimLeft(line, " ")
		leading := len(line) - len(content)
		if leading > 0 && leading != counter.indent[offset] {
			return 0, fmt.Errorf("Markdown line %d: indentation must align with a list item", i+1)
		}
		trimmed := strings.TrimRight(content, " ")
		trailing := len(content) - len(trimmed)
		if trailing > 0 && (trailing != 2 || !counter.hardBreak[offset]) {
			return 0, fmt.Errorf("Markdown line %d: trailing spaces are unsupported except for a two-space line break", i+1)
		}
		offset += len(line) + 1
	}
	return counter.length, nil
}

func (c *notesCounter) walk(node ast.Node, listIndent, listDepth int) error {
	switch v := node.(type) {
	case *ast.Document, *ast.Heading:
	case *ast.List:
		listDepth++
		// Maker's markdown-it parser truncates item content at its nesting
		// limit of 100 blocks. A list and its item use two levels.
		if listDepth > 49 {
			return errors.New("release notes exceed the verified Maker list nesting depth of 49")
		}
	case *ast.ListItem:
		switch v.FirstChild().(type) {
		case *ast.Paragraph, *ast.TextBlock:
		default:
			return errors.New("list items must start with a text paragraph")
		}
		listIndent += v.Offset
	case *ast.Paragraph, *ast.TextBlock:
		c.recordListIndent(node, listIndent)
	case *ast.Emphasis, *ast.Link:
	case *ast.CodeSpan:
		if child, ok := v.FirstChild().(*ast.Text); ok {
			last := child.Segment.Stop
			// Goldmark can trim the final newline from the span's last
			// segment before we inspect its text.
			if v.ChildCount() > 1 || last < len(c.source) && c.source[last] == '\n' {
				return errors.New("multiline inline code is outside the verified Maker Markdown subset")
			}
		}
	case *ast.Text:
		return c.addText(v)
	case *ast.String:
		return c.countText(string(v.Value))
	default:
		return errors.New("release notes contain Markdown outside the verified Maker subset")
	}
	if node.Type() == ast.TypeBlock {
		// Tiptap adds two newlines before every block after the first,
		// including list containers, list items and their paragraphs.
		if c.seenBlock {
			c.length += 2
		}
		c.seenBlock = true
		c.atLineStart = true
		c.lastWasSpace = false
	}
	for child := node.FirstChild(); child != nil; child = child.NextSibling() {
		if err := c.walk(child, listIndent, listDepth); err != nil {
			return err
		}
	}
	switch node.(type) {
	case *ast.Paragraph, *ast.TextBlock, *ast.Heading:
		if c.lastWasSpace {
			return errors.New("rendered text has unsupported whitespace at line edges")
		}
	}
	return nil
}

func (c *notesCounter) recordListIndent(node ast.Node, listIndent int) {
	item, ok := node.Parent().(*ast.ListItem)
	if !ok {
		return
	}
	for i := 0; i < node.Lines().Len(); i++ {
		segment := node.Lines().At(i)
		start := c.lineStart(segment.Start)
		// A lazy continuation can start in column zero. Indented content must
		// line up with the text column of this item, or with its marker on
		// the first line.
		if segment.Start-start == listIndent {
			if i == 0 && item.FirstChild() == node {
				c.indent[start] = listIndent - item.Offset
			} else {
				c.indent[start] = listIndent
			}
		}
	}
}

func (c *notesCounter) lineStart(offset int) int {
	return bytes.LastIndexByte(c.source[:offset], '\n') + 1
}

func (c *notesCounter) addText(node *ast.Text) error {
	value := node.Value(c.source)
	if bytes.ContainsAny(value, "\r\n\t") {
		return errors.New("multiline inline code is outside the verified Maker Markdown subset")
	}
	if node.SoftLineBreak() {
		// Goldmark marks an escaped backslash pair followed by an odd
		// backslash as a soft break. Maker consumes that last backslash.
		before := bytes.TrimRight(value, "\\")
		if (len(value)-len(before))%2 == 1 {
			value = value[:len(value)-1]
		}
	}
	rendered := string(value)
	if !node.IsRaw() {
		// Goldmark's writer resolves punctuation escapes and references in
		// one pass. Decode the HTML text once, preserving escaped entities.
		goldhtml.DefaultWriter.Write(c.writer, value)
		_ = c.writer.Flush() // bytes.Buffer writes cannot fail.
		rendered = html.UnescapeString(c.escaped.String())
		c.escaped.Reset()
	}
	if err := c.countText(rendered); err != nil {
		return err
	}
	if node.HardLineBreak() {
		c.hardBreak[c.lineStart(node.Segment.Start)] = true
	}
	if node.SoftLineBreak() || node.HardLineBreak() {
		if c.atLineStart {
			return errors.New("leading and consecutive inline breaks are outside the verified Maker Markdown subset")
		}
		// Maker parses both Markdown break forms as a Tiptap hardBreak
		// node, whose editor text renderer returns one newline.
		c.length++
		c.atLineStart = true
		c.lastWasSpace = false
	}
	return nil
}

func (c *notesCounter) countText(value string) error {
	for _, r := range value {
		switch r {
		case ' ':
			if c.lastWasSpace {
				return errors.New("repeated text spaces are outside the verified Maker Markdown subset")
			}
			if c.atLineStart {
				return errors.New("rendered text has unsupported whitespace at line edges")
			}
		case '\r', '\n', '\t', '\f':
			return errors.New("rendered text has unsupported control whitespace")
		}
		c.lastWasSpace = r == ' '
		c.atLineStart = false
		c.length++
		if r > 0xffff {
			c.length++
		}
	}
	return nil
}
