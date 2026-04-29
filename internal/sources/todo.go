package sources

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// regexpMustCompile is a single-line alias for regexp.MustCompile so the
// package-level var blocks read like a glossary of patterns rather than a
// mess of MustCompile calls. The compile-time guarantee is the same.
func regexpMustCompile(pattern string) *regexp.Regexp { return regexp.MustCompile(pattern) }

// TODOCollector reads a repo's TODO.md and emits one Task per unchecked
// markdown task list item.
//
// What we collect:
//   - Lines like `- [ ] something` (any of -, *, +, or `1.` numbered)
//   - Multi-line indented continuations are joined into the task body
//   - Lines beginning `- [x]` (already checked) are skipped
//
// What we ignore:
//   - Headings, code fences, plain prose
//   - Files other than TODO.md at the repo root (collectors are explicit;
//     plans/*.md is a separate collector)
type TODOCollector struct {
	// Filename overrides the default "TODO.md". Tests use this.
	Filename string
}

// NewTODOCollector returns a TODOCollector for the conventional TODO.md path.
func NewTODOCollector() *TODOCollector { return &TODOCollector{Filename: "TODO.md"} }

// Collect implements Collector.
func (c *TODOCollector) Collect(_ context.Context, repo string, localPath string) ([]Task, error) {
	name := c.Filename
	if name == "" {
		name = "TODO.md"
	}
	path := filepath.Join(localPath, name)
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Missing TODO.md is fine — most repos won't have one and the
			// run shouldn't fail because of it.
			return nil, nil
		}
		return nil, fmt.Errorf("sources/todo: open %q: %w", path, err)
	}
	defer f.Close()

	var (
		out         []Task
		current     *strings.Builder
		currentMeta struct {
			line     int
			autoOK   bool
			title    string
		}
		flush = func() {
			if current == nil {
				return
			}
			body := strings.TrimSpace(current.String())
			out = append(out, Task{
				Source: state.Source{
					Type:        state.SourceTODO,
					Repo:        repo,
					// URL is the path RELATIVE to repo root + a line anchor.
				// Storing relative lets digests + PR bodies build a
				// proper github.com link (https://github.com/<repo>/blob/main/<rel>#L<n>)
				// instead of leaking the runner's absolute path
				// (/__w/.../audiobook-organizer/TODO.md#L212).
				URL:         fmt.Sprintf("%s#L%d", c.relativePath(name), currentMeta.line),
					ContentHash: state.HashContent(body),
					Title:       currentMeta.title,
				},
				Body:      body,
				HasAutoOK: currentMeta.autoOK,
			})
			current = nil
		}
	)

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024) // tolerate long lines
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if isUncheckedItem(line) {
			flush() // close any in-flight task
			// `[hold]` anywhere in the line excludes the item entirely. Used for
			// spec-pending or under-review tasks that need to stay unchecked but
			// must not be auto-executed.
			if HasHoldMarker(line) {
				current = nil
				continue
			}
			title := stripChecklistPrefix(strings.ToLower(line))
			title = stripAutoOK(title)
			title = strings.TrimSpace(title)
			current = &strings.Builder{}
			currentMeta.line = lineNo
			currentMeta.autoOK = HasAutoOKMarker(unwrapList(line))
			currentMeta.title = title
			current.WriteString(strings.TrimSpace(stripChecklistPrefix(line)))
			continue
		}
		if isCheckedItem(line) || isHeading(line) {
			flush()
			continue
		}
		// Indented continuation of an in-flight item.
		if current != nil && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			current.WriteString("\n")
			current.WriteString(strings.TrimSpace(line))
			continue
		}
		// Blank or unrelated — flush.
		if strings.TrimSpace(line) == "" {
			flush()
		}
	}
	flush()
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sources/todo: scan %q: %w", path, err)
	}
	return out, nil
}

// unwrapList strips the leading "- [ ] " / "* [ ] " / "1. [ ]" so the
// HasAutoOKMarker check sees just the post-checkbox text.
func unwrapList(line string) string {
	return checklistPrefix.ReplaceAllString(line, "")
}

// relativePath returns the configured filename. When TODOCollector is
// scanning a different file (tests override Filename), this is just that
// name. The TODO file always lives at the repo root, so the name itself
// is the rel-path.
func (c *TODOCollector) relativePath(filename string) string {
	if c.Filename != "" {
		return c.Filename
	}
	return filename
}

func isUncheckedItem(line string) bool {
	return uncheckedItem.MatchString(line)
}

func isCheckedItem(line string) bool {
	return checkedItem.MatchString(line)
}

func isHeading(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "#")
}

var (
	uncheckedItem = regexpMustCompile(`(?i)^\s*(?:[-*+]|\d+\.)\s*\[\s\]\s+\S`)
	checkedItem   = regexpMustCompile(`(?i)^\s*(?:[-*+]|\d+\.)\s*\[[xX]\]\s+`)
)
