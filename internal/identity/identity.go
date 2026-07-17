package identity

import (
	"crypto/rand"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/rs/xid"
)

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	xidPattern  = regexp.MustCompile(`^[0-9a-v]{20}$`)
)

func NewXID() string {
	return xid.New().String()
}

func IsXID(value string) bool {
	return xidPattern.MatchString(value)
}

func IsSlug(value string) bool {
	return len(value) <= 63 && slugPattern.MatchString(value)
}

func ChunkID(documentSlug, revision string, chunk int) string {
	return documentSlug + "@" + revision + "#chunk-" + strconv.Itoa(chunk)
}

func Slug(input string) string {
	var out strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(input)) {
		replacement, ok := transliteration[r]
		if !ok {
			switch {
			case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
				replacement = string(r)
			case unicode.IsSpace(r), r == '-', r == '_', r == '/', r == '.':
				replacement = "-"
			default:
				continue
			}
		}
		for _, rr := range replacement {
			if rr == '-' {
				if out.Len() > 0 && !lastDash {
					out.WriteByte('-')
				}
				lastDash = true
				continue
			}
			out.WriteRune(rr)
			lastDash = false
		}
	}
	value := strings.Trim(out.String(), "-")
	if len(value) > 63 {
		value = strings.TrimRight(value[:63], "-")
	}
	if value == "" {
		return "item"
	}
	return value
}

func Secret(bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

var transliteration = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "yo",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "kh", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "shch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}
