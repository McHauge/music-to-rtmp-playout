package handlers

import (
	"bytes"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"

	"music-to-rtmp-playout/services"
)

// Templates holds the parsed template set.
type Templates struct {
	t *template.Template
}

// ThemeEntry describes one selectable UI theme.
type ThemeEntry struct {
	Name  string
	Label string
}

// themeList is the canonical ordered list of available themes (mirrors the
// reference project's palette). The settings page receives it as page data.
var themeList = []ThemeEntry{
	{"teal", "Teal"},
	{"gold", "Gold"},
	{"emerald", "Emerald"},
	{"rose", "Rose"},
	{"violet", "Violet"},
	{"sapphire", "Sapphire"},
	{"crimson", "Crimson"},
	{"slate", "Slate"},
}

// isWarnLine reports whether an import-log line should be highlighted as a
// warning. Exposed to templates as the "warn" func.
func isWarnLine(l string) bool {
	l = strings.ToLower(strings.TrimSpace(l))
	for _, p := range []string{"⚠", "warning:", "failed:", "skipped:", "error:"} {
		if strings.HasPrefix(l, p) {
			return true
		}
	}
	return false
}

// isValidTheme reports whether name is a known theme.
func isValidTheme(name string) bool {
	for _, t := range themeList {
		if t.Name == name {
			return true
		}
	}
	return false
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"add":    func(a, b int) int { return a + b },
		"printf": fmt.Sprintf,
		// warn marks an import-log line for the warning tint.
		"warn":        isWarnLine,
		"fmtDuration": fmtDuration,
		"fmtSecs":     func(sec int) string { return fmtDuration(float64(sec)) },
		// runtimeSec totals a flow's known length (songs + breaks) for the rundown
		// footer's "total estimated runtime".
		"runtimeSec": services.SumRuntimeSec,
		// remaining formats the time left of an item (clamped at zero), e.g. for
		// a "-1:31" countdown next to the now-playing progress bar.
		"remaining": func(dur, elapsed float64) string {
			r := dur - elapsed
			if r < 0 {
				r = 0
			}
			return fmtDuration(r)
		},
		// pct is the completion percentage (0–100) of elapsed within dur, for the
		// now-playing progress-bar width.
		"pct": func(elapsed, dur float64) int {
			if dur <= 0 {
				return 0
			}
			p := int(elapsed / dur * 100)
			if p < 0 {
				p = 0
			}
			if p > 100 {
				p = 100
			}
			return p
		},
	}
}

// fmtDuration renders seconds as H:MM:SS or M:SS.
func fmtDuration(sec float64) string {
	s := int(sec + 0.5)
	h := s / 3600
	m := (s % 3600) / 60
	ss := s % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, ss)
	}
	return fmt.Sprintf("%d:%02d", m, ss)
}

// LoadTemplates parses themes, partials, and pages into one set.
func LoadTemplates(dir string) (*Templates, error) {
	t := template.New("").Funcs(templateFuncs())
	for _, sub := range []string{"themes", "partials", "pages"} {
		glob := filepath.Join(dir, sub, "*.gohtml")
		var err error
		t, err = t.ParseGlob(glob)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", sub, err)
		}
	}
	return &Templates{t: t}, nil
}

// Render executes a named template into a string (for SSE fragments).
func (tp *Templates) Render(name string, data interface{}) (string, error) {
	var buf bytes.Buffer
	if err := tp.t.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Execute writes a named template to w.
func (tp *Templates) Execute(w interface{ Write([]byte) (int, error) }, name string, data interface{}) error {
	return tp.t.ExecuteTemplate(w, name, data)
}
