// Package dotenv loads a .env file the way the Rust binaries do with
// dotenvy::dotenv(): the nearest .env in the working directory or an
// ancestor, without overriding variables already set.
package dotenv

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Load finds and applies .env. Parsing stops at the first malformed line;
// assignments before it stay applied.
func Load() error {
	path, err := find()
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return apply(lines)
}

func find() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, ".env")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

var errParse = errors.New("dotenv: parse error")

func apply(lines []string) error {
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || !validKey(key) {
			return errParse
		}
		rest = strings.TrimLeft(rest, " \t")
		var value string
		switch {
		case strings.HasPrefix(rest, "'"):
			v, consumed, ok := quoted(lines, i, rest[1:], '\'')
			if !ok {
				return errParse
			}
			value, i = v, i+consumed
		case strings.HasPrefix(rest, `"`):
			v, consumed, ok := quoted(lines, i, rest[1:], '"')
			if !ok {
				return errParse
			}
			value, i = expand(unescape(v)), i+consumed
		default:
			if j := strings.Index(rest, " #"); j >= 0 {
				rest = rest[:j]
			}
			value = expand(strings.TrimSpace(rest))
		}
		if _, set := os.LookupEnv(key); !set {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}

func validKey(k string) bool {
	for _, c := range k {
		if !(c == '_' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// quoted reads a value up to the closing quote, continuing across lines.
func quoted(lines []string, i int, first string, q byte) (string, int, bool) {
	var sb strings.Builder
	s := first
	for consumed := 0; ; consumed++ {
		for j := 0; j < len(s); j++ {
			if q == '"' && s[j] == '\\' && j+1 < len(s) {
				sb.WriteByte(s[j])
				sb.WriteByte(s[j+1])
				j++
				continue
			}
			if s[j] == q {
				return sb.String(), consumed, true
			}
			sb.WriteByte(s[j])
		}
		if i+consumed+1 >= len(lines) {
			return "", 0, false
		}
		sb.WriteByte('\n')
		s = lines[i+consumed+1]
	}
}

func unescape(s string) string {
	r := strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`, `\$`, "\x00")
	return r.Replace(s)
}

// expand substitutes $VAR and ${VAR}; an escaped \$ (marked as NUL by
// unescape) stays literal.
func expand(s string) string {
	out := os.Expand(s, os.Getenv)
	return strings.ReplaceAll(out, "\x00", "$")
}
