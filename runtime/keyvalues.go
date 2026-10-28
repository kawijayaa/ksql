// Package runtime contains pure-Go evaluation helpers that applications can
// register as database functions for KSQL capabilities SQL cannot express.
package runtime

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// MaxKeyValueInputBytes bounds one parse-kv input value.
const MaxKeyValueInputBytes = 1 << 20

// KeyValueOptions is the JSON options document KSQL emits for parse-kv.
// Mode is "delimited", "free" or "regex". Quotes hold one- or two-rune
// open/close pairs; Escapes hold single runes. Keys are the requested names.
type KeyValueOptions struct {
	Mode    string   `json:"mode"`
	Pair    string   `json:"pair,omitempty"`
	KV      string   `json:"kv,omitempty"`
	Quotes  []string `json:"quotes,omitempty"`
	Escapes []string `json:"escapes,omitempty"`
	Greedy  bool     `json:"greedy,omitempty"`
	Regex   string   `json:"regex,omitempty"`
	Keys    []string `json:"keys"`
}

type quotePair struct{ open, close rune }

type keyValueParser struct {
	options KeyValueOptions
	quotes  []quotePair
	escapes map[rune]bool
	wanted  map[string]bool
	regex   *regexp.Regexp
}

var (
	parserCache   sync.Map
	parserEntries int
	parserMu      sync.Mutex
)

// KeyValuesJSON evaluates parse-kv for one value and returns a JSON object
// with the first value of each requested key that occurs. It is suitable for
// registration as a deterministic SQL function taking (value, options).
func KeyValuesJSON(value, options string) (string, error) {
	parser, err := cachedParser(options)
	if err != nil {
		return "", err
	}
	if len(value) > MaxKeyValueInputBytes {
		return "", errors.New("parse-kv input exceeds 1 MiB")
	}
	encoded, err := json.Marshal(parser.parse(value))
	return string(encoded), err
}

// ParseKeyValues evaluates parse-kv for one value.
func ParseKeyValues(value string, options KeyValueOptions) (map[string]string, error) {
	parser, err := newKeyValueParser(options)
	if err != nil {
		return nil, err
	}
	return parser.parse(value), nil
}

func cachedParser(options string) (*keyValueParser, error) {
	if cached, ok := parserCache.Load(options); ok {
		return cached.(*keyValueParser), nil
	}
	var decoded KeyValueOptions
	if err := json.Unmarshal([]byte(options), &decoded); err != nil {
		return nil, errors.New("invalid parse-kv options")
	}
	parser, err := newKeyValueParser(decoded)
	if err != nil {
		return nil, err
	}
	parserMu.Lock()
	defer parserMu.Unlock()
	if parserEntries < 256 {
		parserCache.Store(options, parser)
		parserEntries++
	}
	return parser, nil
}

func newKeyValueParser(options KeyValueOptions) (*keyValueParser, error) {
	parser := &keyValueParser{options: options, escapes: map[rune]bool{}, wanted: map[string]bool{}}
	for _, key := range options.Keys {
		parser.wanted[key] = true
	}
	for _, quote := range options.Quotes {
		runes := []rune(quote)
		switch len(runes) {
		case 1:
			parser.quotes = append(parser.quotes, quotePair{runes[0], runes[0]})
		case 2:
			parser.quotes = append(parser.quotes, quotePair{runes[0], runes[1]})
		default:
			return nil, errors.New("parse-kv quote must have one or two characters")
		}
	}
	for _, escape := range options.Escapes {
		if utf8.RuneCountInString(escape) != 1 {
			return nil, errors.New("parse-kv escape must be one character")
		}
		r, _ := utf8.DecodeRuneInString(escape)
		parser.escapes[r] = true
	}
	switch options.Mode {
	case "delimited":
		if options.Pair == "" || options.KV == "" {
			return nil, errors.New("parse-kv delimiters must be non-empty")
		}
	case "free":
	case "regex":
		expression, err := regexp.Compile(options.Regex)
		if err != nil || expression.NumSubexp() != 2 {
			return nil, errors.New("parse-kv regex requires exactly two capture groups")
		}
		parser.regex = expression
	default:
		return nil, errors.New("unknown parse-kv mode")
	}
	return parser, nil
}

func (p *keyValueParser) parse(text string) map[string]string {
	result := map[string]string{}
	record := func(key, value string) bool {
		key = strings.TrimSpace(key)
		if !p.wanted[key] {
			return false
		}
		if _, seen := result[key]; !seen {
			result[key] = strings.TrimSpace(value)
		}
		return true
	}
	switch p.options.Mode {
	case "regex":
		for _, match := range p.regex.FindAllStringSubmatch(text, -1) {
			record(match[1], match[2])
			if len(result) == len(p.wanted) {
				break
			}
		}
	case "free":
		p.parseFree(text, record)
	default:
		p.parseDelimited(text, record)
	}
	return result
}

func (p *keyValueParser) quoteAt(text string, i int) (quotePair, bool) {
	r, _ := utf8.DecodeRuneInString(text[i:])
	for _, quote := range p.quotes {
		if quote.open == r {
			return quote, true
		}
	}
	return quotePair{}, false
}

// readQuoted reads a quoted token starting at its opening quote. An escape
// rune makes the following rune literal. An unterminated quote runs to the end.
func (p *keyValueParser) readQuoted(text string, i int, quote quotePair) (string, int) {
	var b strings.Builder
	i += utf8.RuneLen(quote.open)
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if p.escapes[r] && i+size < len(text) {
			next, nextSize := utf8.DecodeRuneInString(text[i+size:])
			b.WriteRune(next)
			i += size + nextSize
			continue
		}
		if r == quote.close {
			return b.String(), i + size
		}
		b.WriteRune(r)
		i += size
	}
	return b.String(), i
}

func skipSpace(text string, i int) int {
	for i < len(text) {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsSpace(r) {
			break
		}
		i += size
	}
	return i
}

// parseDelimited scans pairs separated by Pair, each split at the first KV
// outside quotes. Pairs without KV are ignored. Greedy values continue until
// a pair delimiter that is followed by another key and KV delimiter.
func (p *keyValueParser) parseDelimited(text string, record func(string, string) bool) {
	pair, kv := p.options.Pair, p.options.KV
	i := 0
	for i < len(text) {
		start := i
		i = skipSpace(text, i)
		if i >= len(text) {
			break
		}
		var key string
		if quote, ok := p.quoteAt(text, i); ok {
			key, i = p.readQuoted(text, i, quote)
			i = skipSpace(text, i)
			if !strings.HasPrefix(text[i:], kv) {
				i = nextDelimiter(text, i, pair)
				continue
			}
		} else {
			end := i
			for end < len(text) && !strings.HasPrefix(text[end:], kv) && !strings.HasPrefix(text[end:], pair) {
				end++
			}
			if end >= len(text) || !strings.HasPrefix(text[end:], kv) {
				i = end + len(pair)
				if i <= start {
					i = start + 1
				}
				continue
			}
			key, i = text[i:end], end
		}
		i += len(kv)
		i = skipSpace(text, i)
		var value string
		if quote, ok := p.quoteAt(text, i); i < len(text) && ok {
			value, i = p.readQuoted(text, i, quote)
			i = nextDelimiter(text, i, pair)
		} else {
			end := i
			for end < len(text) {
				if strings.HasPrefix(text[end:], pair) && (!p.options.Greedy || p.keyFollows(text, end+len(pair))) {
					break
				}
				end++
			}
			value = text[i:end]
			i = end + len(pair)
		}
		record(key, value)
		if i <= start {
			i = start + 1
		}
	}
}

func nextDelimiter(text string, i int, pair string) int {
	if index := strings.Index(text[i:], pair); index >= 0 {
		return i + index + len(pair)
	}
	return len(text)
}

// keyFollows reports whether a key and KV delimiter start at i.
func (p *keyValueParser) keyFollows(text string, i int) bool {
	pair, kv := p.options.Pair, p.options.KV
	i = skipSpace(text, i)
	if quote, ok := p.quoteAt(text, i); i < len(text) && ok {
		_, i = p.readQuoted(text, i, quote)
		return strings.HasPrefix(text[skipSpace(text, i):], kv)
	}
	start := i
	for i < len(text) && !strings.HasPrefix(text[i:], kv) && !strings.HasPrefix(text[i:], pair) {
		i++
	}
	return i > start && strings.HasPrefix(text[i:], kv)
}

// parseFree treats every non-alphanumeric rune as a delimiter. Tokens are
// alphanumeric runs or quoted strings; a requested key token takes the next
// token as its value, and unrequested tokens are skipped one at a time.
func (p *keyValueParser) parseFree(text string, record func(string, string) bool) {
	type token struct {
		text   string
		quoted bool
	}
	var tokens []token
	for i := 0; i < len(text); {
		if quote, ok := p.quoteAt(text, i); ok {
			value, next := p.readQuoted(text, i, quote)
			tokens = append(tokens, token{value, true})
			i = next
			continue
		}
		r, size := utf8.DecodeRuneInString(text[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			i += size
			continue
		}
		start := i
		for i < len(text) {
			r, size := utf8.DecodeRuneInString(text[i:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			if _, quote := p.quoteAt(text, i); quote {
				break
			}
			i += size
		}
		tokens = append(tokens, token{text[start:i], false})
	}
	for i := 0; i+1 < len(tokens); i++ {
		if record(tokens[i].text, tokens[i+1].text) {
			i++
		}
	}
}
