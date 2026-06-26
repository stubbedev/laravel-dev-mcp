package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/VKCOM/php-parser/pkg/ast"
	"github.com/VKCOM/php-parser/pkg/conf"
	"github.com/VKCOM/php-parser/pkg/parser"
	phpver "github.com/VKCOM/php-parser/pkg/version"
)

// configKeyRe restricts config keys to safe characters before embedding them in
// a php expression for the fallback resolver.
var configKeyRe = regexp.MustCompile(`^[A-Za-z0-9_.*-]+$`)

// phpconfig.go reads Laravel `config/*.php` files WITHOUT executing PHP: it
// parses each file to an AST and evaluates the returned array, resolving
// `env()` calls against the project's .env and the common path helpers. This
// covers the cases Laravel config files actually use (literals, nested arrays,
// env(), string concat, ternaries, path helpers); anything else evaluates to a
// descriptive placeholder rather than failing.

// envLookup returns a raw .env value and whether the key is present (even if
// empty). Only the project's own .env is consulted — never the server process
// environment — so a config's env() calls can't leak another root's (or the
// server's) variables.
func (p *Project) envLookup(key string) (string, bool) {
	v, ok := p.envMap()[key]
	return v, ok
}

// parseFileAST parses any PHP file to its AST, cached by mtime/size (the lexing
// is the expensive part).
func (p *Project) parseFileAST(path string) (*ast.Root, error) {
	v, err := loadCached(path, func(src []byte) (any, error) {
		root, perr := parser.Parse(src, conf.Config{Version: &phpver.Version{Major: 8, Minor: 1}})
		if perr != nil {
			return nil, fmt.Errorf("parse %s: %w", filepath.Base(path), perr)
		}
		r, ok := root.(*ast.Root)
		if !ok {
			return nil, fmt.Errorf("unexpected AST root for %s", filepath.Base(path))
		}
		return r, nil
	})
	if err != nil {
		return nil, err
	}
	r, ok := v.(*ast.Root)
	if !ok {
		return nil, fmt.Errorf("%s AST cache type mismatch", filepath.Base(path))
	}
	return r, nil
}

// parseConfigAST parses config/<name>.php. Evaluation happens per-call against
// the current .env so edits to .env are reflected without a config-file change.
func (p *Project) parseConfigAST(name string) (*ast.Root, error) {
	return p.parseFileAST(p.path("config", name+".php"))
}

// readConfigFile evaluates config/<name>.php's returned value. After it returns,
// p.evalLossy reports whether any dynamic construct was skipped.
func (p *Project) readConfigFile(name string) (any, error) {
	r, err := p.parseConfigAST(name)
	if err != nil {
		return nil, err
	}
	p.evalLossy = false
	for _, stmt := range r.Stmts {
		if ret, ok := stmt.(*ast.StmtReturn); ok {
			return p.evalPHP(ret.Expr), nil
		}
	}
	return nil, fmt.Errorf("config/%s.php has no return statement", name)
}

// configViaPHP resolves a config key by actually booting the app via artisan
// and returning the merged value as JSON. Used when static eval is incomplete
// (array spreads, dynamic constructs). Boots Laravel, so it is the slow path.
func (p *Project) configViaPHP(ctx context.Context, key string) (any, bool, error) {
	if key == "" || !configKeyRe.MatchString(key) {
		return nil, false, fmt.Errorf("invalid config key %q", key)
	}
	out, err := p.runArtisan(ctx, "tinker", "--execute=echo json_encode(config('"+key+"'));")
	if err != nil {
		return nil, false, err
	}
	out = strings.TrimSpace(out)
	if out == "" || out == "null" {
		return nil, false, nil
	}
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		return nil, false, fmt.Errorf("php config(%q) returned non-JSON: %s", key, truncate(out, 200))
	}
	return v, true, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// config resolves a dotted config key (file.path.into.array). The first segment
// is the config file; the rest navigate the returned structure.
func (p *Project) config(key string) (any, bool, error) {
	segs := strings.Split(key, ".")
	val, err := p.readConfigFile(segs[0])
	if err != nil {
		return nil, false, err
	}
	for _, seg := range segs[1:] {
		m, ok := val.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		val, ok = m[seg]
		if !ok {
			return nil, false, nil
		}
	}
	return val, true, nil
}

// configFiles lists available config file names (without .php).
func (p *Project) configFiles() ([]string, error) {
	entries, err := os.ReadDir(p.path("config"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".php") {
			out = append(out, strings.TrimSuffix(e.Name(), ".php"))
		}
	}
	return out, nil
}

// evalScalar evaluates literal scalar nodes, returning ok=false for non-scalars.
func evalScalar(node ast.Vertex) (any, bool) {
	switch n := node.(type) {
	case *ast.ScalarString:
		return unquotePHP(string(n.Value)), true
	case *ast.ScalarLnumber:
		i, _ := strconv.ParseInt(string(n.Value), 0, 64)
		return i, true
	case *ast.ScalarDnumber:
		f, _ := strconv.ParseFloat(string(n.Value), 64)
		return f, true
	case *ast.ScalarEncapsed:
		var b strings.Builder
		for _, part := range n.Parts {
			if sp, ok := part.(*ast.ScalarEncapsedStringPart); ok {
				b.Write(sp.Value)
			}
		}
		return b.String(), true
	}
	return nil, false
}

// evalPHP evaluates a config AST node to a Go value.
func (p *Project) evalPHP(node ast.Vertex) any {
	if v, ok := evalScalar(node); ok {
		return v
	}
	switch n := node.(type) {
	case *ast.ExprArray:
		return p.evalArray(n)
	case *ast.ExprFunctionCall:
		return p.evalCall(nameString(n.Function), n.Args)
	default:
		return p.evalOperator(node)
	}
}

// evalOperator handles constant/operator/expression nodes.
func (p *Project) evalOperator(node ast.Vertex) any {
	switch n := node.(type) {
	case *ast.ExprConstFetch:
		switch strings.ToLower(nameString(n.Const)) {
		case "true":
			return true
		case "false":
			return false
		case "null":
			return nil
		default:
			return nameString(n.Const)
		}
	case *ast.ExprClassConstFetch:
		// Foo::class evaluates to the class name as written; other class
		// constants we can't resolve statically, so report them descriptively.
		if identString(n.Const) == "class" {
			return nameString(n.Class)
		}
		return nameString(n.Class) + "::" + identString(n.Const)
	case *ast.ExprBinaryCoalesce:
		if l := p.evalPHP(n.Left); l != nil {
			return l
		}
		return p.evalPHP(n.Right)
	case *ast.ExprBrackets:
		return p.evalPHP(n.Expr)
	case *ast.ExprUnaryMinus:
		switch v := p.evalPHP(n.Expr).(type) {
		case int64:
			return -v
		case float64:
			return -v
		}
		return nil
	case *ast.ExprBinaryConcat:
		return fmt.Sprint(p.evalPHP(n.Left)) + fmt.Sprint(p.evalPHP(n.Right))
	case *ast.ExprTernary:
		if truthy(fmt.Sprint(p.evalPHP(n.Cond))) {
			if n.IfTrue != nil {
				return p.evalPHP(n.IfTrue)
			}
			return p.evalPHP(n.Cond)
		}
		return p.evalPHP(n.IfFalse)
	default:
		return nil
	}
}

func (p *Project) evalArray(n *ast.ExprArray) any {
	hasKey := false
	for _, item := range n.Items {
		if it, ok := item.(*ast.ExprArrayItem); ok && it.Key != nil && it.EllipsisTkn == nil {
			hasKey = true
			break
		}
	}
	if hasKey {
		out := map[string]any{}
		idx := 0
		for _, item := range n.Items {
			it, ok := item.(*ast.ExprArrayItem)
			// Skip spreads (`...$x`): they unpack a runtime value we can't
			// resolve statically. Mark the eval lossy so callers fall back to PHP.
			if ok && it.EllipsisTkn != nil {
				p.evalLossy = true
				continue
			}
			if !ok || it.Val == nil {
				continue
			}
			var key string
			if it.Key != nil {
				key = fmt.Sprint(p.evalPHP(it.Key))
			} else {
				// Mixed array: unkeyed items take the next integer index.
				key = strconv.Itoa(idx)
				idx++
			}
			out[key] = p.evalPHP(it.Val)
		}
		return out
	}
	out := []any{}
	for _, item := range n.Items {
		if it, ok := item.(*ast.ExprArrayItem); ok && it.Val != nil && it.EllipsisTkn == nil {
			out = append(out, p.evalPHP(it.Val))
		}
	}
	return out
}

// evalCall handles the functions Laravel config files use.
func (p *Project) evalCall(name string, args []ast.Vertex) any {
	vals := make([]any, 0, len(args))
	for _, a := range args {
		if arg, ok := a.(*ast.Argument); ok {
			vals = append(vals, p.evalPHP(arg.Expr))
		}
	}
	switch name {
	case "env":
		if len(vals) == 0 {
			return nil
		}
		key := fmt.Sprint(vals[0])
		if raw, ok := p.envLookup(key); ok {
			return castEnv(raw)
		}
		if len(vals) > 1 {
			return vals[1]
		}
		return nil
	case "storage_path", "base_path", "app_path", "public_path", "resource_path", "database_path", "config_path":
		sub := strings.TrimSuffix(name, "_path")
		dir := map[string]string{
			"storage": "storage", "base": "", "app": "app",
			"public": "public", "resource": "resources",
			"database": "database", "config": "config",
		}[sub]
		parts := []string{p.Root}
		if dir != "" {
			parts = append(parts, dir)
		}
		if len(vals) > 0 {
			parts = append(parts, fmt.Sprint(vals[0]))
		}
		return filepath.Join(parts...)
	default:
		// A function call we can't evaluate statically — the result is
		// incomplete, so mark lossy to trigger the PHP fallback.
		p.evalLossy = true
		return fmt.Sprintf("<%s(...)>", name)
	}
}

// castEnv mirrors Laravel's env() coercion of common string values.
func castEnv(s string) any {
	switch strings.ToLower(s) {
	case "true", "(true)":
		return true
	case "false", "(false)":
		return false
	case "null", "(null)":
		return nil
	case "empty", "(empty)":
		return ""
	}
	// Strip surrounding quotes Laravel allows in .env values.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// identString returns the value of an *ast.Identifier node.
func identString(node ast.Vertex) string {
	if id, ok := node.(*ast.Identifier); ok {
		return string(id.Value)
	}
	return ""
}

// nameString extracts a dotted/plain name from a Name/NamePart node.
func nameString(node ast.Vertex) string {
	switch n := node.(type) {
	case *ast.Name:
		var parts []string
		for _, p := range n.Parts {
			if np, ok := p.(*ast.NamePart); ok {
				parts = append(parts, string(np.Value))
			}
		}
		return strings.Join(parts, "\\")
	case *ast.NamePart:
		return string(n.Value)
	}
	return ""
}

// unquotePHP strips quotes from a PHP string literal token and unescapes the
// minimal set of escapes that appear in config files.
func unquotePHP(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q != '\'' && q != '"') || s[len(s)-1] != q {
		return s
	}
	inner := s[1 : len(s)-1]
	if q == '\'' {
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)
		return inner
	}
	r := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t", `\r`, "\r")
	return r.Replace(inner)
}
