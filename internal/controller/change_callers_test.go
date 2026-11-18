/*
Copyright 2026 The Shadewright Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	pathpkg "path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	api "github.com/pkalin/shadewright/api/v1alpha1"
)

// pullRequestReads are the GitProvider methods that read pull requests, or a branch (BranchHead), with the
// functions of this package allowed to call them. Every other function asks changeOf (#688), so one rule per
// verdict holds for all of them. gatedGit only passes the calls on (killswitch.go).
var pullRequestReads = map[string]map[string]bool{
	opGet:  {"changeByNumber": true, "gatedGit.GetChangeRequest": true},
	opList: {"checkOpenListing": true, "changeByNumber": true, "changeOnBranch": true, "gatedGit.ListChangeRequests": true},
	opFind: {"gatedGit.FindChangeRequest": true},
	// BranchHead reads a head, never whether a pull request exists: the step reads it to cut the branch (a 404
	// creates it, and CreateBranch refuses one that exists at another commit), the handoff to update it, and the
	// recovery decides a 404 through changeOf.
	opBranchHead: {"pullRequestStep": true, "takeOver": true, "recoverStep": true, "gatedGit.BranchHead": true},
}

// TestPullRequestReadsGoThroughChangeOf fails when code of this package outside the functions pullRequestReads
// allows refers to one of its methods: a call, a method value (f := git.GetChangeRequest) or a method expression,
// in a function or in a package-level initializer, which no function owns and so is never allowed.
func TestPullRequestReadsGoThroughChangeOf(t *testing.T) {
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		if read, ok := forbiddenRead(owner, n); ok {
			t.Errorf("%s: %q refers to %s; read pull requests through changeOf", fset.Position(n.Pos()), owner, read)
		}
	})
}

// forbiddenRead reports whether n, in the function owner ("" for a package-level declaration), refers to a
// method of pullRequestReads that owner may not use, and which.
func forbiddenRead(owner string, n ast.Node) (string, bool) {
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	allowed, read := pullRequestReads[sel.Sel.Name]
	return sel.Sel.Name, read && !allowed[owner]
}

// forEachSource calls visit for every node of the non-test files of this package, with the name of the function
// it is in (funcName), or "" for a package-level declaration.
func forEachSource(t *testing.T, visit func(fset *token.FileSet, owner string, n ast.Node)) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name) // #nosec G304 -- a file of this package, listed by Glob.
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		forEachDecl(f, func(owner string, n ast.Node) { visit(fset, owner, n) })
	}
}

// forEachDecl calls visit for every node of f, with the name of the function it is in, or "".
func forEachDecl(f *ast.File, visit func(owner string, n ast.Node)) {
	for _, decl := range f.Decls {
		owner := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			owner = funcName(fn)
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if n != nil {
				visit(owner, n)
			}
			return true
		})
	}
}

// TestPullRequestReadGuardCatchesEveryReference checks the guard above on code it must refuse: a method value
// and a package-level initializer, besides a call.
func TestPullRequestReadGuardCatchesEveryReference(t *testing.T) {
	const src = `package controller
var get = providers.GitProvider.GetChangeRequest
func other(git providers.GitProvider) {
	f := git.ListChangeRequests
	_ = f
	_, _ = git.GetChangeRequest(nil, 1)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	forEachDecl(f, func(owner string, n ast.Node) {
		if read, ok := forbiddenRead(owner, n); ok {
			found = append(found, owner+":"+read)
		}
	})
	want := []string{":GetChangeRequest", "other:ListChangeRequests", "other:GetChangeRequest"}
	if !slices.Equal(found, want) {
		t.Errorf("found %v, want %v", found, want)
	}
}

// lockWriters are the functions of this package that may write status.holdsTarget, and releasers those that may call
// lockverdict.Release, the one status.holdsTarget=false writer (in its own package); acquire is the one that takes
// the lock. Only settleTargetLock
// may release a lock for Git work; settleRecorded and checkHeld (through releaseUnread) release one whose status
// records its merge (settleRecordedMerge, as main's settleLock did), or an undelivered merge whose repository left the
// settings (settleUndeliveredGone). Other allowed callers never acquired a lock (admission refusal and duplicates),
// transfer it at a completed handoff, or record a lock already free before the field existed (backfill, with the
// verdict settleReleasedBeforeField). No stopped request, including an Incident cancellation, has a separate release
// rule.
const releaseFunc = "Release"

var (
	lockWriters = map[string]bool{"acquire": true}
	releasers   = map[string]bool{
		"settleTargetLock": true, "releaseUnread": true, "settleRecorded": true, "releaseRefused": true, "supersedeDuplicate": true,
		"endHandoff": true, "supersedePredecessor": true, "cancelUndecided": true, "backfill": true,
	}
	// statusReplacers may replace a request's whole status: restoreStatus puts back the status the same caller saved
	// earlier in the same reconcile, which holds the lock as it was then (a release since then is undone, so the lock
	// stays held: fail closed), and no acquire runs between the save and the restore.
	statusReplacers = map[string]bool{"restoreStatus": true}
)

// lockWrites lists the ways node n of function owner writes status.holdsTarget outside the rule: an assignment to
// the field or through it (*rr.Status.HoldsTarget = false), its address taken or its pointer copied (an alias that
// can write it later), a composite literal that sets it, a replacement of a whole status, a DeepCopyInto into a
// status, a string that names the field (a raw or JSON patch), and a call of release outside releasers.
func lockWrites(owner string, n ast.Node) []string {
	switch n := n.(type) {
	case *ast.AssignStmt:
		return assignWrites(owner, n)
	case *ast.ValueSpec:
		return pointerCopies(n.Values, "copies the pointer of status.holdsTarget")
	case *ast.UnaryExpr:
		if n.Op == token.AND && isLockField(n.X) {
			return []string{"takes the address of status.holdsTarget"}
		}
	case *ast.KeyValueExpr:
		if id, ok := n.Key.(*ast.Ident); ok && id.Name == holdsTargetField {
			return []string{"sets status.holdsTarget in a literal"}
		}
	case *ast.BasicLit:
		if n.Kind == token.STRING && strings.Contains(strings.ToLower(n.Value), "holdstarget") && owner != "" {
			return []string{"names status.holdsTarget in a string (a patch)"}
		}
	case *ast.ReturnStmt:
		return pointerCopies(n.Results, "returns the pointer of status.holdsTarget")
	case *ast.CallExpr:
		return callWrites(owner, n)
	}
	return nil
}

// holdsTargetField is the Go name of status.holdsTarget.
const holdsTargetField = "HoldsTarget"

// isLockField reports whether e is status.holdsTarget, through any parentheses and dereferences.
func isLockField(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.SelectorExpr:
			return x.Sel.Name == holdsTargetField
		default:
			return false
		}
	}
}

// isWholeStatus reports whether e is a request's whole status (x.Status).
func isWholeStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Status"
}

// pointerCopies is what for each of exprs that is the pointer of status.holdsTarget itself.
func pointerCopies(exprs []ast.Expr, what string) []string {
	var found []string
	for _, e := range exprs {
		if sel, ok := e.(*ast.SelectorExpr); ok && sel.Sel.Name == holdsTargetField {
			found = append(found, what)
		}
	}
	return found
}

// assignWrites lists the lock writes of an assignment: to the field or through it, a whole status replaced, the
// pointer copied.
func assignWrites(owner string, n *ast.AssignStmt) []string {
	var found []string
	for _, lhs := range n.Lhs {
		switch {
		case isLockField(lhs) && !lockWriters[owner]:
			found = append(found, "assigns status.holdsTarget")
		case isWholeStatus(lhs) && !statusReplacers[owner]:
			found = append(found, "replaces a whole status")
		}
	}
	return append(found, pointerCopies(n.Rhs, "copies the pointer of status.holdsTarget")...)
}

// callWrites lists the lock writes of a call: the pointer handed to a function (ptr.Deref only reads it), a
// release outside releasers, and a copy or decode into a whole status.
func callWrites(owner string, n *ast.CallExpr) []string {
	var found []string
	sel, isSel := n.Fun.(*ast.SelectorExpr)
	if !isSel || sel.Sel.Name != "Deref" {
		found = append(found, pointerCopies(n.Args, "passes the pointer of status.holdsTarget")...)
	}
	if isSel && sel.Sel.Name == releaseFunc && isPackage(sel.X, "lockverdict") && !releasers[owner] {
		found = append(found, "releases the lock outside the lock rule")
	}
	if isSel && (sel.Sel.Name == "DeepCopyInto" || sel.Sel.Name == "Unmarshal") && len(n.Args) > 0 {
		if u, ok := n.Args[len(n.Args)-1].(*ast.UnaryExpr); ok && u.Op == token.AND && isWholeStatus(u.X) &&
			!statusReplacers[owner] {
			found = append(found, "copies into a whole status")
		}
	}
	return found
}

// TestLockClearedInOnePlace fails when any function writes status.holdsTarget other than acquire (and
// lockverdict.Release, in its own package), a function outside releasers calls lockverdict.Release, or a status is replaced whole or patched by name outside the allowed
// places (#688, decision of 2026-11-17 on #771): a caller such as the deadline's cleanup, which closes only the pull
// requests with this request's marker, never releases the lock on its own. The package-level field selector of the
// limits (lockField, owner "") only reads it.
func TestLockClearedInOnePlace(t *testing.T) {
	forEachSource(t, func(fset *token.FileSet, owner string, n ast.Node) {
		for _, what := range lockWrites(owner, n) {
			t.Errorf("%s: %q %s; take or release the lock through acquire and release", fset.Position(n.Pos()),
				owner, what)
		}
	})
}

// TestLockGuardCatchesEveryWrite checks the guard above on code it must refuse: a write through the pointer, an
// alias of the pointer, the address of the field, a composite literal, a whole status replaced or copied into, a
// patch that names the field, and a release outside the lock rule; and that acquire, release and restoreStatus
// pass.
func TestLockGuardCatchesEveryWrite(t *testing.T) {
	const src = `package controller
func star(rr *R) { *rr.Status.HoldsTarget = false }
func plain(rr *R) { rr.Status.HoldsTarget = new(false) }
func alias(rr *R) { p := rr.Status.HoldsTarget; *p = false }
func varAlias(rr *R) { var p = rr.Status.HoldsTarget; _ = p }
func passed(rr *R) { clear(rr.Status.HoldsTarget) }
func returned(rr *R) *bool { return rr.Status.HoldsTarget }
func parenStar(rr *R) { *(rr.Status.HoldsTarget) = false }
func viaStatus(rr *R) { s := &rr.Status; *s.HoldsTarget = false }
func reads(rr *R) bool { return ptr.Deref(rr.Status.HoldsTarget, false) }
func addr(rr *R) { set(&rr.Status.HoldsTarget) }
func literal() S { return S{HoldsTarget: new(false)} }
func replace(rr, o *R) { rr.Status = o.Status }
func copyInto(rr, o *R) { o.Status.DeepCopyInto(&rr.Status) }
func decode(rr *R, b []byte) { _ = json.Unmarshal(b, &rr.Status) }
func patch(rr *R) { _ = client.RawPatch(types.MergePatchType, []byte(` + "`" + `{"status":{"holdsTarget":false}}` + "`" + `)) }
func early(rr *R) { _ = lockverdict.Release(&rr.Status, s) }
func release(rr *R, s L) { rr.Status.HoldsTarget = new(false) }
func acquire(rr *R) { rr.Status.HoldsTarget = new(true) }
func releaseRefused(rr *R) { _ = lockverdict.Release(&rr.Status, s) }
func restoreStatus(rr *R, before *S) { rr.Status = *before }
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	caught := map[string]bool{}
	forEachDecl(f, func(owner string, n ast.Node) {
		if len(lockWrites(owner, n)) > 0 {
			caught[owner] = true
		}
	})
	for _, owner := range []string{"star", "plain", "alias", "varAlias", "passed", "returned", "parenStar", "viaStatus",
		"addr", "literal", "replace", "copyInto", "decode", "patch", "early", "release"} {
		if !caught[owner] {
			t.Errorf("the guard misses the write in %s", owner)
		}
	}
	for _, owner := range []string{"acquire", "releaseRefused", "restoreStatus", "reads"} {
		if caught[owner] {
			t.Errorf("the guard refuses %s", owner)
		}
	}
}

// settlementBuilders are the only functions of this package that may call a constructor of package lockverdict:
// each is called by the read or the state that proves its verdict.
var settlementBuilders = map[string]bool{
	"settleByRead": true, "settleNeverHeld": true, "settleHandedOver": true, "settleHandoffAborted": true,
	"settleReleasedBeforeField": true, "settleRecordedMerge": true, "settleUndeliveredGone": true,
}

// isPackage reports whether e is the package name pkg.
func isPackage(e ast.Expr, pkg string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == pkg
}

// lockverdictPath is the import path of package lockverdict.
const lockverdictPath = "github.com/pkalin/shadewright/internal/controller/lockverdict"

// importedAs returns the names under which f imports path: its own name or the one the import gives ("." for a
// dot import). A package of another path that happens to have the same name is not in it.
func importedAs(f *ast.File, path string) map[string]bool {
	names := map[string]bool{}
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			names[imp.Name.Name] = true
		} else {
			names[pathpkg.Base(path)] = true
		}
	}
	return names
}

// lockverdictFuncs lists the package-level functions of package lockverdict, read from its source, so a type or a
// constant of the package (Verdict, Found) is not taken for a constructor.
func lockverdictFuncs(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("lockverdict", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.IsExported() {
				funcs[fn.Name.Name] = true
			}
		}
	}
	if len(funcs) == 0 || !funcs[releaseFunc] {
		t.Fatalf("no functions read from package lockverdict: %v", funcs)
	}
	return funcs
}

// verdictBuilds lists the references in f to a function of package lockverdict other than Release, called or taken
// as a value, outside settlementBuilders. The package is resolved by its import path, whatever name the file gives
// it, and an identifier that a local declaration binds (a shadowing variable) is not the package. A dot import is
// refused whole: its functions could not be told from this package's.
func verdictBuilds(fset *token.FileSet, f *ast.File, funcs map[string]bool) []string {
	names := importedAs(f, lockverdictPath)
	var found []string
	if names["."] {
		found = append(found, fset.Position(f.Pos()).String()+": imports package lockverdict with a dot")
	}
	forEachDecl(f, func(owner string, n ast.Node) {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || !names[id.Name] || id.Obj != nil || !funcs[sel.Sel.Name] || sel.Sel.Name == releaseFunc ||
			settlementBuilders[owner] {
			return
		}
		found = append(found, fmt.Sprintf("%s: %q refers to lockverdict.%s", fset.Position(n.Pos()), owner,
			sel.Sel.Name))
	})
	return found
}

// TestLockSettlementBuiltByItsProofs fails when a function outside settlementBuilders calls a constructor of
// package lockverdict, or takes one as a value: a release of the target lock takes a verdict only the proving
// reads build (decisions of 2026-11-17 and 2026-11-18 on #771). The type itself is sealed in its package, so no
// other code can build one.
func TestLockSettlementBuiltByItsProofs(t *testing.T) {
	funcs := lockverdictFuncs(t)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, what := range verdictBuilds(fset, f, funcs) {
			t.Errorf("%s; only its proving reads build a lock verdict", what)
		}
	}
}

// TestVerdictGuardResolvesThePackage checks the guard above on code it must refuse (a call and a function value
// outside the builders, under the package's own name and under another, and a dot import) and on code it must
// pass (a builder, a type and a constant of the package, another package of the same name, and a local variable
// that shadows the name).
func TestVerdictGuardResolvesThePackage(t *testing.T) {
	funcs := map[string]bool{"ByRead": true, releaseFunc: true}
	cases := []struct {
		name, src string
		refused   bool
	}{
		{"call", `import "` + lockverdictPath + `"
func early() { _, _ = lockverdict.ByRead(f, true, true, false) }`, true},
		{"function value", `import "` + lockverdictPath + `"
var build = lockverdict.ByRead`, true},
		{"value in a function", `import "` + lockverdictPath + `"
func later() { f := lockverdict.ByRead; _ = f }`, true},
		{"renamed import", `import lv "` + lockverdictPath + `"
func early() { _, _ = lv.ByRead(f, true, true, false) }`, true},
		{"dot import", `import . "` + lockverdictPath + `"
func early() {}`, true},
		{"builder", `import "` + lockverdictPath + `"
func settleByRead() { _, _ = lockverdict.ByRead(f, true, true, false) }`, false},
		{"type and constant", `import "` + lockverdictPath + `"
func uses(v *lockverdict.Verdict) { _ = lockverdict.FoundOpen }`, false},
		{"the release itself", `import "` + lockverdictPath + `"
func release() { _ = lockverdict.Release(s, v) }`, false},
		{"another package of the same name", `import "example.com/other/lockverdict"
func early() { _, _ = lockverdict.ByRead(f, true, true, false) }`, false},
		{"shadowing variable", `import "` + lockverdictPath + `"
func early(lockverdict builder) { _, _ = lockverdict.ByRead(f, true, true, false) }`, false},
	}
	for _, c := range cases {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, "x.go", "package controller\n"+c.src+"\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(verdictBuilds(fset, f, funcs)) > 0; got != c.refused {
			t.Errorf("%s: refused = %v, want %v", c.name, got, c.refused)
		}
	}
}

// funcName is the name of fn, with the receiver's type for a method of gatedGit.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok && id.Name == "gatedGit" {
		return "gatedGit." + fn.Name.Name
	}
	return fn.Name.Name
}

// TestEveryRuleCoversEveryVerdict checks that each caller's rule table has an entry for every verdict, and that
// unknown never ends, releases or removes anything: it keeps the lock, retries the adoption, the finalizer's close
// and the recovery.
func TestEveryRuleCoversEveryVerdict(t *testing.T) {
	check := func(name string, n int, unknownOK bool) {
		t.Helper()
		if n != len(changeStates) {
			t.Errorf("%s has %d of %d verdicts", name, n, len(changeStates))
		}
		if !unknownOK {
			t.Errorf("%s does not retry or keep on unknown", name)
		}
	}
	for _, state := range []changeState{changeUnknown, changeOpen} {
		if _, ok := settleByRead(&api.RemediationRequest{}, changeVerdict{state: state}, nil); ok {
			t.Errorf("settleByRead gives a verdict for %v", state)
		}
	}
	check("adoptRule", len(adoptRule), adoptRule[changeUnknown] == adoptRetry)
	check("finalizeRule", len(finalizeRule), finalizeRule[changeUnknown] == finalizeRetry)
	check("recoverRule", len(recoverRule), recoverRule[changeUnknown] == recoverRetry)
	// The zero value of each outcome is the safe one, so a verdict missing from a table keeps and retries.
	if adoptRetry != 0 || finalizeRetry != 0 || recoverRetry != 0 {
		t.Error("the zero outcome of a rule must keep and retry")
	}
}
