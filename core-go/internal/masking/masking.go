// Package masking hides private details (phone numbers, emails, OTPs, card and account numbers,
// government ids, addresses) behind placeholders such as [PHONE_1] before text goes to a cloud
// model, and puts the real values back in the model's answer. Its rules are the TypeScript core's
// regular expressions, rewritten for Go's regexp, which has no look-around: look-behinds are
// checked by hand and look-aheads are matched as a following character that is not kept. Like
// JavaScript, it measures text in UTF-16 units, so windows and limits fall on the same characters.
package masking

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/dhananjay6561/kaya/core/internal/text"
)

// Kinds of detail, in the order their rules run.
const (
	Email    = "EMAIL"
	UPI      = "UPI"
	Card     = "CARD"
	Phone    = "PHONE"
	Aadhaar  = "AADHAAR"
	PAN      = "PAN"
	IFSC     = "IFSC"
	Passport = "PASSPORT"
	Vehicle  = "VEHICLE"
	Account  = "ACCOUNT"
	Address  = "ADDRESS"
	OTP      = "OTP"
)

// Session masks the texts of one request and restores the model's answer. The same value gets
// the same placeholder in every text it masks. It is not safe for concurrent use.
type Session struct {
	byValue       map[string]string
	byPlaceholder map[string]string
	seen          map[string]int
}

// NewSession starts a session.
func NewSession() *Session {
	return &Session{byValue: map[string]string{}, byPlaceholder: map[string]string{}, seen: map[string]int{}}
}

// Counts is how many distinct values of each kind were masked.
func (s *Session) Counts() map[string]int { return maps.Clone(s.seen) }

// Mask replaces private details in text with placeholders. context is text around this one, such
// as the question an answer replied to: its words count as context (an OTP was asked for, an
// account was named). A placeholder-shaped text already in text is escaped first, so it can never
// be restored into a real value.
func (s *Session) Mask(input, context string) string {
	escaped := issuedShape.ReplaceAllString(input, "［${1}_${2}］")
	sc := newScan(escaped, context)
	for i := range rules {
		rules[i].run(sc)
	}
	slices.SortFunc(sc.spans, func(a, b span) int { return a.start - b.start })
	var b strings.Builder
	at := 0
	for _, sp := range sc.spans {
		b.WriteString(decode(sc.units[at:sp.start]))
		b.WriteString(s.placeholderFor(sp.kind, decode(sc.units[sp.start:sp.end]), decode(sc.ascii[sp.start:sp.end])))
		at = sp.end
	}
	b.WriteString(decode(sc.units[at:]))
	return b.String()
}

func (s *Session) placeholderFor(kind, value, shadow string) string {
	key := kind + ":" + normalized(kind, shadow)
	if known, ok := s.byValue[key]; ok {
		return known
	}
	s.seen[kind]++
	made := "[" + kind + "_" + strconv.Itoa(s.seen[kind]) + "]"
	s.byValue[key] = made
	s.byPlaceholder[made] = value
	return made
}

// Restore puts the original values back wherever a placeholder this session issued appears, also
// when the model changed its case, spacing, brackets or escaped its underscore. Escaped
// placeholders from the input come back as they were typed.
func (s *Session) Restore(input string) string {
	issued := func(groups []string) string {
		if value, ok := s.byPlaceholder["["+strings.ToUpper(groups[1])+"_"+groups[2]+"]"]; ok {
			return value
		}
		return groups[0]
	}
	var b strings.Builder
	at := 0
	for _, m := range escapedShape.FindAllStringSubmatchIndex(input, -1) {
		b.WriteString(restorePart(input[at:m[0]], issued))
		b.WriteString("[" + input[m[2]:m[3]] + "_" + input[m[4]:m[5]] + "]")
		at = m[1]
	}
	b.WriteString(restorePart(input[at:], issued))
	return b.String()
}

func restorePart(part string, issued func(groups []string) string) string {
	return replaceGroups(bareShape, replaceGroups(bracketedShape, part, issued), issued)
}

func replaceGroups(re *regexp.Regexp, s string, fn func(groups []string) string) string {
	return re.ReplaceAllStringFunc(s, func(match string) string {
		return fn(re.FindStringSubmatch(match))
	})
}

func decode(units []uint16) string { return string(utf16.Decode(units)) }

func normalized(kind, value string) string {
	switch kind {
	case Email, UPI:
		return strings.ToLower(value)
	case Address:
		return collapseSpace(strings.ToLower(value))
	case PAN, IFSC, Vehicle, Passport:
		return strings.Map(func(r rune) rune {
			if r == '-' || text.IsSpace(r) {
				return -1
			}
			return r
		}, strings.ToUpper(value))
	case Phone:
		digits := digitsOf(value)
		for _, prefix := range []string{"0091", "91", "0"} {
			if rest, ok := strings.CutPrefix(digits, prefix); ok && indianMobile(rest) {
				return rest
			}
		}
		return digits
	default:
		return digitsOf(value)
	}
}

// collapseSpace turns every run of white space into one space, as replace(/\s+/g, " ") does.
func collapseSpace(s string) string {
	var b strings.Builder
	inSpace := false
	for _, r := range s {
		if text.IsSpace(r) {
			if !inSpace {
				b.WriteByte(' ')
			}
			inSpace = true
			continue
		}
		inSpace = false
		b.WriteRune(r)
	}
	return b.String()
}

func indianMobile(digits string) bool {
	if len(digits) != 10 || digits[0] < '6' || digits[0] > '9' {
		return false
	}
	return strings.Trim(digits, "0123456789") == ""
}

func digitsOf(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

var kindNames = "EMAIL|UPI|CARD|PHONE|AADHAAR|PAN|IFSC|PASSPORT|VEHICLE|ACCOUNT|ADDRESS|OTP"

var (
	issuedShape    = regexp.MustCompile(`\[(` + kindNames + `)_(\d+)\]`)
	escapedShape   = regexp.MustCompile(`［(` + kindNames + `)_(\d+)］`)
	bracketedShape = regexp.MustCompile(`\[[ \t]*(` + anyCase(kindNames) + `)[ \t]*\\?[_ -][ \t]*(\d+)[ \t]*\]`)
	bareShape      = regexp.MustCompile(`\b(` + anyCase(kindNames) + `)\\?_(\d+)\b`)
)

// anyCase matches ASCII letters in either case, as JavaScript's i flag does without u: unlike
// Go's (?i), it does not let ſ stand for s or the Kelvin sign for k.
func anyCase(pattern string) string {
	var b strings.Builder
	for _, r := range pattern {
		switch {
		case r >= 'A' && r <= 'Z':
			b.WriteString("[" + string(r) + string(r+'a'-'A') + "]")
		case r >= 'a' && r <= 'z':
			b.WriteString("[" + string(r-'a'+'A') + string(r) + "]")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
