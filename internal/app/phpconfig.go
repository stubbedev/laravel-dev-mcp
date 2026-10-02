package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/VKCOM/php-parser/pkg/ast"
	"github.com/VKCOM/php-parser/pkg/conf"
	phperrors "github.com/VKCOM/php-parser/pkg/errors"
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
	val, found := p.envMap()[key]

	return val, found
}

// phpParserVersion is the newest grammar php-parser implements. Syntax added
// after it (DNF types, typed class constants, property hooks, asymmetric
// visibility, the 8.5 pipe operator, …) is recovered from as a syntax error,
// which parsedFile.partial records so callers never present that AST as
// complete.
func phpParserVersion() *phpver.Version {
	return &phpver.Version{Major: phpParserMajor, Minor: phpParserMinor}
}

const (
	phpParserMajor = 8
	phpParserMinor = 1
)

// PHP literal spellings shared by constant fetches, env() casts and the
// artisan fallback's JSON output.
const (
	phpLitTrue  = "true"
	phpLitFalse = "false"
	phpLitNull  = "null"
)

// Laravel app directories the path helpers resolve to.
const (
	laravelAppDir     = "app"
	laravelConfigDir  = "config"
	laravelStorageDir = "storage"
	laravelDBDir      = "database"
)

// Config read failures. Each reads naturally once wrapped with its context.
var (
	errConfigUnexpectedRoot = errors.New("unexpected AST root")
	errConfigNoReturn       = errors.New("has no return statement")
	errConfigInvalidKey     = errors.New("invalid config key")
	errConfigNonJSON        = errors.New("returned non-JSON")
	// errConfigUnparsedReturn means the return statement itself was
	// unparseable; evalLossy is set so the caller asks PHP instead.
	errConfigUnparsedReturn = errors.New("config return statement not parseable")
)

// configPreviewLen bounds how much of a bad artisan reply an error quotes.
const configPreviewLen = 200

// parsedFile is a PHP file's AST. partial reports that the parser hit syntax it
// does not understand and recovered by dropping the surrounding statement, so
// the tree may be missing parts of the file.
type parsedFile struct {
	root    *ast.Root
	partial bool
}

// parseFileAST parses any PHP file to its AST, cached by mtime/size (the lexing
// is the expensive part).
func (*Project) parseFileAST(path string) (parsedFile, error) {
	return loadCached(path, func(src []byte) (parsedFile, error) {
		var syntaxErrs int

		root, perr := parser.Parse(src, conf.Config{
			Version:          phpParserVersion(),
			ErrorHandlerFunc: func(*phperrors.Error) { syntaxErrs++ },
		})
		if perr != nil {
			return parsedFile{}, fmt.Errorf("parse %s: %w", filepath.Base(path), perr)
		}

		fileRoot, isRoot := root.(*ast.Root)
		if !isRoot {
			return parsedFile{}, fmt.Errorf("%w for %s", errConfigUnexpectedRoot, filepath.Base(path))
		}

		return parsedFile{root: fileRoot, partial: syntaxErrs > 0}, nil
	})
}

// parseConfigAST parses config/<name>.php. Evaluation happens per-call against
// the current .env so edits to .env are reflected without a config-file change.
func (p *Project) parseConfigAST(name string) (parsedFile, error) {
	return p.parseFileAST(p.path(laravelConfigDir, name+".php"))
}

// readConfigFile evaluates config/<name>.php's returned value. After it returns,
// p.evalLossy reports whether any dynamic construct or unparseable syntax was
// skipped.
func (p *Project) readConfigFile(name string) (any, error) {
	file, err := p.parseConfigAST(name)
	if err != nil {
		return nil, err
	}
	// Syntax newer than the parser's grammar leaves holes in the tree, so the
	// static result can't be trusted: route it through the PHP fallback.
	p.evalLossy = file.partial
	for _, stmt := range file.root.Stmts {
		if ret, isReturn := stmt.(*ast.StmtReturn); isReturn {
			return p.evalPHP(ret.Expr), nil
		}
	}

	if file.partial {
		// The return statement itself was unparseable: no static value, and
		// evalLossy tells the caller to ask PHP.
		return nil, errConfigUnparsedReturn
	}

	return nil, fmt.Errorf("config/%s.php %w", name, errConfigNoReturn)
}

// configViaPHP resolves a config key by actually booting the app via artisan
// and returning the merged value as JSON. Used when static eval is incomplete
// (array spreads, dynamic constructs). Boots Laravel, so it is the slow path.
func (p *Project) configViaPHP(ctx context.Context, key string) (any, bool, error) {
	if key == "" || !configKeyRe.MatchString(key) {
		return nil, false, fmt.Errorf("%w %q", errConfigInvalidKey, key)
	}

	out, err := p.runArtisan(ctx, "tinker", "--execute=echo json_encode(config('"+key+"'));")
	if err != nil {
		return nil, false, err
	}

	out = strings.TrimSpace(out)
	if out == "" || out == phpLitNull {
		return nil, false, nil
	}

	var val any

	err = json.Unmarshal([]byte(out), &val)
	if err != nil {
		return nil, false, fmt.Errorf("php config(%q) %w: %s", key, errConfigNonJSON, truncate(out, configPreviewLen))
	}

	return val, true, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return truncateUTF8(s, n) + "…"
	}

	return s
}

// config resolves a dotted config key (file.path.into.array). The first segment
// is the config file; the rest navigate the returned structure.
func (p *Project) config(key string) (any, bool, error) {
	segs := strings.Split(key, ".")

	val, err := p.readConfigFile(segs[0])
	if errors.Is(err, errConfigUnparsedReturn) {
		// No static value; evalLossy is already set, so callers ask PHP.
		val, err = nil, nil
	}

	if err != nil {
		return nil, false, err
	}

	for _, seg := range segs[1:] {
		obj, isMap := val.(map[string]any)
		if !isMap {
			return nil, false, nil
		}

		var present bool

		val, present = obj[seg]
		if !present {
			return nil, false, nil
		}
	}

	return val, true, nil
}

// configFiles lists available config file names (without .php).
func (p *Project) configFiles() ([]string, error) {
	entries, err := os.ReadDir(p.path(laravelConfigDir))
	if err != nil {
		return nil, fmt.Errorf("list config files: %w", err)
	}

	var out []string

	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".php") {
			out = append(out, strings.TrimSuffix(entry.Name(), ".php"))
		}
	}

	return out, nil
}

// evalScalar evaluates literal scalar nodes, returning ok=false for non-scalars.
func evalScalar(node ast.Vertex) (any, bool) {
	switch scalar := node.(type) {
	case *ast.ScalarString:
		return unquotePHP(string(scalar.Value)), true
	case *ast.ScalarLnumber:
		num, _ := strconv.ParseInt(string(scalar.Value), 0, 64)

		return num, true
	case *ast.ScalarDnumber:
		num, _ := strconv.ParseFloat(string(scalar.Value), 64)

		return num, true
	case *ast.ScalarEncapsed:
		var buf strings.Builder

		for _, part := range scalar.Parts {
			if strPart, isStr := part.(*ast.ScalarEncapsedStringPart); isStr {
				buf.Write(strPart.Value)
			}
		}

		return buf.String(), true
	default:
		return nil, false
	}
}

// evalPHP evaluates a config AST node to a Go value.
func (p *Project) evalPHP(node ast.Vertex) any {
	if val, ok := evalScalar(node); ok {
		return val
	}

	switch expr := node.(type) {
	case *ast.ExprArray:
		return p.evalArray(expr)
	case *ast.ExprFunctionCall:
		return p.evalCall(nameString(expr.Function), expr.Args)
	case *ast.ExprStaticCall:
		return p.evalStaticCall(nameString(expr.Class), identString(expr.Call), expr.Args)
	default:
		return p.evalOperator(node)
	}
}

// evalOperator handles constant/operator/expression nodes. Anything it doesn't
// model marks the eval lossy, so callers ask PHP instead of trusting the nil.
func (p *Project) evalOperator(node ast.Vertex) any {
	if val, ok := evalConstant(node); ok {
		return val
	}

	if val, ok := p.evalConversion(node); ok {
		return val
	}

	if val, ok := p.evalControl(node); ok {
		return val
	}

	p.evalLossy = true

	return nil
}

// evalConstant resolves nil, bare constants and class constants.
func evalConstant(node ast.Vertex) (any, bool) {
	switch expr := node.(type) {
	case nil:
		return nil, true
	case *ast.ExprConstFetch:
		return constFetchValue(nameString(expr.Const)), true
	case *ast.ExprClassConstFetch:
		// Foo::class evaluates to the class name as written; other class
		// constants we can't resolve statically, so report them descriptively.
		if identString(expr.Const) == "class" {
			return nameString(expr.Class), true
		}

		return nameString(expr.Class) + "::" + identString(expr.Const), true
	default:
		return nil, false
	}
}

// constFetchValue maps true/false/null (any case) to Go values; other
// constants evaluate to their name.
func constFetchValue(name string) any {
	switch strings.ToLower(name) {
	case phpLitTrue:
		return true
	case phpLitFalse:
		return false
	case phpLitNull:
		return nil
	default:
		return name
	}
}

// evalConversion handles casts, unary minus and string concatenation.
func (p *Project) evalConversion(node ast.Vertex) (any, bool) {
	switch expr := node.(type) {
	case *ast.ExprUnaryMinus:
		return phpNegate(p.evalPHP(expr.Expr)), true
	case *ast.ExprBinaryConcat:
		return phpString(p.evalPHP(expr.Left)) + phpString(p.evalPHP(expr.Right)), true
	case *ast.ExprCastString:
		return phpString(p.evalPHP(expr.Expr)), true
	case *ast.ExprCastBool:
		return phpTruthy(p.evalPHP(expr.Expr)), true
	case *ast.ExprCastInt:
		return int64(phpFloat(p.evalPHP(expr.Expr))), true
	case *ast.ExprCastDouble:
		return phpFloat(p.evalPHP(expr.Expr)), true
	default:
		return nil, false
	}
}

// phpNegate negates a number; anything else evaluates to nil.
func phpNegate(val any) any {
	switch num := val.(type) {
	case int64:
		return -num
	case float64:
		return -num
	default:
		return nil
	}
}

// evalControl handles ??, parentheses, ternaries and boolean not.
func (p *Project) evalControl(node ast.Vertex) (any, bool) {
	switch expr := node.(type) {
	case *ast.ExprBinaryCoalesce:
		if left := p.evalPHP(expr.Left); left != nil {
			return left, true
		}

		return p.evalPHP(expr.Right), true
	case *ast.ExprBrackets:
		return p.evalPHP(expr.Expr), true
	case *ast.ExprTernary:
		return p.evalTernary(expr), true
	case *ast.ExprBooleanNot:
		return !phpTruthy(p.evalPHP(expr.Expr)), true
	default:
		return nil, false
	}
}

// evalTernary evaluates `a ? b : c` and the short `a ?: c` form.
func (p *Project) evalTernary(expr *ast.ExprTernary) any {
	cond := p.evalPHP(expr.Cond)
	if !phpTruthy(cond) {
		return p.evalPHP(expr.IfFalse)
	}

	if expr.IfTrue != nil {
		return p.evalPHP(expr.IfTrue)
	}

	return cond
}

// phpString converts a value the way PHP's string conversion does: null and
// false are "", true is "1", arrays are "Array".
func phpString(val any) string {
	switch typed := val.(type) {
	case nil:
		return ""
	case bool:
		if typed {
			return "1"
		}

		return ""
	case string:
		return typed
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case []any, map[string]any:
		return "Array"
	default:
		return fmt.Sprint(val)
	}
}

// phpTruthy reports PHP truthiness: null, false, 0, 0.0, "", "0" and empty
// arrays are false; everything else is true.
func phpTruthy(val any) bool {
	switch typed := val.(type) {
	case nil:
		return false
	case bool:
		return typed
	case int64:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		return typed != "" && typed != "0"
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// phpFloat converts a scalar to a number the way PHP's numeric casts do for the
// values config files hold (leading-numeric strings, bools, null).
func phpFloat(val any) float64 {
	switch typed := val.(type) {
	case bool:
		if typed {
			return 1
		}

		return 0
	case int64:
		return float64(typed)
	case float64:
		return typed
	case string:
		num, _ := strconv.ParseFloat(leadingNumber.FindString(strings.TrimSpace(typed)), 64)

		return num
	default:
		return 0
	}
}

var leadingNumber = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?`)

// evalStaticCall handles the static helpers Laravel's stock config files call,
// such as Str::slug(env('APP_NAME', 'laravel'), '_') for cache and redis key
// prefixes. Any other call marks the eval lossy.
func (p *Project) evalStaticCall(class, method string, args []ast.Vertex) any {
	vals := p.evalArgs(args)

	if class == "Str" || class == "Illuminate\\Support\\Str" {
		arg := func(i int, def string) string {
			if i < len(vals) {
				return phpString(vals[i])
			}

			return def
		}

		switch method {
		case "slug":
			if slug, ok := strSlug(arg(0, ""), arg(1, "-")); ok {
				return slug
			}
		case "lower":
			return strings.ToLower(arg(0, ""))
		case "upper":
			return strings.ToUpper(arg(0, ""))
		default:
			// Other Str helpers fall through to the lossy placeholder.
		}
	}

	p.evalLossy = true

	return nil
}

// strSlug mirrors Laravel's Str::slug for ASCII input. It reports false for
// input it can't reproduce faithfully: non-ASCII text, which Laravel first
// transliterates, or an empty separator.
func strSlug(title, sep string) (string, bool) {
	if sep == "" {
		return "", false
	}

	for i := range len(title) {
		if title[i] >= utf8.RuneSelf {
			return "", false
		}
	}

	flip := "-"
	if sep == "-" {
		flip = "_"
	}

	qs := regexp.QuoteMeta(sep)
	title = regexp.MustCompile(`[`+regexp.QuoteMeta(flip)+`]+`).ReplaceAllLiteralString(title, sep)
	title = strings.ReplaceAll(title, "@", sep+"at"+sep)
	title = regexp.MustCompile(`[^`+qs+`\pL\pN\s]+`).ReplaceAllLiteralString(strings.ToLower(title), "")
	title = regexp.MustCompile(`[`+qs+`\s]+`).ReplaceAllLiteralString(title, sep)

	return strings.Trim(title, sep), true
}

// evalArray evaluates an array literal: a map when any item is keyed, else a
// list.
func (p *Project) evalArray(arr *ast.ExprArray) any {
	if arrayHasKey(arr) {
		return p.evalKeyedArray(arr)
	}

	out := []any{}

	for _, item := range arr.Items {
		if elem, isItem := item.(*ast.ExprArrayItem); isItem && elem.Val != nil && elem.EllipsisTkn == nil {
			out = append(out, p.evalPHP(elem.Val))
		}
	}

	return out
}

// arrayHasKey reports whether any non-spread item carries an explicit key.
func arrayHasKey(arr *ast.ExprArray) bool {
	for _, item := range arr.Items {
		if elem, isItem := item.(*ast.ExprArrayItem); isItem && elem.Key != nil && elem.EllipsisTkn == nil {
			return true
		}
	}

	return false
}

func (p *Project) evalKeyedArray(arr *ast.ExprArray) map[string]any {
	out := map[string]any{}
	idx := 0

	for _, item := range arr.Items {
		elem, isItem := item.(*ast.ExprArrayItem)
		// Skip spreads (`...$x`): they unpack a runtime value we can't
		// resolve statically. Mark the eval lossy so callers fall back to PHP.
		if isItem && elem.EllipsisTkn != nil {
			p.evalLossy = true

			continue
		}

		if !isItem || elem.Val == nil {
			continue
		}

		var key string
		if elem.Key != nil {
			key = phpString(p.evalPHP(elem.Key))
		} else {
			// Mixed array: unkeyed items take the next integer index.
			key = strconv.Itoa(idx)
			idx++
		}

		out[key] = p.evalPHP(elem.Val)
	}

	return out
}

// evalArgs evaluates a call's positional arguments.
func (p *Project) evalArgs(args []ast.Vertex) []any {
	vals := make([]any, 0, len(args))
	for _, node := range args {
		if arg, isArg := node.(*ast.Argument); isArg {
			vals = append(vals, p.evalPHP(arg.Expr))
		}
	}

	return vals
}

// evalCall handles the functions Laravel config files use.
func (p *Project) evalCall(name string, args []ast.Vertex) any {
	vals := p.evalArgs(args)

	if val, ok := p.evalBuiltin(name, vals); ok {
		return val
	}
	// Anything else, or a supported function called in a form not modeled
	// above.
	return p.unresolvedCall(name)
}

// evalBuiltin dispatches the modeled functions. ok=false means the function,
// or the form it was called in, isn't modeled.
func (p *Project) evalBuiltin(name string, vals []any) (any, bool) {
	if dir, isPathHelper := pathHelperDir(name); isPathHelper {
		return p.evalPathHelper(dir, vals), true
	}

	switch name {
	case "env":
		return p.evalEnvCall(vals), true
	case "explode":
		return evalExplode(vals)
	case "trim", "rtrim", "ltrim":
		return evalTrimCall(name, vals)
	case "array_filter":
		return evalArrayFilter(vals)
	case "parse_url":
		return evalParseURL(vals)
	default:
		return nil, false
	}
}

// evalTrimCall is trim/rtrim/ltrim($str[, $chars]).
func evalTrimCall(trimFn string, vals []any) (any, bool) {
	if len(vals) != 1 && len(vals) != phpArgPair {
		return nil, false
	}

	return phpTrim(trimFn, phpString(vals[0]), vals[1:]), true
}

// evalArrayFilter models only the callback-less form, which drops falsy
// values.
func evalArrayFilter(vals []any) (any, bool) {
	if len(vals) != 1 {
		return nil, false
	}

	return filterTruthy(vals[0]), true
}

// evalParseURL is parse_url($url, PHP_URL_*); the one-argument form returns
// an array we don't model.
func evalParseURL(vals []any) (any, bool) {
	if len(vals) != phpArgPair {
		return nil, false
	}

	return parseURLComponent(phpString(vals[0]), phpString(vals[1]))
}

// phpArgPair is the argument count of the two-argument call forms modeled.
const phpArgPair = 2

// evalEnvCall is env($key, $default): the .env value cast like Laravel does,
// else the default (or null).
func (p *Project) evalEnvCall(vals []any) any {
	if len(vals) == 0 {
		return nil
	}

	key := phpString(vals[0])
	if raw, found := p.envLookup(key); found {
		return castEnv(raw)
	}

	if len(vals) > 1 {
		return vals[1]
	}

	return nil
}

// pathHelperDir maps a Laravel path helper to its directory under the app
// root ("" for base_path).
func pathHelperDir(name string) (string, bool) {
	switch name {
	case "storage_path":
		return laravelStorageDir, true
	case "base_path":
		return "", true
	case "app_path":
		return laravelAppDir, true
	case "public_path":
		return "public", true
	case "resource_path":
		return "resources", true
	case "database_path":
		return laravelDBDir, true
	case "config_path":
		return laravelConfigDir, true
	default:
		return "", false
	}
}

// evalPathHelper is storage_path($sub) and friends: root/dir/sub.
func (p *Project) evalPathHelper(dir string, vals []any) string {
	parts := []string{p.Root}
	if dir != "" {
		parts = append(parts, dir)
	}

	if len(vals) > 0 {
		parts = append(parts, phpString(vals[0]))
	}

	return filepath.Join(parts...)
}

// evalExplode is explode($sep, $str) with a non-empty separator.
func evalExplode(vals []any) (any, bool) {
	if len(vals) != phpArgPair || phpString(vals[0]) == "" {
		return nil, false
	}

	parts := strings.Split(phpString(vals[1]), phpString(vals[0]))

	out := make([]any, len(parts))
	for i, part := range parts {
		out[i] = part
	}

	return out, true
}

// unresolvedCall marks the eval lossy and returns a placeholder for a call we
// can't evaluate statically.
func (p *Project) unresolvedCall(name string) any {
	p.evalLossy = true

	return fmt.Sprintf("<%s(...)>", name)
}

// phpTrim implements trim/rtrim/ltrim with PHP's default character set or an
// explicit one (ranges like "a..z" aren't supported and are taken literally).
func phpTrim(trimFn, str string, chars []any) string {
	cut := " \t\n\r\x00\x0B"
	if len(chars) == 1 {
		cut = phpString(chars[0])
	}

	switch trimFn {
	case "rtrim":
		return strings.TrimRight(str, cut)
	case "ltrim":
		return strings.TrimLeft(str, cut)
	default:
		return strings.Trim(str, cut)
	}
}

// filterTruthy is array_filter without a callback: falsy values are dropped.
// Lists stay lists (PHP would keep the original integer keys).
func filterTruthy(val any) any {
	switch typed := val.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, elem := range typed {
			if phpTruthy(elem) {
				out[k] = elem
			}
		}

		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, val := range typed {
			if phpTruthy(val) {
				out = append(out, val)
			}
		}

		return out
	default:
		return nil
	}
}

// parseURLComponent is parse_url($url, PHP_URL_*) for the components config
// files ask for. A missing component is null, as in PHP.
func parseURLComponent(raw, component string) (any, bool) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, true // PHP returns false; null reads the same to a config
	}

	if component == "PHP_URL_PORT" {
		port, perr := strconv.ParseInt(parsed.Port(), 10, 64)
		if perr != nil {
			return nil, true
		}

		return port, true
	}

	val, known := urlStringComponent(parsed, component)
	if !known {
		return nil, false
	}

	if val == "" {
		return nil, true
	}

	return val, true
}

// urlStringComponent returns the string-valued PHP_URL_* components.
func urlStringComponent(parsed *url.URL, component string) (string, bool) {
	switch component {
	case "PHP_URL_SCHEME":
		return parsed.Scheme, true
	case "PHP_URL_HOST":
		return parsed.Hostname(), true
	case "PHP_URL_USER":
		return parsed.User.Username(), true
	case "PHP_URL_PATH":
		return parsed.Path, true
	case "PHP_URL_QUERY":
		return parsed.RawQuery, true
	case "PHP_URL_FRAGMENT":
		return parsed.Fragment, true
	default:
		return "", false
	}
}

// castEnv mirrors Laravel's env() coercion of common string values.
func castEnv(raw string) any {
	switch strings.ToLower(raw) {
	case phpLitTrue, "(true)":
		return true
	case phpLitFalse, "(false)":
		return false
	case phpLitNull, "(null)":
		return nil
	case "empty", "(empty)":
		return ""
	default:
		// Anything else is a literal string, quotes aside.
	}
	// Strip surrounding quotes Laravel allows in .env values.
	if len(raw) >= phpQuotedMinLen && raw[0] == '"' && raw[len(raw)-1] == '"' {
		return raw[1 : len(raw)-1]
	}

	return raw
}

// phpQuotedMinLen is the shortest quoted string: just the two quotes.
const phpQuotedMinLen = 2

// identString returns the value of an *ast.Identifier node.
func identString(node ast.Vertex) string {
	if id, ok := node.(*ast.Identifier); ok {
		return string(id.Value)
	}

	return ""
}

// nameString extracts a dotted/plain name from a Name/NamePart node.
func nameString(node ast.Vertex) string {
	switch name := node.(type) {
	case *ast.Name:
		var parts []string

		for _, part := range name.Parts {
			if namePart, isPart := part.(*ast.NamePart); isPart {
				parts = append(parts, string(namePart.Value))
			}
		}

		return strings.Join(parts, "\\")
	case *ast.NamePart:
		return string(name.Value)
	default:
		return ""
	}
}

// unquotePHP strips quotes from a PHP string literal token and unescapes the
// minimal set of escapes that appear in config files.
func unquotePHP(lit string) string {
	if len(lit) < phpQuotedMinLen {
		return lit
	}

	quote := lit[0]
	if (quote != '\'' && quote != '"') || lit[len(lit)-1] != quote {
		return lit
	}

	inner := lit[1 : len(lit)-1]
	if quote == '\'' {
		inner = strings.ReplaceAll(inner, `\'`, `'`)
		inner = strings.ReplaceAll(inner, `\\`, `\`)

		return inner
	}

	unescape := strings.NewReplacer(`\"`, `"`, `\\`, `\`, `\n`, "\n", `\t`, "\t", `\r`, "\r")

	return unescape.Replace(inner)
}
