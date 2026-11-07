package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// env reads environment variables and accumulates parse errors.
type env struct {
	getenv func(string) string
	errs   []error
}

func (e *env) err() error { return errors.Join(e.errs...) }

// lookup returns the trimmed value of name, and whether it is set and non-empty.
func (e *env) lookup(name string) (string, bool) {
	v := strings.TrimSpace(e.getenv(name))
	return v, v != ""
}

func (e *env) str(name, def string) string {
	if v, ok := e.lookup(name); ok {
		return v
	}
	return def
}

func (e *env) boolean(name string, def bool) bool {
	v, ok := e.lookup(name)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %q is not a boolean", name, v))
		return def
	}
	return b
}

func (e *env) integer(name string, def, maxVal int64) int64 {
	v, ok := e.lookup(name)
	if !ok {
		return def
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i > maxVal {
		e.errs = append(e.errs, fmt.Errorf("%s: %q is not an integer <= %d", name, v, maxVal))
		return def
	}
	return i
}

// dur reads a duration given as plain seconds ("300") or a Go duration ("5m").
func (e *env) dur(name string, def time.Duration) time.Duration {
	v, ok := e.lookup(name)
	if !ok {
		return def
	}
	d, err := ParseDuration(v)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s: %w", name, err))
		return def
	}
	return d
}

// ParseDuration accepts plain (possibly fractional) seconds or a Go duration string.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return time.Duration(f * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is neither seconds nor a Go duration", s)
	}
	return d, nil
}
