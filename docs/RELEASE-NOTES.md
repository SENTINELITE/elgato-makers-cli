# Release-note Markdown

Use UTF-8 Markdown for a local release, hosted draft or CI policy default. Notes are validated before provider delivery. Keep the source you reviewed with the release package.

Use normal Markdown for paragraphs, headings, lists, emphasis, inline code and links. A blank line starts a new paragraph. A single line break inside a paragraph renders as a line break in the Maker editor. Two spaces at the end of a line or a single trailing backslash can also mark a hard break.

Indent a child list to the start of the parent item's text. Indent the child list by two spaces under `- Parent` or three under `1. Parent`. Start each item with a text paragraph. A child list starts on the next line. Lists can be up to 49 levels deep.

```markdown
Fixed input handling on macOS.
Updated action labels in the inspector.

- Improved profile switching.
  - Kept the previous selection when a device reconnects.
  - Added feedback while a profile loads.
- Updated the setup guide.
```

Use single-line inline code. Avoid HTML, images, fenced code blocks, tabs and whitespace outside the supported break or list syntax. Plain text and ordinary Markdown lists are easiest to review in the Maker editor.
