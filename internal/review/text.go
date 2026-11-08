package review

import (
	"context"
	"errors"
	"fmt"
	"html"
	"html/template"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/aymanbagabas/go-udiff"
)

const MaxText = 64 << 10
const MaxLines = 2000
const maxMarkup = 2 << 20

var ErrLimit = errors.New("text review exceeds 64 KiB, 2000 lines, or 2 MiB of highlighted output")

type Line struct {
	Number int
	HTML   template.HTML
}
type DiffLine struct {
	OldNumber, NewNumber int
	Kind                 string
	HTML                 template.HTML
}
type DiffPair struct{ Old, New DiffLine }
type Hunk struct {
	Label   string
	Unified []DiffLine
	Split   []DiffPair
}

func textLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	if strings.HasSuffix(text, "\n") {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func validate(text string) error {
	if len(text) > MaxText || len(textLines(text)) > MaxLines {
		return ErrLimit
	}
	if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return errors.New("text preview requires UTF-8 without NUL bytes")
	}
	return nil
}

func tokenClass(kind chroma.TokenType) string {
	switch {
	case kind.InCategory(chroma.Keyword):
		return "syntax-keyword"
	case kind.InCategory(chroma.Comment):
		return "syntax-comment"
	case kind.InSubCategory(chroma.LiteralString):
		return "syntax-string"
	case kind.InSubCategory(chroma.LiteralNumber):
		return "syntax-number"
	case kind.InCategory(chroma.Name):
		return "syntax-name"
	case kind.InCategory(chroma.Operator):
		return "syntax-operator"
	default:
		return "syntax-text"
	}
}

// Highlight renders escaped source using fixed, internal syntax span classes.
func Highlight(ctx context.Context, filename, text string) ([]Line, error) {
	if err := validate(text); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lexer := lexers.Match(filename)
	if strings.HasSuffix(filename, ".epp") {
		lexer = lexers.Get("puppet")
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	iterator, err := lexer.Tokenise(&chroma.TokeniseOptions{State: "root"}, text)
	if err != nil {
		return nil, err
	}
	builders := []*strings.Builder{new(strings.Builder)}
	var reconstructed strings.Builder
	markup := 0
	for token := iterator(); token != chroma.EOF; token = iterator() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, _ = reconstructed.WriteString(token.Value)
		if reconstructed.Len() > MaxText+1 {
			return nil, ErrLimit
		}
		pieces := strings.Split(token.Value, "\n")
		for index, piece := range pieces {
			if index > 0 {
				builders = append(builders, new(strings.Builder))
			}
			if len(builders) > MaxLines+1 {
				return nil, ErrLimit
			}
			if piece == "" {
				continue
			}
			span := `<span class="` + tokenClass(token.Type) + `">` + html.EscapeString(piece) + `</span>`
			markup += len(span)
			if markup > maxMarkup {
				return nil, ErrLimit
			}
			_, _ = builders[len(builders)-1].WriteString(span)
		}
	}
	sourceLines := textLines(text)
	result := make([]Line, len(sourceLines))
	exact := reconstructed.String() == text || reconstructed.String() == text+"\n"
	for index, source := range sourceLines {
		code := html.EscapeString(source)
		if exact && index < len(builders) {
			code = builders[index].String()
		}
		// #nosec G203 -- source is escaped above; token markup has fixed span classes only.
		result[index] = Line{Number: index + 1, HTML: template.HTML(code)}
	}
	return result, nil
}

func Compare(ctx context.Context, filename, old, new string) ([]Hunk, error) {
	oldLines, err := Highlight(ctx, filename, old)
	if err != nil {
		return nil, err
	}
	newLines, err := Highlight(ctx, filename, new)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	diff, err := udiff.ToUnifiedDiff("before/"+filename, "after/"+filename, old, udiff.Strings(old, new), 3)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := make([]Hunk, 0, len(diff.Hunks))
	for _, hunk := range diff.Hunks {
		view := Hunk{Label: fmt.Sprintf("@@ -%d +%d @@", hunk.FromLine, hunk.ToLine)}
		oldNumber, newNumber := hunk.FromLine, hunk.ToLine
		for _, line := range hunk.Lines {
			var row DiffLine
			switch line.Kind {
			case udiff.Delete:
				if oldNumber < 1 || oldNumber > len(oldLines) {
					return nil, errors.New("invalid old diff line")
				}
				row = DiffLine{OldNumber: oldNumber, Kind: "removed", HTML: oldLines[oldNumber-1].HTML}
				oldNumber++
			case udiff.Insert:
				if newNumber < 1 || newNumber > len(newLines) {
					return nil, errors.New("invalid new diff line")
				}
				row = DiffLine{NewNumber: newNumber, Kind: "added", HTML: newLines[newNumber-1].HTML}
				newNumber++
			default:
				if newNumber < 1 || newNumber > len(newLines) {
					return nil, errors.New("invalid context diff line")
				}
				row = DiffLine{OldNumber: oldNumber, NewNumber: newNumber, Kind: "context", HTML: newLines[newNumber-1].HTML}
				oldNumber++
				newNumber++
			}
			view.Unified = append(view.Unified, row)
		}
		view.Split = splitRows(view.Unified)
		result = append(result, view)
	}
	return result, nil
}

func splitRows(lines []DiffLine) []DiffPair {
	var result []DiffPair
	for index := 0; index < len(lines); {
		if lines[index].Kind == "context" {
			result = append(result, DiffPair{Old: lines[index], New: lines[index]})
			index++
			continue
		}
		var removed, added []DiffLine
		for index < len(lines) && lines[index].Kind != "context" {
			if lines[index].Kind == "removed" {
				removed = append(removed, lines[index])
			} else {
				added = append(added, lines[index])
			}
			index++
		}
		for offset := range max(len(removed), len(added)) {
			pair := DiffPair{}
			if offset < len(removed) {
				pair.Old = removed[offset]
			}
			if offset < len(added) {
				pair.New = added[offset]
			}
			result = append(result, pair)
		}
	}
	return result
}
