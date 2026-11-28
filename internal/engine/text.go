package engine

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// normalize folds case, punctuation and spacing so that "Geta-Deploy",
// "geta deploy" and "«geta_deploy»" compare equal.
func normalize(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	space := true
	for _, r := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '+' || r == '#' || r == '@' || r == ':' || r == '/':
			builder.WriteRune(r)
			space = false
		case !space:
			builder.WriteByte(' ')
			space = true
		}
	}
	return strings.Trim(builder.String(), " .:/")
}

// tokens returns the lowercase words of value: runs of letters and digits.
func tokens(value string) []string {
	return strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

var stopwords = func() map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(`a an and are as at be by can could did do does for from had has have how i in
		into is it its of on or our should so than that the their then there these this those to was we were what when
		where which who why will with would you your about after all also any been but if not no only over such us
		и в во на не что как по к ко с со из у за от до о об а но да же ли или то это эти этот та тот для при так
		уже есть был была были быть его ее её их мы вы он она оно они я ты нет где когда чем чтобы`) {
		words[word] = true
	}
	return words
}()

// queryTokens returns distinct searchable words of value, at most max.
func queryTokens(value string, max int) []string {
	seen := map[string]bool{}
	var result []string
	for _, token := range tokens(value) {
		if utf8.RuneCountInString(token) < 2 || stopwords[token] || seen[token] {
			continue
		}
		seen[token] = true
		result = append(result, token)
		if len(result) == max {
			break
		}
	}
	return result
}

// ftsQuery builds an FTS5 query that matches any of the words of value.
// Long words also match by prefix, which covers most inflected forms.
func ftsQuery(value string, max int) string {
	words := queryTokens(value, max)
	terms := make([]string, 0, len(words))
	for _, word := range words {
		runes := []rune(word)
		if len(runes) >= 6 {
			terms = append(terms, `"`+string(runes[:len(runes)-2])+`"*`)
			continue
		}
		terms = append(terms, `"`+word+`"`)
	}
	return strings.Join(terms, " OR ")
}

// grounded reports whether most words of claim occur in the token set.
func grounded(claim string, text map[string]bool) bool {
	words := tokens(claim)
	if len(words) == 0 {
		return false
	}
	found := 0
	for _, word := range words {
		if text[word] {
			found++
		}
	}
	return found*10 >= len(words)*6
}

func tokenSet(value string) map[string]bool {
	set := map[string]bool{}
	for _, token := range tokens(value) {
		set[token] = true
	}
	return set
}

// predicateName turns a model predicate into snake_case ASCII. It keeps
// double underscores, which namespaced predicates such as
// code_memory__invariant use.
func predicateName(value string) string {
	var builder strings.Builder
	pending := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case r == '_':
			builder.WriteRune(r)
			pending = false
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if pending && builder.Len() > 0 {
				builder.WriteByte('_')
			}
			builder.WriteRune(r)
			pending = false
		default:
			pending = true
		}
	}
	name := strings.Trim(builder.String(), "_")
	if len(name) < 2 || len(name) > 64 {
		return ""
	}
	return name
}

// predicateWords is the predicate as searchable words.
func predicateWords(predicate string) string {
	return strings.Join(strings.FieldsFunc(predicate, func(r rune) bool { return r == '_' }), " ")
}

// section is a part of a document with the headings above it.
type section struct {
	Heading string
	Text    string
}

// splitSections cuts Markdown or plain text into parts of at most max
// characters. It cuts at headings and blank lines where it can, never
// inside a fenced code block unless the block alone exceeds max.
func splitSections(text string, max int) []section {
	type block struct {
		heading string
		text    string
	}
	var blocks []block
	var headings []string
	var current []string
	fenced := false
	flush := func() {
		joined := strings.TrimSpace(strings.Join(current, "\n"))
		if joined != "" {
			blocks = append(blocks, block{heading: strings.Join(headings, " > "), text: joined})
		}
		current = current[:0]
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			current = append(current, line)
			if !fenced {
				flush()
			}
			continue
		}
		if !fenced {
			if level := headingLevel(trimmed); level > 0 {
				flush()
				if level <= len(headings) {
					headings = headings[:level-1]
				}
				for len(headings) < level-1 {
					headings = append(headings, "")
				}
				headings = append(headings, strings.TrimSpace(trimmed[level:]))
				current = append(current, line)
				continue
			}
			if trimmed == "" {
				flush()
				continue
			}
		}
		current = append(current, line)
	}
	flush()

	var sections []section
	var builder strings.Builder
	heading := ""
	emit := func() {
		if builder.Len() > 0 {
			sections = append(sections, section{Heading: compactHeading(heading), Text: builder.String()})
			builder.Reset()
		}
	}
	for _, b := range blocks {
		for _, piece := range splitLong(b.text, max) {
			if builder.Len() > 0 && builder.Len()+2+len(piece) > max {
				emit()
			}
			if builder.Len() == 0 {
				heading = b.heading
			} else {
				builder.WriteString("\n\n")
			}
			builder.WriteString(piece)
		}
	}
	emit()
	return sections
}

func compactHeading(heading string) string {
	parts := strings.Split(heading, " > ")
	kept := parts[:0]
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " > ")
}

func headingLevel(line string) int {
	level := 0
	for level < len(line) && level < 6 && line[level] == '#' {
		level++
	}
	if level == 0 || level >= len(line) || line[level] != ' ' {
		return 0
	}
	return level
}

// splitLong cuts text longer than max at line ends, then at rune
// boundaries.
func splitLong(text string, max int) []string {
	if len(text) <= max {
		return []string{text}
	}
	var pieces []string
	var builder strings.Builder
	for _, line := range strings.Split(text, "\n") {
		for len(line) > max {
			if builder.Len() > 0 {
				pieces = append(pieces, builder.String())
				builder.Reset()
			}
			cut := max
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			pieces = append(pieces, line[:cut])
			line = line[cut:]
		}
		if builder.Len() > 0 && builder.Len()+1+len(line) > max {
			pieces = append(pieces, builder.String())
			builder.Reset()
		}
		if builder.Len() > 0 {
			builder.WriteByte('\n')
		}
		builder.WriteString(line)
	}
	if builder.Len() > 0 {
		pieces = append(pieces, builder.String())
	}
	return pieces
}

// truncateRunes shortens value to at most max runes.
func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return strings.TrimSpace(string(runes[:max])) + "…"
}
