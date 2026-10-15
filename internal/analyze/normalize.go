package analyze

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/ahmed-hashim-pro/trace-diff/internal/otlp"
)

// Op identifies "the same operation" across runs: the service doing it, the
// span kind, and a description with the values taken out. Two spans with
// the same Op are the same call made with different arguments.
type Op struct {
	Service string
	Kind    string // db, client, server, producer, consumer, internal
	Desc    string
}

func (o Op) String() string {
	return o.Service + " · " + o.Kind + " · " + o.Desc
}

func (o Op) key() string { return o.Service + "\x00" + o.Kind + "\x00" + o.Desc }

var (
	sqlString  = regexp.MustCompile(`'(?:[^']|'')*'`)
	sqlNumber  = regexp.MustCompile(`(?i)\b(?:0x[0-9a-f]+|\d+(?:\.\d+)?(?:e[+-]?\d+)?)\b`)
	sqlParam   = regexp.MustCompile(`\$\d+|:[A-Za-z_]\w*|@p\d+|@[A-Za-z_]\w*`)
	sqlList    = regexp.MustCompile(`\(\s*\?(?:\s*,\s*\?)*\s*\)`)
	sqlRows    = regexp.MustCompile(`\(\?\)(?:\s*,\s*\(\?\))+`)
	whitespace = regexp.MustCompile(`\s+`)
	digits     = regexp.MustCompile(`\d+`)
	uuidLike   = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hexLike    = regexp.MustCompile(`(?i)^[0-9a-f]{16,}$`)
	idLike     = regexp.MustCompile(`^[A-Za-z0-9_-]*\d[A-Za-z0-9_-]*$`)
)

// NormalizeSQL takes the values out of a statement: string and number
// literals and bind parameters ($1, :name, @p1) become ?, and a list of
// them, as in IN (1, 2, 3) or multi-row VALUES, becomes one. Whitespace is
// collapsed and a trailing semicolon dropped. Identifiers and keywords keep
// their case.
func NormalizeSQL(q string) string {
	q = sqlString.ReplaceAllString(q, "?")
	q = sqlParam.ReplaceAllString(q, "?")
	q = sqlNumber.ReplaceAllString(q, "?")
	q = whitespace.ReplaceAllString(q, " ")
	q = sqlList.ReplaceAllString(q, "(?)")
	q = sqlRows.ReplaceAllString(q, "(?)")
	q = strings.TrimSpace(q)
	q = strings.TrimSuffix(q, ";")
	return strings.TrimSpace(q)
}

// NormalizePath replaces the variable segments of a URL path with
// placeholders: numbers become {n}, UUIDs and long hex strings {id}, and
// other segments with a digit in them {id}. The query string is dropped.
func NormalizePath(p string) string {
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p = p[:i]
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch {
		case s == "":
		case digits.MatchString(s) && digits.FindString(s) == s:
			segs[i] = "{n}"
		case uuidLike.MatchString(s), hexLike.MatchString(s):
			segs[i] = "{id}"
		case idLike.MatchString(s) && len(s) >= 6:
			segs[i] = "{id}"
		}
	}
	return strings.Join(segs, "/")
}

// opOf describes a span by what it does rather than its arguments. Database
// spans use their statement; HTTP servers their route; HTTP clients the
// host and normalized path they call; RPCs their service and method;
// everything else its name with numbers taken out.
func opOf(s *otlp.Span, keepLiterals bool) Op {
	kind := s.Kind.String()
	if kind == "unspecified" {
		kind = "internal"
	}
	a := s.Attrs
	if stmt := first(a, "db.query.text", "db.statement"); stmt != "" {
		if !keepLiterals {
			stmt = NormalizeSQL(stmt)
		}
		return Op{s.Service, "db", clip(stmt, 160)}
	}
	if a["db.system"] != "" || a["db.system.name"] != "" {
		return Op{s.Service, "db", clip(digits.ReplaceAllString(s.Name, "{n}"), 160)}
	}
	method := first(a, "http.request.method", "http.method")
	switch s.Kind {
	case otlp.KindServer:
		if route := a["http.route"]; route != "" {
			return Op{s.Service, kind, strings.TrimSpace(method + " " + route)}
		}
	case otlp.KindClient:
		if u := first(a, "url.full", "http.url"); u != "" {
			if pu, err := url.Parse(u); err == nil && pu.Host != "" {
				return Op{s.Service, kind, strings.TrimSpace(method + " " + pu.Host + " " + normPath(pu.Path, keepLiterals))}
			}
		}
		if host := first(a, "server.address", "net.peer.name"); host != "" {
			if path := first(a, "url.path", "http.target"); path != "" {
				return Op{s.Service, kind, strings.TrimSpace(method + " " + host + " " + normPath(path, keepLiterals))}
			}
		}
	}
	if svc := a["rpc.service"]; svc != "" {
		return Op{s.Service, kind, "rpc " + svc + "/" + a["rpc.method"]}
	}
	if s.Kind == otlp.KindServer || s.Kind == otlp.KindClient {
		if p := first(a, "url.path", "http.target"); p != "" && !keepLiterals {
			return Op{s.Service, kind, strings.TrimSpace(method + " " + NormalizePath(p))}
		}
	}
	name := s.Name
	if !keepLiterals {
		name = NormalizePath(name)
		name = digits.ReplaceAllString(name, "{n}")
	}
	return Op{s.Service, kind, clip(name, 160)}
}

func normPath(p string, keep bool) string {
	if keep {
		return p
	}
	return NormalizePath(p)
}

func first(a map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := a[k]; v != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// Describe is what a span did, with its values taken out: the statement,
// route or call that identifies it across runs.
func Describe(s *otlp.Span) string { return opOf(s, false).Desc }

// IsDB reports whether a span is a database call.
func IsDB(s *otlp.Span) bool { return opOf(s, false).Kind == "db" }

// OpOf is the operation a span belongs to.
func OpOf(s *otlp.Span) Op { return opOf(s, false) }
