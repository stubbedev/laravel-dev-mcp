package app

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/VKCOM/php-parser/pkg/ast"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// modelRelationType returns the canonical name of the Eloquent relation
// builder a $this->method() call names (matched case-insensitively, as PHP
// does), or "" when it isn't one.
func modelRelationType(method string) string {
	for _, relation := range [...]string{
		"hasOne", "hasMany", "belongsTo", "belongsToMany", "hasOneThrough", "hasManyThrough",
		"morphTo", "morphOne", "morphMany", "morphToMany", "morphedByMany",
	} {
		if strings.EqualFold(method, relation) {
			return relation
		}
	}

	return ""
}

// modelScanRoots are the directories scanned for Eloquent models, covering the
// standard layout plus modular apps (app/Modules, top-level Modules/, src/).
func modelScanRoots() []string {
	return []string{laravelAppDir, "Modules", "src"}
}

// modelSkipDir reports directories never descended when scanning for models.
func modelSkipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", ".git", laravelStorageDir, "bootstrap", "tests":
		return true
	default:
		return false
	}
}

type modelRelation struct {
	Method  string `json:"method"`
	Type    string `json:"type"`
	Related string `json:"related,omitempty"`
}

type modelInfo struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Class     string            `json:"class"`
	File      string            `json:"file"`
	Extends   string            `json:"extends,omitempty"`
	Table     string            `json:"table,omitempty"`
	Fillable  []string          `json:"fillable,omitempty"`
	Guarded   []string          `json:"guarded,omitempty"`
	Casts     map[string]string `json:"casts,omitempty"`
	Relations []modelRelation   `json:"relations,omitempty"`
	Factory   string            `json:"factory,omitempty"`
	// Partial marks a model whose file uses PHP syntax newer than the parser
	// understands; the fields above may be incomplete.
	Partial bool `json:"partial,omitempty"`
}

// factoryFor returns the relative path of a model's factory by Laravel's naming
// convention (database/factories/<Name>Factory.php), or "" when none exists.
func (p *Project) factoryFor(name string) string {
	rel := filepath.Join(laravelDBDir, "factories", name+"Factory.php")

	info, err := os.Stat(p.path(rel))
	if err == nil && !info.IsDir() {
		return rel
	}

	return ""
}

// modelSummary is the list-mode row: lightweight so a large app doesn't flood
// the context.
type modelSummary struct {
	Name      string `json:"name"`
	Class     string `json:"class"`
	Table     string `json:"table,omitempty"`
	File      string `json:"file"`
	Factory   string `json:"factory,omitempty"`
	Relations int    `json:"relations"`
	Partial   bool   `json:"partial,omitempty"`
}

// modelsResultKey is the list-mode result key holding the summaries.
const modelsResultKey = "models"

func models(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	proj, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}

	found := proj.scanModels()
	if len(found) == 0 {
		return textResult("No Eloquent models found under app/, Modules/, or src/."), nil
	}

	for i := range found {
		found[i].Factory = proj.factoryFor(found[i].Name)
	}

	slices.SortFunc(found, func(a, b modelInfo) int { return strings.Compare(a.Class, b.Class) })

	// Detail mode: a name/class filter returns full info for matches.
	if query := argString(args, "model"); query != "" {
		matches := matchModels(found, query)
		if len(matches) == 0 {
			return textResult("No model matching " + query), nil
		}

		return jsonResult(ctx, matches), nil
	}

	out := summarizeModels(found)

	return jsonResult(ctx, map[string]any{
		"count":         len(out),
		modelsResultKey: out,
		keyNote:         "Pass `model` (name or class substring) for full columns/casts/relations.",
	}), nil
}

// matchModels returns the models whose name matches query (exactly or
// case-insensitively) or whose class contains it.
func matchModels(found []modelInfo, query string) []modelInfo {
	var matches []modelInfo

	for _, info := range found {
		if info.Name == query || strings.EqualFold(info.Name, query) || strings.Contains(info.Class, query) {
			matches = append(matches, info)
		}
	}

	return matches
}

func summarizeModels(found []modelInfo) []modelSummary {
	out := make([]modelSummary, 0, len(found))
	for _, info := range found {
		out = append(out, modelSummary{
			Name:      info.Name,
			Class:     info.Class,
			Table:     info.Table,
			File:      info.File,
			Factory:   info.Factory,
			Relations: len(info.Relations),
			Partial:   info.Partial,
		})
	}

	return out
}

// scanModels walks the project's source roots and returns every class that
// looks like an Eloquent model.
func (p *Project) scanModels() []modelInfo {
	var out []modelInfo

	for _, root := range modelScanRoots() {
		dir := p.path(root)

		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}

		_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, _ error) error {
			if entry == nil {
				return nil
			}

			if entry.IsDir() {
				if modelSkipDir(entry.Name()) {
					return fs.SkipDir
				}

				return nil
			}

			if strings.HasSuffix(path, ".php") && looksLikeModelFile(path) {
				out = append(out, p.modelsInFile(path)...)
			}

			return nil
		})
	}

	return out
}

// modelsInFile returns the models declared in one PHP file; a file that
// fails to parse contributes none.
func (p *Project) modelsInFile(path string) []modelInfo {
	file, err := p.parseFileAST(path)
	if err != nil {
		return nil
	}

	rel, _ := filepath.Rel(p.Root, path)

	found := p.modelsInStmts(file.root.Stmts, "", rel)
	if len(found) == 0 && file.partial {
		// The parser dropped the class itself; keep the model visible from
		// its declaration line.
		if info, ok := modelFromHeader(path, rel); ok {
			found = append(found, info)
		}
	}

	for i := range found {
		found[i].Partial = file.partial
	}

	return found
}

// looksLikeModelFile is a cheap byte pre-filter to avoid AST-parsing every PHP
// file in a large app.
func looksLikeModelFile(path string) bool {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: scanning the project's own source is the point
	if err != nil || !bytes.Contains(raw, []byte("extends")) {
		return false
	}

	for _, marker := range [][]byte{
		[]byte("Model"), []byte("Authenticatable"), []byte("Pivot"),
		[]byte("$table"), []byte("$fillable"), []byte("$guarded"), []byte("$casts"),
	} {
		if bytes.Contains(raw, marker) {
			return true
		}
	}

	return false
}

// Declaration patterns for modelFromHeader.
var (
	namespaceDeclRe = regexp.MustCompile(`(?m)^\s*namespace\s+([\w\\]+)\s*[;{]`)
	classDeclRe     = regexp.MustCompile(
		`(?m)^\s*(?:(?:abstract|final|readonly)\s+)*class\s+(\w+)\s+extends\s+([\w\\]+)`,
	)
	tablePropRe = regexp.MustCompile(`\$table\s*=\s*['"]([^'"]+)['"]`)
)

// modelFromHeader recovers a model's identity (namespace, class, parent,
// $table) by pattern-matching its source, for files whose class statement the
// parser could not recover from. Members aren't extracted.
func modelFromHeader(path, rel string) (modelInfo, bool) {
	var none modelInfo

	raw, err := os.ReadFile(path) //nolint:gosec // G304: scanning the project's own source is the point
	if err != nil {
		return none, false
	}

	classMatch := classDeclRe.FindSubmatch(raw)
	if classMatch == nil || !modelish(string(classMatch[2])) {
		return none, false
	}

	name := string(classMatch[1])
	info := newModelInfo(name, "", rel, string(classMatch[2]))

	if nsMatch := namespaceDeclRe.FindSubmatch(raw); nsMatch != nil {
		info.Namespace = string(nsMatch[1])
		info.Class = info.Namespace + "\\" + info.Name
	}

	if tableMatch := tablePropRe.FindSubmatch(raw); tableMatch != nil {
		info.Table = string(tableMatch[1])
	}

	return info, true
}

// newModelInfo returns a model's identity; its class is namespace-qualified
// when namespace is set. Members are filled in by the caller.
func newModelInfo(name, namespace, file, extends string) modelInfo {
	class := name
	if namespace != "" {
		class = namespace + "\\" + name
	}

	return modelInfo{
		Name:      name,
		Namespace: namespace,
		Class:     class,
		File:      file,
		Extends:   extends,
		Table:     "",
		Fillable:  nil,
		Guarded:   nil,
		Casts:     nil,
		Relations: nil,
		Factory:   "",
		Partial:   false,
	}
}

func (p *Project) modelsInStmts(stmts []ast.Vertex, ns, file string) []modelInfo {
	var out []modelInfo

	cur := ns

	for _, stmt := range stmts {
		switch node := stmt.(type) {
		case *ast.StmtNamespace:
			// Braced `namespace X { ... }` nests its statements; braceless
			// `namespace X;` has no children and applies to following siblings.
			if len(node.Stmts) > 0 {
				out = append(out, p.modelsInStmts(node.Stmts, nameString(node.Name), file)...)
			} else {
				cur = nameString(node.Name)
			}
		case *ast.StmtClass:
			if info, ok := p.classToModel(node, cur, file); ok {
				out = append(out, info)
			}
		default:
			// Other top-level statements (use, functions, …) declare no models.
		}
	}

	return out
}

// varName returns a variable/property name without the leading '$' (the parser
// keeps the sigil in the identifier).
func varName(v ast.Vertex) string {
	ev, ok := v.(*ast.ExprVariable)
	if !ok {
		return ""
	}

	return strings.TrimPrefix(identString(ev.Name), "$")
}

func (p *Project) classToModel(class *ast.StmtClass, namespace, file string) (modelInfo, bool) {
	var none modelInfo

	name := identString(class.Name)
	if name == "" {
		return none, false
	}

	info := newModelInfo(name, namespace, file, nameString(class.Extends))
	isModel := modelish(info.Extends)

	for _, stmt := range class.Stmts {
		switch member := stmt.(type) {
		case *ast.StmtPropertyList:
			if p.applyModelProperty(&info, member) {
				isModel = true
			}
		case *ast.StmtClassMethod:
			if p.applyModelMethod(&info, member) {
				isModel = true
			}
		default:
			// Constants, trait uses, … carry nothing we report.
		}
	}

	return info, isModel
}

// applyModelMethod records a casts() method or a relation method and reports
// whether the method was one of them.
func (p *Project) applyModelMethod(info *modelInfo, method *ast.StmtClassMethod) bool {
	// Laravel 11+ defines casts via a casts() method; it takes precedence
	// over a legacy $casts property.
	if casts, ok := p.extractCastsMethod(method); ok {
		info.Casts = casts

		return true
	}

	if rel, ok := p.extractRelation(method); ok {
		info.Relations = append(info.Relations, rel)

		return true
	}

	return false
}

// applyModelProperty records $table/$fillable/$guarded/$casts and reports
// whether the property was one of them.
func (p *Project) applyModelProperty(info *modelInfo, list *ast.StmtPropertyList) bool {
	matched := false

	for _, node := range list.Props {
		prop, isProp := node.(*ast.StmtProperty)
		if !isProp {
			continue
		}

		switch varName(prop.Var) {
		case keyTable:
			if table, isStr := p.evalPHP(prop.Expr).(string); isStr {
				info.Table = table
			}

			matched = true
		case "fillable":
			info.Fillable = toStrSlice(p.evalPHP(prop.Expr))
			matched = true
		case "guarded":
			info.Guarded = toStrSlice(p.evalPHP(prop.Expr))
			matched = true
		case "casts":
			info.Casts = toStrMap(p.evalPHP(prop.Expr))
			matched = true
		default:
			// Any other property is not part of the model's schema.
		}
	}

	return matched
}

// extractCastsMethod reads a Laravel 11 `protected function casts(): array {
// return [...]; }` body into a casts map.
func (p *Project) extractCastsMethod(method *ast.StmtClassMethod) (map[string]string, bool) {
	if !strings.EqualFold(identString(method.Name), "casts") {
		return nil, false
	}

	body, ok := method.Stmt.(*ast.StmtStmtList)
	if !ok {
		return nil, false
	}

	for _, st := range body.Stmts {
		ret, ok := st.(*ast.StmtReturn)
		if !ok {
			continue
		}

		if arr, ok := ret.Expr.(*ast.ExprArray); ok {
			return toStrMap(p.evalArray(arr)), true
		}
	}

	return nil, false
}

// extractRelation detects `return $this-><relation>(Related::class, ...)` method
// bodies, unwrapping any chained calls (->withDefault(), ->where(), ...).
func (p *Project) extractRelation(method *ast.StmtClassMethod) (modelRelation, bool) {
	var none modelRelation

	body, ok := method.Stmt.(*ast.StmtStmtList)
	if !ok {
		return none, false
	}

	for _, stmt := range body.Stmts {
		ret, isReturn := stmt.(*ast.StmtReturn)
		if !isReturn {
			continue
		}

		call := baseRelationCall(ret.Expr)
		if call == nil {
			continue
		}

		return modelRelation{
			Method:  identString(method.Name),
			Type:    modelRelationType(identString(call.Method)),
			Related: p.relatedClass(call),
		}, true
	}

	return none, false
}

// relatedClass is the relation's first argument (Related::class) when it
// evaluates to a string, else "".
func (p *Project) relatedClass(call *ast.ExprMethodCall) string {
	if len(call.Args) == 0 {
		return ""
	}

	arg, isArg := call.Args[0].(*ast.Argument)
	if !isArg {
		return ""
	}

	related, isStr := p.evalPHP(arg.Expr).(string)
	if !isStr {
		return ""
	}

	return related
}

// baseRelationCall walks down a (possibly chained) method-call expression and
// returns the $this-><relation>(...) call, or nil.
func baseRelationCall(expr ast.Vertex) *ast.ExprMethodCall {
	for {
		call, ok := expr.(*ast.ExprMethodCall)
		if !ok {
			return nil
		}

		if isThis(call.Var) && modelRelationType(identString(call.Method)) != "" {
			return call
		}

		expr = call.Var
	}
}

func isThis(v ast.Vertex) bool {
	return varName(v) == "this"
}

func modelish(extends string) bool {
	e := strings.ToLower(extends)

	return strings.Contains(e, "model") || strings.Contains(e, "authenticatable") || strings.Contains(e, "pivot")
}

func toStrSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(arr))
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

func toStrMap(v any) map[string]string {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil
	}

	out := make(map[string]string, len(obj))
	for k, val := range obj {
		out[k] = strings.TrimSpace(strings.Trim(strings.ReplaceAll(toScalarString(val), "\n", " "), " "))
	}

	return out
}

func toScalarString(v any) string {
	if v == nil {
		return ""
	}

	if s, ok := v.(string); ok {
		return s
	}

	return ""
}
