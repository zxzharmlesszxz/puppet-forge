package review

import (
	"context"
	"errors"
	"html"
	"regexp"
	"strings"
	"testing"
)

func TestHighlightEscapesSourceAndPreservesMultilineTokens(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{"init.pp", "template.epp", "script.rb", "data.yaml", "metadata.json", "run.sh", "README.md", "unknown.txt"} {
		t.Run(filename, func(t *testing.T) {
			source := "class demo {\n  $value = '<script>alert(1)</script>'\n}\n"
			lines, err := Highlight(t.Context(), filename, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 3 {
				t.Fatalf("line count = %d", len(lines))
			}
			code := string(lines[1].HTML)
			plain := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(code, ""))
			if strings.Contains(code, "<script>") || plain != "  $value = '<script>alert(1)</script>'" {
				t.Fatalf("unsafe or modified source: %s", code)
			}
		})
	}
}

func TestCompareLineNumbersAndEmptyFiles(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, old, new string
		added, removed int
	}{
		{"modify", "a\nb\nc\n", "a\nx\nc\n", 1, 1},
		{"added", "", "hello\n", 1, 0},
		{"removed", "hello\n", "", 0, 1},
		{"unicode", "привіт\n", "вітаю\n", 1, 1},
		{"newline", "hello", "hello\n", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hunks, err := Compare(t.Context(), "file.txt", tc.old, tc.new)
			if err != nil {
				t.Fatal(err)
			}
			added, removed := 0, 0
			for _, hunk := range hunks {
				for _, line := range hunk.Unified {
					switch line.Kind {
					case "added":
						added++
						if line.NewNumber < 1 {
							t.Fatal("missing new line number")
						}
					case "removed":
						removed++
						if line.OldNumber < 1 {
							t.Fatal("missing old line number")
						}
					}
				}
			}
			if added != tc.added || removed != tc.removed {
				t.Fatalf("changes = +%d -%d", added, removed)
			}
		})
	}
	hunks, err := Compare(t.Context(), "same.pp", "same\n", "same\n")
	if err != nil || len(hunks) != 0 {
		t.Fatalf("identical diff = %#v, %v", hunks, err)
	}
}

func TestTextReviewLimitsAndCancellation(t *testing.T) {
	t.Parallel()
	for _, text := range []string{strings.Repeat("x", MaxText+1), strings.Repeat("x\n", MaxLines+1)} {
		if _, err := Compare(t.Context(), "file.txt", "", text); !errors.Is(err, ErrLimit) {
			t.Fatalf("limit error = %v", err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Highlight(ctx, "init.pp", "class demo {}"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	for _, text := range []string{"a\x00b", string([]byte{0xff})} {
		if _, err := Highlight(t.Context(), "file.txt", text); err == nil {
			t.Fatal("binary text accepted")
		}
	}
}

func TestHighlightStartsInRootStateWithoutNormalizingNewlines(t *testing.T) {
	t.Parallel()
	for _, filename := range []string{"init.pp", "template.epp", "metadata.json", "unknown.txt"} {
		t.Run(filename, func(t *testing.T) {
			source := "alpha\r\nbeta"
			lines, err := Highlight(t.Context(), filename, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 2 {
				t.Fatalf("lines = %#v", lines)
			}
			plain := html.UnescapeString(regexp.MustCompile(`<[^>]*>`).ReplaceAllString(string(lines[0].HTML), ""))
			if plain != "alpha\r" {
				t.Fatalf("source line was normalized: %q", plain)
			}
		})
	}
}
