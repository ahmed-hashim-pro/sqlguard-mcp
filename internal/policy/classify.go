package policy

import (
	"errors"
	"fmt"
	"strings"
)

// Refusals that callers may want to distinguish. Sentinel errors are compared
// with errors.Is, so wrapping one with %w keeps it matchable while adding
// context.
var (
	ErrEmpty              = errors.New("statement is empty")
	ErrComment            = errors.New("statement contains a comment")
	ErrMultipleStatements = errors.New("statement contains more than one statement")
	ErrUnterminatedQuote  = errors.New("statement has an unterminated quoted section")
)

// Anything that changes rows.
var writeKeywords = map[string]bool{
	"INSERT": true, "UPDATE": true, "DELETE": true, "REPLACE": true,
	"MERGE": true, "UPSERT": true,
}

// Anything that changes the schema, the files behind it, or who may read it.
// PRAGMA, ATTACH and VACUUM are here rather than under "read" because their
// read and write forms differ only by an argument.
var ddlKeywords = map[string]bool{
	"CREATE": true, "DROP": true, "ALTER": true, "TRUNCATE": true,
	"RENAME": true, "GRANT": true, "REVOKE": true, "PRAGMA": true,
	"ATTACH": true, "DETACH": true, "VACUUM": true, "REINDEX": true,
}

// Statements that may begin a read. A statement must start with one of these
// AND contain no write or DDL keyword to be classified as a read.
var readStarters = map[string]bool{
	"SELECT": true, "WITH": true, "VALUES": true, "EXPLAIN": true,
	"SHOW": true, "DESCRIBE": true, "DESC": true, "TABLE": true,
}

// Classify reports what a statement does.
//
// It is deliberately conservative. A statement is a read only when it starts
// like one and contains no keyword that could change anything, so a
// data-modifying CTE — which starts with WITH and is a write — is caught by the
// keyword scan rather than by the first word.
//
// The error return is for statements that cannot be classified at all, which is
// separate from a statement classified as something the caller will refuse.
func Classify(sql string) (Kind, error) {
	words, err := scan(sql)
	if err != nil {
		return KindUnknown, err
	}
	if len(words) == 0 {
		return KindUnknown, ErrEmpty
	}

	for _, w := range words {
		if writeKeywords[w] {
			return KindWrite, nil
		}
	}
	for _, w := range words {
		if ddlKeywords[w] {
			return KindDDL, nil
		}
	}
	if readStarters[words[0]] {
		return KindRead, nil
	}
	return KindUnknown, nil
}

// scan walks the statement once and returns its bare words, uppercased, with
// anything inside quotes left out — so a row containing the text 'DELETE' is
// data and not a verb.
//
// It refuses rather than interprets: a comment could hide a second statement
// from a reader, and a second statement could hide anything at all.
func scan(sql string) ([]string, error) {
	var (
		words   []string
		current strings.Builder
		sawEnd  bool // a semicolon has closed the first statement
		runes   = []rune(sql)
	)

	flushWord := func() {
		if current.Len() > 0 {
			words = append(words, strings.ToUpper(current.String()))
			current.Reset()
		}
	}

	for i := 0; i < len(runes); i++ {
		c := runes[i]

		// Anything after the end of the first statement must be blank.
		if sawEnd {
			if !isSpace(c) {
				return nil, fmt.Errorf("%w: found %q after the first statement ended",
					ErrMultipleStatements, string(c))
			}
			continue
		}

		switch {
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			return nil, ErrComment

		case c == '/' && i+1 < len(runes) && runes[i+1] == '*':
			return nil, ErrComment

		case c == '\'' || c == '"' || c == '`' || c == '[':
			flushWord()
			end, err := skipQuoted(runes, i)
			if err != nil {
				return nil, err
			}
			i = end

		case c == ';':
			flushWord()
			sawEnd = true

		case isWordRune(c):
			current.WriteRune(c)

		default:
			flushWord()
		}
	}
	flushWord()
	return words, nil
}

// skipQuoted returns the index of the closing delimiter for the quoted section
// that starts at open. A doubled delimiter inside a section is an escaped
// literal, not the end of it: 'it”s' is one string.
func skipQuoted(runes []rune, open int) (int, error) {
	closer := runes[open]
	if closer == '[' {
		closer = ']'
	}
	doubling := runes[open] != '['

	for i := open + 1; i < len(runes); i++ {
		if runes[i] != closer {
			continue
		}
		if doubling && i+1 < len(runes) && runes[i+1] == closer {
			i++ // an escaped delimiter, keep going
			continue
		}
		return i, nil
	}
	return 0, fmt.Errorf("%w: no closing %q", ErrUnterminatedQuote, string(closer))
}

func isSpace(c rune) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func isWordRune(c rune) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}
