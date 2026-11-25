// Package o200k counts tokens the way OpenAI's o200k encoding splits text, which is what the
// gpt-oss models read. It matches gpt-tokenizer, which the TypeScript core used, token for token.
package o200k

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"unicode/utf8"
)

// The merge ranks published with tiktoken (MIT licence), from
// https://openaipublic.blob.core.windows.net/encodings/o200k_base.tiktoken.
//
//go:embed o200k_base.tiktoken
var ranksFile []byte

// space is Unicode's White_Space: unlike JavaScript's \s it has U+0085 and not U+FEFF.
const space = `\t\n\v\f\r \x{85}\x{a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}`

type encoding struct {
	ranks map[string]int
	split *regexp.Regexp
}

var load = sync.OnceValue(func() *encoding {
	ranks, err := readRanks(ranksFile)
	if err != nil {
		panic(err)
	}
	return &encoding{ranks: ranks, split: splitPattern()}
})

// Load reads the ranks now rather than on the first count. It is safe to call more than once.
func Load() { load() }

// Count is how many tokens text takes, with no chat framing. Text that looks like a special
// token, such as "<|endoftext|>", is counted as plain text.
func Count(text string) int {
	e := load()
	n := 0
	for start := 0; start < len(text); {
		end := e.next(text, start)
		n += e.pieceTokens(text[start:end])
		start = end
	}
	return n
}

// splitPattern is the o200k split pattern written for Go, which has no look-ahead. Its last two
// branches, \s+(?!\S)|\s+, become one captured run of white space that next gives a character
// back from.
func splitPattern() *regexp.Regexp {
	contraction := `(?:'[sSſ]|'[dD]|'[mM]|'[tT]|'[lL][lL]|'[vV][eE]|'[rR][eE])?`
	notLetterOrNumber := `[^\r\n` + letters + numbers + `]?`
	return regexp.MustCompile(`^(?:` +
		notLetterOrNumber + `[` + upper + `]*[` + lower + `]+` + contraction +
		`|` + notLetterOrNumber + `[` + upper + `]+[` + lower + `]*` + contraction +
		`|[` + numbers + `]{1,3}` +
		`| ?[^` + space + letters + numbers + `]+[\r\n/]*` +
		`|[` + space + `]*[\r\n]+` +
		`|([` + space + `]+))`)
}

// next finds where the piece starting at start ends.
func (e *encoding) next(text string, start int) int {
	m := e.split.FindStringSubmatchIndex(text[start:])
	if m == nil || m[1] == 0 {
		panic(fmt.Sprintf("o200k: no progress at byte %d", start))
	}
	end := start + m[1]
	if m[2] >= 0 && end < len(text) {
		// A run of white space before other text leaves its last character to that text, as
		// \s+(?!\S) does; a run of one character stays whole.
		_, size := utf8.DecodeLastRuneInString(text[:end])
		if end-size > start {
			end -= size
		}
	}
	return end
}

func readRanks(file []byte) (map[string]int, error) {
	ranks := make(map[string]int, 200_000)
	lines := bufio.NewScanner(bytes.NewReader(file))
	for lines.Scan() {
		token, rank, ok := bytes.Cut(lines.Bytes(), []byte{' '})
		if !ok {
			return nil, fmt.Errorf("o200k: bad line %q", lines.Bytes())
		}
		b, err := base64.StdEncoding.DecodeString(string(token))
		if err != nil {
			return nil, fmt.Errorf("o200k: bad token %q: %w", token, err)
		}
		r, err := strconv.Atoi(string(rank))
		if err != nil {
			return nil, fmt.Errorf("o200k: bad rank %q: %w", rank, err)
		}
		ranks[string(b)] = r
	}
	return ranks, lines.Err()
}
