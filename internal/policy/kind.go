// Package policy decides whether a SQL statement may run.
//
// The design rule throughout is that ambiguity is a refusal. This package
// never tries to fully parse SQL: engine-accurate parsers are engine-specific
// and the good ones need cgo, which would cost the zero-setup install. A
// conservative classifier that refuses what it cannot prove is a read has a
// smaller failure surface than a parser that is wrong in ways nobody has
// enumerated.
package policy

// Kind is what a statement does to the database.
type Kind int

// The zero value is KindUnknown on purpose: a Kind nobody set is the one that
// gets denied, so a missed assignment fails closed.
const (
	KindUnknown Kind = iota
	KindRead
	KindWrite
	KindDDL
)

func (k Kind) String() string {
	switch k {
	case KindRead:
		return "read"
	case KindWrite:
		return "write"
	case KindDDL:
		return "ddl"
	default:
		return "unknown"
	}
}
