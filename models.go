package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/VKCOM/php-parser/pkg/ast"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// relationMethods are the Eloquent relation builders we recognize on $this.
var relationMethods = map[string]string{
	"hasone": "hasOne", "hasmany": "hasMany", "belongsto": "belongsTo",
	"belongstomany": "belongsToMany", "hasonethrough": "hasOneThrough",
	"hasmanythrough": "hasManyThrough", "morphto": "morphTo", "morphone": "morphOne",
	"morphmany": "morphMany", "morphtomany": "morphToMany", "morphedbymany": "morphedByMany",
}

// modelScanRoots are the directories scanned for Eloquent models, covering the
// standard layout plus modular apps (app/Modules, top-level Modules/, src/).
var modelScanRoots = []string{"app", "Modules", "src"}

// skipDirs are never descended when scanning for models.
var skipDirs = map[string]bool{
	"vendor": true, "node_modules": true, ".git": true,
	"storage": true, "bootstrap": true, "tests": true,
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
}

func models(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (toolResult, error) {
	p, err := resolveProject(ctx, req)
	if err != nil {
		return toolResult{}, err
	}
	found := p.scanModels()
	if len(found) == 0 {
		return textResult("No Eloquent models found under app/, Modules/, or src/."), nil
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Class < found[j].Class })

	// Detail mode: a name/class filter returns full info for matches.
	if q := argString(args, "model"); q != "" {
		var matches []modelInfo
		for _, m := range found {
			if m.Name == q || strings.EqualFold(m.Name, q) || strings.Contains(m.Class, q) {
				matches = append(matches, m)
			}
		}
		if len(matches) == 0 {
			return textResult("No model matching " + q), nil
		}
		return jsonResult(ctx, matches), nil
	}

	// List mode: lightweight summary so a large app doesn't flood the context.
	type summary struct {
		Name      string `json:"name"`
		Class     string `json:"class"`
		Table     string `json:"table,omitempty"`
		File      string `json:"file"`
		Relations int    `json:"relations"`
	}
	out := make([]summary, 0, len(found))
	for _, m := range found {
		out = append(out, summary{m.Name, m.Class, m.Table, m.File, len(m.Relations)})
	}
	return jsonResult(ctx, map[string]any{
		"count":  len(out),
		"models": out,
		"note":   "Pass `model` (name or class substring) for full columns/casts/relations.",
	}), nil
}

// scanModels walks the project's source roots and returns every class that
// looks like an Eloquent model.
func (p *Project) scanModels() []modelInfo {
	var out []modelInfo
	for _, root := range modelScanRoots {
		dir := p.path(root)
		if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
			continue
		}
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, _ error) error {
			if d == nil {
				return nil
			}
			if d.IsDir() {
				if skipDirs[d.Name()] {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(path, ".php") && looksLikeModelFile(path) {
				if r, perr := p.parseFileAST(path); perr == nil {
					rel, _ := filepath.Rel(p.Root, path)
					out = append(out, p.modelsInStmts(r.Stmts, "", rel)...)
				}
			}
			return nil
		})
	}
	return out
}

// looksLikeModelFile is a cheap byte pre-filter to avoid AST-parsing every PHP
// file in a large app.
func looksLikeModelFile(path string) bool {
	raw, err := os.ReadFile(path)
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

func (p *Project) modelsInStmts(stmts []ast.Vertex, ns, file string) []modelInfo {
	var out []modelInfo
	cur := ns
	for _, st := range stmts {
		switch n := st.(type) {
		case *ast.StmtNamespace:
			// Braced `namespace X { ... }` nests its statements; braceless
			// `namespace X;` has no children and applies to following siblings.
			if len(n.Stmts) > 0 {
				out = append(out, p.modelsInStmts(n.Stmts, nameString(n.Name), file)...)
			} else {
				cur = nameString(n.Name)
			}
		case *ast.StmtClass:
			if mi, ok := p.classToModel(n, cur, file); ok {
				out = append(out, mi)
			}
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

func (p *Project) classToModel(c *ast.StmtClass, ns, file string) (modelInfo, bool) {
	name := identString(c.Name)
	if name == "" {
		return modelInfo{}, false
	}
	mi := modelInfo{Name: name, Namespace: ns, File: file, Extends: nameString(c.Extends)}
	if ns != "" {
		mi.Class = ns + "\\" + name
	} else {
		mi.Class = name
	}

	isModel := modelish(mi.Extends)
	for _, st := range c.Stmts {
		switch s := st.(type) {
		case *ast.StmtPropertyList:
			if p.applyModelProperty(&mi, s) {
				isModel = true
			}
		case *ast.StmtClassMethod:
			// Laravel 11+ defines casts via a casts() method; it takes
			// precedence over a legacy $casts property.
			if c, ok := p.extractCastsMethod(s); ok {
				mi.Casts = c
				isModel = true
			} else if rel, ok := p.extractRelation(s); ok {
				mi.Relations = append(mi.Relations, rel)
				isModel = true
			}
		}
	}
	return mi, isModel
}

// applyModelProperty records $table/$fillable/$guarded/$casts and reports
// whether the property was one of them.
func (p *Project) applyModelProperty(mi *modelInfo, list *ast.StmtPropertyList) bool {
	matched := false
	for _, pr := range list.Props {
		prop, ok := pr.(*ast.StmtProperty)
		if !ok {
			continue
		}
		switch varName(prop.Var) {
		case "table":
			if s, ok := p.evalPHP(prop.Expr).(string); ok {
				mi.Table = s
			}
			matched = true
		case "fillable":
			mi.Fillable = toStrSlice(p.evalPHP(prop.Expr))
			matched = true
		case "guarded":
			mi.Guarded = toStrSlice(p.evalPHP(prop.Expr))
			matched = true
		case "casts":
			mi.Casts = toStrMap(p.evalPHP(prop.Expr))
			matched = true
		}
	}
	return matched
}

// extractCastsMethod reads a Laravel 11 `protected function casts(): array {
// return [...]; }` body into a casts map.
func (p *Project) extractCastsMethod(m *ast.StmtClassMethod) (map[string]string, bool) {
	if !strings.EqualFold(identString(m.Name), "casts") {
		return nil, false
	}
	body, ok := m.Stmt.(*ast.StmtStmtList)
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
func (p *Project) extractRelation(m *ast.StmtClassMethod) (modelRelation, bool) {
	body, ok := m.Stmt.(*ast.StmtStmtList)
	if !ok {
		return modelRelation{}, false
	}
	for _, st := range body.Stmts {
		ret, ok := st.(*ast.StmtReturn)
		if !ok {
			continue
		}
		call := baseRelationCall(ret.Expr)
		if call == nil {
			continue
		}
		typ := relationMethods[strings.ToLower(identString(call.Method))]
		rel := modelRelation{Method: identString(m.Name), Type: typ}
		if len(call.Args) > 0 {
			if arg, ok := call.Args[0].(*ast.Argument); ok {
				if s, ok := p.evalPHP(arg.Expr).(string); ok {
					rel.Related = s
				}
			}
		}
		return rel, true
	}
	return modelRelation{}, false
}

// baseRelationCall walks down a (possibly chained) method-call expression and
// returns the $this-><relation>(...) call, or nil.
func baseRelationCall(expr ast.Vertex) *ast.ExprMethodCall {
	for {
		call, ok := expr.(*ast.ExprMethodCall)
		if !ok {
			return nil
		}
		if isThis(call.Var) && relationMethods[strings.ToLower(identString(call.Method))] != "" {
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
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
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
