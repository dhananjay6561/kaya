package masking

import (
	"regexp"
	"strings"
)

// Word lists, matched on text whose ASCII letters are lowered, as the TypeScript ones were matched
// with the i flag.
var (
	strongOTPWords = regexp.MustCompile(`otp|passcode|pass code|one[` + space + `-]?time|verification|login code|log in code`)
	weakOTPBefore  = regexp.MustCompile(`\b(?:code|pin|mpin)\b[` + space + `:=-]*(?:is|hai|h|:)?[` + space + `:=-]*$`)
	weakOTPAfter   = regexp.MustCompile(`^[` + space + `]*(?:is|hai)?[` + space + `]*(?:your|the|aapka|tumhara)?[` + space +
		`]*(?:\w+[` + space + `]+){0,2}(?:code|pin|otp)\b`)
	moneyBefore  = regexp.MustCompile(`(?:rs\.?|inr|₹|\$|usd|eur|€)[` + space + `]*$`)
	accountWords = regexp.MustCompile(`a/c|acc(?:oun)?t|acct|khata|ifsc|neft|imps|rtgs|bank|hdfc|icici|sbi|axis|kotak|pnb|canara|idfc|indusind|yes[` +
		space + `]?bank|bob|federal`)
	flatOrPlot = regexp.MustCompile(`^(?:flat|plot)\b`)
	numbered   = regexp.MustCompile(`\b(?:no|number)\b|#`)
	year       = regexp.MustCompile(`^(?:19|20)\d{2}$`)

	indianMobileDigits = regexp.MustCompile(`^(?:91|0)?[6-9]\d{9}$`)
	landlineDigits     = regexp.MustCompile(`^0\d{9,10}$`)
	stdCode            = regexp.MustCompile(`^\(?0\d{2,4}\)?[ -]`)
	international      = regexp.MustCompile(`^[` + space + `]*(?:\+|00)`)
)

func luhn(digits string) bool {
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

// wordStart is a \b before a match that starts with a word character.
func wordStart(sc *scan, i int) bool { return !isWord(sc.runeAt(i - 1)) }

// notAfterNumber is (?<!X[ -]?), with X a digit or, when plus is set, a plus sign too.
func notAfterNumber(plus bool) func(sc *scan, i int) bool {
	number := func(r rune) bool { return isDigit(r) || plus && r == '+' }
	return func(sc *scan, i int) bool {
		prev := sc.runeAt(i - 1)
		if number(prev) {
			return false
		}
		return !((prev == ' ' || prev == '-') && number(sc.runeAt(i-2)))
	}
}

// notFollowedByNumber is (?![ -]?\d) written as what may follow instead.
const notFollowedByNumber = `(?:$|[^\d -]|[ -](?:$|\D))`

var rules = []rule{
	newRule(Email, `[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`, ""),
	with(newRule(UPI, `[A-Za-z0-9._-]{2,}@[A-Za-z]{2,}`, ""), func(r *rule) {
		// (?![\w-]*\.[A-Za-z]{2,})(?![A-Za-z0-9]): the domain is a whole run of letters, so only
		// its full length can pass, and it must not go on into a dotted domain.
		r.after = func(sc *scan, end int) bool {
			next := sc.runeAt(end)
			if isWord(next) && next != '_' {
				return false
			}
			k := end
			for isWord(sc.runeAt(k)) || sc.runeAt(k) == '-' {
				k++
			}
			return !(sc.runeAt(k) == '.' && isLetter(sc.runeAt(k+1)) && isLetter(sc.runeAt(k+2)))
		}
	}),
	with(newRule(Card, `\d(?:[ .-]?\d){12,18}`, `(?:$|\D)`), func(r *rule) {
		r.longest = 37
		r.before = func(sc *scan, i int) bool { p := sc.runeAt(i - 1); return !isDigit(p) && p != '.' }
		r.accept = func(_ *scan, value string, _, _ int) bool { return luhn(digitsOf(value)) }
	}),
	with(newRule(Phone, `(?:(?:\+|00)[ ]?)?\(?\d(?:[ ().-]{0,2}\d){7,14}`, ""), func(r *rule) {
		r.longest = 47
		r.accept = func(sc *scan, value string, start, end int) bool {
			if isDigit(sc.runeAt(start-1)) || isDigit(sc.runeAt(end)) {
				return false
			}
			digits := digitsOf(value)
			if international.MatchString(value) {
				return len(digits) >= 8 && len(digits) <= 15
			}
			if indianMobileDigits.MatchString(digits) {
				return true
			}
			return landlineDigits.MatchString(digits) && stdCode.MatchString(value)
		}
	}),
	with(newRule(Aadhaar, `[2-9]\d{3}[ -]?\d{4}[ -]?\d{4}`, notFollowedByNumber), func(r *rule) {
		r.longest = 14
		r.before = notAfterNumber(true)
	}),
	with(newRule(PAN, `[A-Za-z]{5}\d{4}[A-Za-z]\b`, ""), func(r *rule) {
		r.longest = 10
		r.before = wordStart
	}),
	with(newRule(IFSC, `[A-Za-z]{4}0[A-Za-z0-9]{6}\b`, ""), func(r *rule) {
		r.longest = 11
		r.before = wordStart
		r.accept = func(_ *scan, value string, _, _ int) bool {
			return strings.ContainsAny(string(utf16Tail(value, 5)), "0123456789")
		}
	}),
	with(newRule(Vehicle, `[A-Z]{2}[ -]?\d{1,2}[ -]?[A-Z]{1,2}(?:[ -]?[A-Z]{1,3})?[ -]?\d{4}\b`, ""), func(r *rule) {
		r.longest = 17
		r.before = wordStart
	}),
	with(newRule(Passport, `[A-Z][1-9]\d{6}\b`, ""), func(r *rule) {
		r.longest = 8
		r.before = wordStart
	}),
	with(newRule(Account, `\d(?:[ ]?\d){8,17}`, notFollowedByNumber), func(r *rule) {
		r.longest = 35
		r.before = notAfterNumber(false)
		r.accept = func(sc *scan, _ string, start, end int) bool {
			return accountWords.MatchString(sc.around(start, end, 40, 25)) || accountWords.MatchString(sc.context)
		}
	}),
	with(newRule(Address, `(?:flat|plot|house|h\.?[ \t]?no|door|shop)\.?[ \t]{0,3}(?:no\.?|number|#)?[ \t]{0,3}[:-]?[ \t]{0,3}[a-z]?-?\d+[a-z]?(?:[/-]\d+[a-z]?)?`, ""), func(r *rule) {
		r.lower = true
		r.before = wordStart
		r.accept = func(_ *scan, value string, _, _ int) bool {
			v := lowered(value)
			return flatOrPlot.MatchString(v) || numbered.MatchString(v)
		}
	}),
	with(newRule(Address, `(?:pin[ \t]?code|pincode|postal code|zip)[ \t]{0,3}[:-]?[ \t]{0,3}[1-9]\d{2}[ \t]?\d{3}\b`, ""), func(r *rule) {
		r.longest = 40
		r.lower = true
	}),
	with(newRule(Address, `(?:[A-Za-z]{1,2}-?)?\d{1,4}[A-Za-z]?(?:[/-]\d{1,4})?,?[ \t][^\n]{2,120}`, ""), func(r *rule) {
		r.longest = 140
		r.find = regexp.MustCompile(`(?:[A-Za-z]{1,2}-?)?\d{1,4}[A-Za-z]?(?:[/-]\d{1,4})?,?[ \t]`)
		r.clip = true
		r.before = lineStart
		r.accept = namesAPlace
	}),
	with(newRule(Address, `[^\n,]{0,60}\b[1-9]\d{2}[ \t]?\d{3}\b`, ""), func(r *rule) {
		r.candidate = beforePIN
		r.exactAt = pinWithin60
		r.clip = true
		r.accept = namesAPlace
	}),
	with(newRule(OTP, `\d(?:[ -]?\d){3,7}`, `(?:$|[^\d,:/%.]|\.(?:$|\D))`), func(r *rule) {
		r.longest = 15
		r.before = func(sc *scan, i int) bool {
			p := sc.runeAt(i - 1)
			if isDigit(p) || strings.ContainsRune(",/_.", p) {
				return false
			}
			return !(p == ':' && isDigit(sc.runeAt(i-2)))
		}
		r.accept = func(sc *scan, value string, start, end int) bool {
			if year.MatchString(digitsOf(value)) {
				return false
			}
			if moneyBefore.MatchString(lowered(sc.slice(start-6, start))) {
				return false
			}
			if strongOTPWords.MatchString(sc.around(start, end, 40, 40)) || strongOTPWords.MatchString(sc.context) {
				return true
			}
			return weakOTPBefore.MatchString(lowered(sc.slice(start-16, start))) ||
				weakOTPAfter.MatchString(lowered(sc.slice(end, end+40)))
		}
	}),
}

func with(r rule, set func(r *rule)) rule {
	set(&r)
	return r
}

func isLetter(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }

// lineStart is (?<=^|[,:;\n][ \t]{0,3}) with ^ matching after any JavaScript line break.
func lineStart(sc *scan, i int) bool {
	if i == 0 || isLineBreak(sc.runeAt(i-1)) {
		return true
	}
	for k := 0; k <= 3; k++ {
		r := sc.runeAt(i - k - 1)
		if strings.ContainsRune(",:;\n", r) && r >= 0 {
			return true
		}
		if r != ' ' && r != '\t' {
			return false
		}
	}
	return false
}

var pinCode = regexp.MustCompile(`[1-9]\d{2}[ \t]?\d{3}`)

// beforePIN is the earliest place a match of [^\n,]{0,60}\b<PIN> can start: up to 60 units before
// the next PIN-shaped number, back to the last line break or comma.
func beforePIN(sc *scan, hay string, p int) (int, bool) {
	pin, ok := firstAt(pinCode, sc, hay, p)
	if !ok {
		return 0, false
	}
	start := pin
	for start > p && pin-start < 60 && sc.runes[start-1] != '\n' && sc.runes[start-1] != ',' {
		start--
	}
	return start, true
}

// placeWords are the words that make a line an address, as \b(?:road|...|phase)\b.
var placeWords = strings.Fields("road rd street st marg nagar sector sec colony lane gali block apartments apartment apts apt " +
	"society chowk tower floor layout enclave vihar bagh ganj puram village district mohalla extension ext cross main phase")

// namesAPlace reports whether the match names a road, nagar, sector and the like as a whole word.
// It looks the words up once per text instead of running the word list on every match tried.
func namesAPlace(sc *scan, _ string, start, end int) bool {
	if sc.places == nil {
		sc.places = make([][]int, len(sc.runes))
		for i := range sc.runes {
			for _, w := range placeWords {
				if hasWordAt(sc.runes, i, w) {
					sc.places[i] = append(sc.places[i], len(w))
				}
			}
		}
	}
	for a := start; a < end; a++ {
		if a > start && isWord(sc.runes[a-1]) {
			continue
		}
		for _, n := range sc.places[a] {
			if b := a + n; b == end || b < end && !isWord(sc.runes[b]) {
				return true
			}
		}
	}
	return false
}

func hasWordAt(runes []rune, i int, w string) bool {
	if i+len(w) > len(runes) {
		return false
	}
	for k := range len(w) {
		if asciiLower(runes[i+k]) != rune(w[k]) {
			return false
		}
	}
	return true
}

// pinWithin60 is [^\n,]{0,60}\b[1-9]\d{2}[ \t]?\d{3}\b at start, written out: the PIN that
// starts furthest away wins, as the greedy prefix makes it.
func pinWithin60(sc *scan, start int) (int, bool) {
	most := 0
	for most < 60 && start+most < len(sc.runes) && sc.runes[start+most] != '\n' && sc.runes[start+most] != ',' {
		most++
	}
	for k := most; k >= 0; k-- {
		if end, ok := pinAt(sc, start+k); ok {
			return end, true
		}
	}
	return 0, false
}

func pinAt(sc *scan, q int) (int, bool) {
	r := sc.runeAt
	if r(q) < '1' || r(q) > '9' || isWord(r(q-1)) || !isDigit(r(q+1)) || !isDigit(r(q+2)) {
		return 0, false
	}
	for _, gap := range []int{1, 0} {
		at := q + 3
		if gap == 1 && r(at) != ' ' && r(at) != '\t' {
			continue
		}
		at += gap
		if isDigit(r(at)) && isDigit(r(at+1)) && isDigit(r(at+2)) && !isWord(r(at+3)) {
			return at + 3, true
		}
	}
	return 0, false
}

// utf16Tail is value from its n-th UTF-16 unit on, as JavaScript's slice(n).
func utf16Tail(value string, n int) []rune {
	units := utf16Encode(value)
	if n >= len(units) {
		return nil
	}
	return utf16Decode(units[n:])
}
