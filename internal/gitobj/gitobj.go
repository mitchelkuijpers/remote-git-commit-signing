// Package gitobj structurally parses Git object payloads. It exists so the
// signer can reason about what it is asked to sign (a commit object, authored
// by an expected identity, not already signed) instead of treating the request
// body as opaque bytes.
//
// Parsing is byte-oriented and deliberately not regex-based: commit objects
// are line-structured with space-prefixed continuation lines for multi-line
// header values, and a single blank line separating the header block from the
// free-form message body. The message is never scanned for headers.
package gitobj

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// ErrMalformed wraps every structural parse failure. Callers classify a
// rejected payload by matching it with errors.Is.
var ErrMalformed = errors.New("malformed commit object")

// Header is one commit-object header. Value is the logical (unfolded) value:
// continuation lines are joined with '\n' after their single leading space is
// stripped, which is how git reconstructs multi-line values such as gpgsig.
type Header struct {
	Key   string
	Value []byte
}

// Commit is a structurally parsed git commit object.
type Commit struct {
	// Headers are the commit headers in payload order.
	Headers []Header
	// Message is the raw, uninterpreted message body. It may contain arbitrary
	// bytes, including invalid UTF-8.
	Message []byte
}

// Header returns the value of the first header with the given key.
func (c *Commit) Header(key string) ([]byte, bool) {
	for _, h := range c.Headers {
		if h.Key == key {
			return h.Value, true
		}
	}
	return nil, false
}

// HasHeader reports whether a header with the given key is present.
func (c *Commit) HasHeader(key string) bool {
	_, ok := c.Header(key)
	return ok
}

// requiredHeaders are the headers every commit object must carry exactly once.
var requiredHeaders = []string{"tree", "author", "committer"}

// ParseCommit parses payload as a git commit object. It validates the object's
// structure — header syntax, continuation folding, the blank separator, and
// the mandatory tree/author/committer headers — and returns a wrapped
// ErrMalformed for anything that is not a commit object.
func ParseCommit(payload []byte) (*Commit, error) {
	sep := bytes.Index(payload, []byte("\n\n"))
	if sep < 0 {
		return nil, fmt.Errorf("%w: no header/message separator", ErrMalformed)
	}

	commit := &Commit{Message: append([]byte(nil), payload[sep+2:]...)}

	if err := parseHeaders(commit, payload[:sep]); err != nil {
		return nil, err
	}

	for _, key := range requiredHeaders {
		if countHeader(commit.Headers, key) != 1 {
			return nil, fmt.Errorf("%w: header %q must appear exactly once", ErrMalformed, key)
		}
	}
	return commit, nil
}

// parseHeaders unfolds the header block (everything before the blank
// separator) into commit.Headers.
func parseHeaders(commit *Commit, block []byte) error {
	for _, line := range bytes.Split(block, []byte("\n")) {
		if len(line) > 0 && line[0] == ' ' {
			// Continuation of the previous header's value.
			if len(commit.Headers) == 0 {
				return fmt.Errorf("%w: continuation line without a preceding header", ErrMalformed)
			}
			h := &commit.Headers[len(commit.Headers)-1]
			h.Value = append(h.Value, '\n')
			h.Value = append(h.Value, line[1:]...)
			continue
		}

		key, value, ok := bytes.Cut(line, []byte(" "))
		if !ok {
			return fmt.Errorf("%w: header line without a value separator", ErrMalformed)
		}
		if !validKey(key) {
			return fmt.Errorf("%w: invalid header key %q", ErrMalformed, key)
		}
		// Copy the value so later continuation lines never write back into the
		// caller's payload buffer.
		commit.Headers = append(commit.Headers, Header{
			Key:   string(key),
			Value: append([]byte(nil), value...),
		})
	}
	return nil
}

// validKey reports whether key is a syntactically valid commit header name
// (letters, digits, and hyphens, as used by tree/parent/gpgsig-sha256/...).
func validKey(key []byte) bool {
	if len(key) == 0 {
		return false
	}
	for _, c := range key {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func countHeader(headers []Header, key string) int {
	n := 0
	for _, h := range headers {
		if h.Key == key {
			n++
		}
	}
	return n
}

// Signature is the identity encoded by an author or committer header value:
//
//	<name> <<email>> <unix-timestamp> <+-HHMM>
type Signature struct {
	Name  string
	Email string
}

// ParseSignature parses an author/committer header value. It returns a wrapped
// ErrMalformed when the value does not have git's
// "Name <email> timestamp timezone" shape.
func ParseSignature(value []byte) (Signature, error) {
	lt := bytes.LastIndexByte(value, '<')
	if lt < 0 {
		return Signature{}, fmt.Errorf("%w: signature has no '<'", ErrMalformed)
	}
	gt := bytes.IndexByte(value[lt:], '>')
	if gt < 0 {
		return Signature{}, fmt.Errorf("%w: signature has no '>' after '<'", ErrMalformed)
	}

	name := strings.TrimSpace(string(value[:lt]))
	email := string(value[lt+1 : lt+gt])
	if email == "" {
		return Signature{}, fmt.Errorf("%w: signature has an empty email", ErrMalformed)
	}

	rest := bytes.Fields(value[lt+gt+1:])
	if len(rest) != 2 {
		return Signature{}, fmt.Errorf("%w: signature needs a timestamp and timezone", ErrMalformed)
	}
	if !allDigits(rest[0]) {
		return Signature{}, fmt.Errorf("%w: signature timestamp %q is not numeric", ErrMalformed, rest[0])
	}
	if !validTimezone(rest[1]) {
		return Signature{}, fmt.Errorf("%w: signature timezone %q is not +-HHMM", ErrMalformed, rest[1])
	}

	return Signature{Name: name, Email: email}, nil
}

func allDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// validTimezone reports whether b is a git timezone offset: sign followed by
// four digits (e.g. "+0000", "-0730").
func validTimezone(b []byte) bool {
	if len(b) != 5 || (b[0] != '+' && b[0] != '-') {
		return false
	}
	return allDigits(b[1:])
}
