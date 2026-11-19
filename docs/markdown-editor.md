# Maker Markdown editor compatibility

Maker's release form supplies the Markdown string and a separate plaintext length. The CLI retains every input byte and its SHA-256. It normalizes CRLF to LF for parsing and counts the editor's plaintext in UTF-16 code units. The same reader validates product descriptions. The package and the notes are independent inputs.

## Verified formatting

Paragraphs, headings, emphasis, one-line inline code and links are supported. A single newline within a paragraph or list item produces a line break. A backslash or exactly two spaces before an inline newline also produces a line break. Each break contributes one UTF-16 code unit. A blank line separates paragraphs.

Ordered and unordered lists can be nested and mixed. Start each item with a text paragraph. Put each sublist marker or indented continuation under the first character of the parent item's text. For example, indent two spaces under a `- ` marker and three under a `1. ` marker. The editor represents each item with a paragraph. It inserts two newlines before every block after the first, including the list containers, items and paragraphs.

The reader rejects tabs, standalone carriage returns, repeated spaces, unaligned indentation, and trailing spaces outside the two-space break syntax. Repeated, edge, and control whitespace introduced by character references is also restricted. Empty list items and items beginning with a heading or a same-line sublist are rejected because Maker may insert a paragraph or reparent those blocks. At the Markdown parser's nesting limit, level 50 loses the deepest item text. The reader accepts up to 49 levels. Leading and consecutive inline breaks are rejected because the editor can drop them or turn them into paragraph separators when it serializes. HTML, images, fenced code blocks, tables, strikethrough and multiline inline code remain outside the verified subset.

## Reference measurements

The fixtures in [maker-editor-notes.json](../internal/artifact/testdata/maker-editor-notes.json) were generated using the public Maker bundle's editor, running under Node 24.3.0 and jsdom 26.1.0 in an isolated temporary directory. For each synthetic input, the harness created an editor from the bundled extension factory, read `editor.getText()` and its JavaScript `.length`, and read `editor.storage.markdown.getMarkdown()`. The fixture records each result. Maker itself serializes both hard-break spellings as an unadorned newline.

| Markdown | Editor count |
| --- | ---: |
| `a` and `b` separated by a line break | 3 |
| `a` and `b` in separate paragraphs | 4 |
| Two flat bullet items with one letter each | 10 |
| A bullet item with one nested bullet and one letter in each | 12 |
| Three levels of mixed lists followed by a top-level sibling | 24 |
| Combined CRLF, both hard-break spellings, nesting, inline formatting and emoji | 69 |
| Historical flat-list capture | 115 |

The bundled Markdown extension uses Markdown-it with `breaks: true` and a Tiptap hard-break extension. The editor emits `getText()` to the form, which assigns its `.length` to `release_notesPlainTextLength`. The public chunk files identify the exact tested build. The editor extensions were loaded from webpack module 41522 and the Tiptap core from module 48349. [Maker editor bundle](https://maker.elgato.com/_next/static/chunks/5195-44b38d28930241c9.js) · [Tiptap core bundle](https://maker.elgato.com/_next/static/chunks/22747d63-0c964c4337f60ce9.js) · [Captured release form bundle](https://maker.elgato.com/_next/static/chunks/294-f6f53991b2b06fd4.js)

| Public asset | SHA-256 |
| --- | --- |
| `5195-44b38d28930241c9.js` | `1a32d0a6cb55dce7fc0d2df40a2c368d5f9854a1ba8e242d38edd7590f3c58df` |
| `22747d63-0c964c4337f60ce9.js` | `7dc429b132dfc0fd1a2c1ff9b86a89282df1646901eecc600f4feaf70cc1d4f1` |
| `294-f6f53991b2b06fd4.js` | `f8f7958e15b60dad6309fb31781cb39dad287d38834d5bc3e723c496f7042352` |

The Go regression tests compare the CLI count to the recorded editor output and check that Markdown and SHA-256 remain byte-exact. They run without Node or network access.

```sh
go test ./internal/artifact -run 'TestNotes|TestCapturedMakerPlainTextCount'
```
