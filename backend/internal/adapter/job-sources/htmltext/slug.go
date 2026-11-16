package htmltext

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify converts a Vietnamese keyword into a URL slug,
// e.g. "Lập trình viên Golang" -> "lap-trinh-vien-golang".
func Slugify(keyword string) string {
	s := strings.ToLower(strings.TrimSpace(keyword))
	s = strings.NewReplacer("đ", "d", "Đ", "d").Replace(s)
	s, _, _ = transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s)
	s = nonSlugRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
