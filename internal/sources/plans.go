package sources

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jdfalk/overnight-burndown/internal/state"
)

// PlanCollector reads each `plans/*.md` file at the repo root and emits
// one Task per file. The whole file body is the task; the filename minus
// extension becomes the title.
//
// Auto-OK eligibility: the first line of the file may be a marker comment
// `<!-- auto-ok -->` (with optional surrounding whitespace). Plan files with
// the marker have HasAutoOK=true; everything else does not.
type PlanCollector struct {
	// Glob overrides the default "plans/*.md". Tests use this.
	Glob string
}

// NewPlanCollector returns a PlanCollector for the conventional plans/*.md location.
func NewPlanCollector() *PlanCollector { return &PlanCollector{Glob: "plans/*.md"} }

// Collect implements Collector.
func (c *PlanCollector) Collect(_ context.Context, repo string, localPath string) ([]Task, error) {
	pattern := c.Glob
	if pattern == "" {
		pattern = "plans/*.md"
	}
	matches, err := filepath.Glob(filepath.Join(localPath, pattern))
	if err != nil {
		return nil, fmt.Errorf("sources/plans: glob %q: %w", pattern, err)
	}
	sort.Strings(matches) // deterministic order across runs

	var out []Task
	for _, p := range matches {
		body, err := os.ReadFile(p)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // race against the glob; skip
			}
			return nil, fmt.Errorf("sources/plans: read %q: %w", p, err)
		}
		text := string(body)
		title := strings.TrimSuffix(filepath.Base(p), ".md")
		out = append(out, Task{
			Source: state.Source{
				Type:        state.SourcePlan,
				Repo:        repo,
				URL:         p,
				ContentHash: state.HashContent(text),
				Title:       title,
			},
			Body:      text,
			HasAutoOK: hasPlanAutoOKMarker(text),
		})
	}
	return out, nil
}

// hasPlanAutoOKMarker returns true if the file body opens with an HTML
// comment marker like `<!-- auto-ok -->` (case insensitive). Surrounding
// whitespace and any leading shebang-style line are ignored.
func hasPlanAutoOKMarker(body string) bool {
	scan := strings.SplitN(body, "\n", 4)
	for _, line := range scan {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return planMarker.MatchString(line)
	}
	return false
}

var planMarker = regexpMustCompile(`(?i)^<!--\s*auto-ok\s*-->\s*$`)
