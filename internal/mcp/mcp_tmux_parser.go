package mcp

import (
	"encoding/base64"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/shlex"
)

type TmuxVerb struct {
	Name   string
	Args   []string
	Kwargs map[string]string
	Pos    int
	Wire   string
}

var allowedVerbs = map[string]bool{
	"spawn": true, "expect": true, "expect?": true, "send": true,
	"key": true, "snap": true, "kill": true, "sleep": true,
}

var allowedKeyNames map[string]bool

func init() {
	allowedKeyNames = make(map[string]bool)
	fixed := []string{
		"Enter", "C-m", "C-j", "Tab", "Escape", "Space",
		"BSpace", "BTab", "Up", "Down", "Left", "Right",
		"Home", "End", "PPage", "NPage", "PageUp", "PageDown", "Insert", "Delete", "IC", "DC",
	}
	for _, k := range fixed {
		allowedKeyNames[k] = true
	}
	for i := 1; i <= 12; i++ {
		allowedKeyNames[fmt.Sprintf("F%d", i)] = true
	}
	for c := 'a'; c <= 'z'; c++ {
		allowedKeyNames[fmt.Sprintf("C-%c", c)] = true
		allowedKeyNames[fmt.Sprintf("M-%c", c)] = true
	}
}

func ParseVerbScript(body string) ([]TmuxVerb, error) {
	var verbs []TmuxVerb
	verbIndex := 0
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Extract verb name from first whitespace-delimited token.
		verbEnd := strings.IndexByte(trimmed, ' ')
		var name, rawRest string
		if verbEnd < 0 {
			name = trimmed
		} else {
			name = trimmed[:verbEnd]
			rawRest = trimmed[verbEnd+1:]
		}
		if !allowedVerbs[name] {
			return nil, fmt.Errorf("unknown verb %q at line %d", name, i+1)
		}

		// For send: preserve raw text so escape sequences survive.
		// Other verbs go through shlex.
		var args []string
		kwargs := make(map[string]string)
		if name == "send" {
			if rawRest == "" {
				return nil, fmt.Errorf("send requires body text at line %d", i+1)
			}
			args = []string{stripOuterQuotes(rawRest)}
		} else if rawRest != "" {
			tokens, err := shlex.Split(rawRest)
			if err != nil {
				return nil, fmt.Errorf("shlex parse error at line %d: %w", i+1, err)
			}
			for j := len(tokens) - 1; j >= 0; j-- {
				if strings.Contains(tokens[j], "=") && !strings.HasPrefix(tokens[j], "=") {
					parts := strings.SplitN(tokens[j], "=", 2)
					kwargs[parts[0]] = parts[1]
				} else {
					args = tokens[:j+1]
					break
				}
			}
		}

		verb := TmuxVerb{
			Name:   name,
			Args:   args,
			Kwargs: kwargs,
			Pos:    verbIndex,
		}
		if err := buildWire(&verb); err != nil {
			return nil, fmt.Errorf("%s at line %d", err, i+1)
		}
		verbs = append(verbs, verb)
		verbIndex++
	}
	return verbs, nil
}

func stripOuterQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

func buildWire(v *TmuxVerb) error {
	switch v.Name {
	case "spawn":
		return wireSpawn(v)
	case "send":
		return wireSend(v)
	case "key":
		return wireKey(v)
	case "expect":
		return wireExpect(v, false)
	case "expect?":
		return wireExpect(v, true)
	case "snap":
		return wireSnap(v)
	case "kill":
		return wireKill(v)
	case "sleep":
		return wireSleep(v)
	}
	return fmt.Errorf("unhandled verb %q", v.Name)
}

func wireSpawn(v *TmuxVerb) error {
	if len(v.Args) == 0 {
		return fmt.Errorf("spawn requires command")
	}
	payload := strings.Join(v.Args, "\x00")
	v.Wire = "spawn_b64 " + base64.StdEncoding.EncodeToString([]byte(payload))
	return nil
}

func wireSend(v *TmuxVerb) error {
	if len(v.Args) == 0 {
		return fmt.Errorf("send requires body text")
	}
	raw := strings.Join(v.Args, " ")
	decoded := decodeSendEscapes(raw)
	v.Wire = "send_b64 " + base64.StdEncoding.EncodeToString(decoded)
	return nil
}

func decodeSendEscapes(s string) []byte {
	var buf []byte
	i := 0
	for i < len(s) {
		if s[i] != '\\' {
			r, size := utf8.DecodeRuneInString(s[i:])
			b := make([]byte, utf8.RuneLen(r))
			utf8.EncodeRune(b, r)
			buf = append(buf, b...)
			i += size
			continue
		}
		if i+1 >= len(s) {
			buf = append(buf, '\\')
			i++
			continue
		}
		next := s[i+1]
		switch next {
		case '\\':
			buf = append(buf, '\\')
			i += 2
		case 'r':
			buf = append(buf, '\r')
			i += 2
		case 'n':
			buf = append(buf, '\n')
			i += 2
		case 't':
			buf = append(buf, '\t')
			i += 2
		case 'x':
			if i+3 < len(s) {
				val, err := strconv.ParseUint(s[i+2:i+4], 16, 8)
				if err == nil {
					buf = append(buf, byte(val))
					i += 4
					continue
				}
			}
			buf = append(buf, '\\', 'x')
			i += 2
		case 'u':
			consumed, bytes := parseUnicodeEscape(s[i:])
			if consumed > 0 {
				buf = append(buf, bytes...)
				i += consumed
			} else {
				buf = append(buf, '\\', 'u')
				i += 2
			}
		default:
			buf = append(buf, '\\', next)
			i += 2
		}
	}
	return buf
}

func parseUnicodeEscape(s string) (int, []byte) {
	if len(s) < 6 || s[0] != '\\' || s[1] != 'u' {
		return 0, nil
	}
	hexStr := s[2:6]
	val, err := strconv.ParseUint(hexStr, 16, 32)
	if err != nil {
		return 0, nil
	}
	r := rune(val)
	consumed := 6
	b := make([]byte, utf8.RuneLen(r))
	utf8.EncodeRune(b, r)
	return consumed, b
}

func wireKey(v *TmuxVerb) error {
	if len(v.Args) == 0 {
		return fmt.Errorf("key requires at least one key name")
	}
	var names []string
	for _, arg := range v.Args {
		parts := strings.Split(arg, ",")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			if !allowedKeyNames[p] {
				return fmt.Errorf("invalid key name %q", p)
			}
			names = append(names, p)
		}
	}
	v.Wire = "key " + strings.Join(names, " ")
	return nil
}

func wireExpect(v *TmuxVerb, soft bool) error {
	if len(v.Args) == 0 {
		return fmt.Errorf("expect requires pattern")
	}
	pattern := v.Args[0]
	if pattern == "" {
		return fmt.Errorf("expect pattern cannot be empty")
	}
	if strings.Contains(pattern, "\n") {
		return fmt.Errorf("expect pattern cannot contain newline; v1 matches per line only")
	}
	if len(pattern) > 200 {
		return fmt.Errorf("expect pattern exceeds 200 chars")
	}
	_, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("expect pattern invalid: %w", err)
	}
	timeout := 30
	if ts, ok := v.Kwargs["timeout"]; ok {
		parsed, err := parseDuration(ts)
		if err != nil {
			return fmt.Errorf("expect timeout invalid: %w", err)
		}
		timeout = parsed
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(pattern))
	verb := "expect"
	if soft {
		verb = "expect_q"
	}
	v.Wire = fmt.Sprintf("%s %s %d", verb, b64, timeout)
	return nil
}

func parseDuration(s string) (int, error) {
	if len(s) < 2 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	suffix := s[len(s)-1]
	numStr := s[:len(s)-1]
	val, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	switch suffix {
	case 's':
		return int(math.Ceil(val)), nil
	case 'm':
		return int(val * 60), nil
	case 'h':
		return int(val * 3600), nil
	default:
		return 0, fmt.Errorf("invalid duration suffix %q", string(suffix))
	}
}

func wireSnap(v *TmuxVerb) error {
	lines := 200
	if ls, ok := v.Kwargs["lines"]; ok {
		n, err := strconv.Atoi(ls)
		if err != nil {
			return fmt.Errorf("snap lines invalid: %w", err)
		}
		if n < 1 || n > 10000 {
			return fmt.Errorf("snap lines must be 1-10000, got %d", n)
		}
		lines = n
	}
	v.Wire = fmt.Sprintf("snap lines=%d", lines)
	return nil
}

func wireKill(v *TmuxVerb) error {
	v.Wire = "kill"
	return nil
}

func wireSleep(v *TmuxVerb) error {
	if len(v.Args) != 1 {
		return fmt.Errorf("sleep requires exactly 1 argument")
	}
	val, err := strconv.ParseFloat(v.Args[0], 64)
	if err != nil {
		return fmt.Errorf("sleep argument invalid: %w", err)
	}
	if math.IsInf(val, 0) || math.IsNaN(val) || val < 0 || val > 3600 {
		return fmt.Errorf("sleep value must be 0-3600s, got %s", v.Args[0])
	}
	v.Wire = "sleep " + v.Args[0]
	return nil
}
