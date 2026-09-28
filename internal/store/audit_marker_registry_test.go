package store

// 這支測試補的是「唯一的 canonical operator action allowlist 仍可能漏收一個會寫
// replay／transport rejection marker 的 action」這個缺口。它用 AST 找出 Detail
// marker，並靜態歸因四種 site：同一函式的 Action 常數、跨一跳呼叫時由呼叫端
// AuditEntry composite literal 提供的 Action、沿「會組 marker 的函式」遞移閉包
// 所找到之 action 引數位常數，以及呼叫端先寫入識別字的 AuditEntry.Action 常數
// 再把該識別字當引數傳入。四種都只認常數字面值；閉包只往呼叫端走，callee
// 不因為被呼叫而進集合，單純提到 prefix 也不算寫入 marker。因此本測試並不
// 完備；它保證的是「凡是歸因得到的 action，一個都不准漏出 allowlist」。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 目前實測可解析到 131 個 sites；保留少量餘裕，同時防止掃描規則大幅退化。
const minimumAuditMarkerSites = 126

type auditMarkerSite struct {
	file     string
	line     int
	position token.Pos
	actions  map[AuditAction]bool
}

func TestAuditMarkerActionsAreCanonical(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()
	declared := markerAuditActionDeclarations(t, fileSet)
	sites := markerAuditSites(t, fileSet, declared)
	if len(sites) < minimumAuditMarkerSites {
		t.Errorf("replay／transport rejection marker sites 數量 got=%d, expected>=%d；少於下限，AST 掃描器可能已失效；下面的 foundXxx 訊息會指出是哪一種歸因規則失效", len(sites), minimumAuditMarkerSites)
	}

	foundEnrollmentLimit := false
	foundRetentionPrune := false
	foundCatalogManifest := false
	foundDeviceSync := false
	for _, site := range sites {
		for action := range site.actions {
			if site.file == "../operator/enrollment_limit.go" && action == AuditEnrollmentLimit {
				foundEnrollmentLimit = true
			}
			if site.file == "../../cmd/clawctl-hub/operator_retention_api.go" && action == AuditRetentionPrune {
				foundRetentionPrune = true
			}
			if site.file == "../../cmd/clawctl-hub/operator_catalog_api.go" && action == AuditCatalogManifest {
				foundCatalogManifest = true
			}
			if site.file == "../store/operator_device_sync.go" && action == AuditDeviceSync {
				foundDeviceSync = true
			}
			if !IsCanonicalOperatorAction(action) {
				t.Errorf("%s:%d 會寫回放／transport rejection marker，但 %s 不在 allowlist；稽核頁會把這一列講成『做了』", site.file, site.line, action)
			}
		}
	}
	if !foundEnrollmentLimit {
		t.Error("../operator/enrollment_limit.go 的 enrollment-limit marker site 歸因結果 got=false, expected=true；AST 掃描器可能已失效，漏收的 action 會逃過 allowlist 檢查")
	}
	if !foundRetentionPrune {
		t.Error("../../cmd/clawctl-hub/operator_retention_api.go 的 retention-prune marker site 歸因結果 got=false, expected=true；跨一跳 AST 掃描規則可能已失效，呼叫端的 action 會逃過 allowlist 檢查")
	}
	if !foundCatalogManifest {
		t.Error("../../cmd/clawctl-hub/operator_catalog_api.go 的 catalog-manifest marker site 歸因結果 got=false, expected=true；引數位 AST 歸因規則可能已失效，經 helper 寫入的 action 會逃過 allowlist 檢查")
	}
	if !foundDeviceSync {
		t.Error("../store/operator_device_sync.go 的 device-sync marker site 歸因結果 got=false, expected=true；識別字引數 AST 歸因規則可能已失效，先寫入變數的 action 會逃過 allowlist 檢查")
	}
}

func markerAuditActionDeclarations(t *testing.T, fileSet *token.FileSet) map[string]AuditAction {
	t.Helper()
	parsed, err := parser.ParseFile(fileSet, "audit.go", nil, 0)
	if err != nil {
		t.Fatalf("parse audit.go: %v", err)
	}
	actions := make(map[string]AuditAction)
	for _, declaration := range parsed.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, specification := range general.Specs {
			value, ok := specification.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "AuditAction" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					t.Fatalf("AuditAction 常數 %s 不是字串字面值", name.Name)
				}
				unquoted, err := strconv.Unquote(literal.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", literal.Value, err)
				}
				actions[name.Name] = AuditAction(unquoted)
			}
		}
	}
	if len(actions) == 0 {
		t.Fatal("audit.go 裡一個 AuditAction 宣告都沒解析到")
	}
	return actions
}

func markerAuditSites(t *testing.T, fileSet *token.FileSet, declared map[string]AuditAction) []auditMarkerSite {
	t.Helper()
	type parsedAuditFile struct {
		path string
		file *ast.File
	}
	var parsedFiles []parsedAuditFile
	for _, root := range []string{"../", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			parsed, err := parser.ParseFile(fileSet, path, nil, 0)
			if err != nil {
				return err
			}
			parsedFiles = append(parsedFiles, parsedAuditFile{path: path, file: parsed})
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", root, err)
		}
	}

	functions := make(map[string]*ast.FuncDecl)
	for _, parsed := range parsedFiles {
		for _, declaration := range parsed.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			actionParameters := auditActionParameterIndexes(function)
			if len(actionParameters) > 1 {
				position := fileSet.Position(function.Pos())
				t.Fatalf("函式 %s (%s:%d) 的 AuditAction 參數數量 got=%d, expected<=1；掃描器無法判斷哪一個參數會流進 AuditEntry.Action，猜錯會歸因到不會被寫入的常數，變成一支說謊的閘", function.Name.Name, parsed.path, position.Line, len(actionParameters))
			}
			functions[function.Name.Name] = function
		}
	}

	markerFunctions := make(map[string]bool)
	compositeMarkerFunctions := make(map[string]bool)
	for name, function := range functions {
		if hasAuditParameter(function) && functionBuildsAuditMarker(function) {
			markerFunctions[name] = true
		}
		if hasAuditEntryParameter(function) && functionBuildsAuditMarker(function) {
			compositeMarkerFunctions[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, function := range functions {
			if markerFunctions[name] || !hasAuditParameter(function) {
				continue
			}
			if functionCallsAny(function, markerFunctions) {
				markerFunctions[name] = true
				changed = true
			}
		}
	}

	actionParameterIndexes := make(map[string]int)
	for changed := true; changed; {
		changed = false
		for name := range markerFunctions {
			if _, known := actionParameterIndexes[name]; known {
				continue
			}
			if index, ok := actionParameterIndex(functions[name], actionParameterIndexes); ok {
				actionParameterIndexes[name] = index
				changed = true
			}
		}
	}

	var sites []auditMarkerSite
	seen := make(map[string]bool)
	addSite := func(site auditMarkerSite) {
		key := site.file + ":" + strconv.Itoa(int(site.position))
		if !seen[key] {
			seen[key] = true
			sites = append(sites, site)
		}
	}
	for _, parsed := range parsedFiles {
		for _, declaration := range parsed.file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok {
				continue
			}
			for _, site := range markerSitesInFunction(fileSet, parsed.path, function, declared) {
				addSite(site)
			}
			actionAssignments := markerActionAssignmentsInFunction(function, declared)
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || !markerFunctions[calledFunctionName(call.Fun)] {
					return true
				}
				if compositeMarkerFunctions[calledFunctionName(call.Fun)] {
					for _, argument := range call.Args {
						switch value := argument.(type) {
						case *ast.CompositeLit:
							if !isAuditEntryType(value.Type) {
								continue
							}
							addSite(auditMarkerSite{
								file:     parsed.path,
								line:     fileSet.Position(value.Pos()).Line,
								position: value.Pos(),
								actions:  markerActionsIn(value, declared),
							})
						case *ast.Ident:
							// 同一函式內若同一變數被寫入多個 action 常數，會全部歸因；
							// 這是刻意保留、與既有同函式規則一致的 over-approximation。
							if actions := actionAssignments[markerExpressionKey(value)]; len(actions) != 0 {
								addSite(auditMarkerSite{
									file:     parsed.path,
									line:     fileSet.Position(value.Pos()).Line,
									position: value.Pos(),
									actions:  cloneMarkerActions(actions),
								})
							}
						}
					}
				}
				if index, known := actionParameterIndexes[calledFunctionName(call.Fun)]; known && index < len(call.Args) {
					if actions := literalMarkerActions(call.Args[index], declared); len(actions) != 0 {
						addSite(auditMarkerSite{
							file:     parsed.path,
							line:     fileSet.Position(call.Args[index].Pos()).Line,
							position: call.Args[index].Pos(),
							actions:  actions,
						})
					}
				}
				return true
			})
		}
	}
	return sites
}

func hasAuditParameter(function *ast.FuncDecl) bool {
	if function.Type.Params == nil {
		return false
	}
	for _, field := range function.Type.Params.List {
		if isAuditEntryType(field.Type) || isAuditActionType(field.Type) {
			return true
		}
	}
	return false
}

func hasAuditEntryParameter(function *ast.FuncDecl) bool {
	if function.Type.Params == nil {
		return false
	}
	for _, field := range function.Type.Params.List {
		if isAuditEntryType(field.Type) {
			return true
		}
	}
	return false
}

func isAuditActionType(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name == "AuditAction"
	case *ast.SelectorExpr:
		qualifier, ok := value.X.(*ast.Ident)
		return ok && qualifier.Name == "store" && value.Sel.Name == "AuditAction"
	default:
		return false
	}
}

func auditActionParameterIndexes(function *ast.FuncDecl) []int {
	var indexes []int
	index := 0
	if function.Type.Params == nil {
		return indexes
	}
	for _, field := range function.Type.Params.List {
		count := len(field.Names)
		if count == 0 {
			count = 1
		}
		if isAuditActionType(field.Type) {
			for offset := 0; offset < count; offset++ {
				indexes = append(indexes, index+offset)
			}
		}
		index += count
	}
	return indexes
}

func functionCallsAny(function *ast.FuncDecl, callees map[string]bool) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if ok && callees[calledFunctionName(call.Fun)] {
			found = true
		}
		return !found
	})
	return found
}

func actionParameterIndex(function *ast.FuncDecl, known map[string]int) (int, bool) {
	parameters := functionParameterIndexes(function)
	index := -1
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch expression := node.(type) {
		case *ast.AssignStmt:
			for i, left := range expression.Lhs {
				selector, ok := left.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "Action" {
					index = parameterIndex(markerAssignmentRHS(expression, i), parameters, index)
				}
			}
		case *ast.CompositeLit:
			for _, element := range expression.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				name, named := pairKeyName(pair)
				if ok && named && name == "Action" {
					index = parameterIndex(pair.Value, parameters, index)
				}
			}
		case *ast.CallExpr:
			calleeIndex, ok := known[calledFunctionName(expression.Fun)]
			if ok && calleeIndex < len(expression.Args) {
				index = parameterIndex(expression.Args[calleeIndex], parameters, index)
			}
		}
		return true
	})
	return index, index >= 0
}

func functionParameterIndexes(function *ast.FuncDecl) map[string]int {
	parameters := make(map[string]int)
	index := 0
	if function.Type.Params == nil {
		return parameters
	}
	for _, field := range function.Type.Params.List {
		if len(field.Names) == 0 {
			index++
			continue
		}
		for _, name := range field.Names {
			parameters[name.Name] = index
			index++
		}
	}
	return parameters
}

func parameterIndex(expression ast.Expr, parameters map[string]int, current int) int {
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return current
	}
	if index, ok := parameters[identifier.Name]; ok {
		return index
	}
	return current
}

func literalMarkerActions(expression ast.Expr, declared map[string]AuditAction) map[AuditAction]bool {
	switch value := expression.(type) {
	case *ast.Ident:
		if _, ok := declared[value.Name]; !ok {
			return nil
		}
	case *ast.SelectorExpr:
		if _, ok := value.X.(*ast.Ident); !ok {
			return nil
		}
		if _, ok := declared[value.Sel.Name]; !ok {
			return nil
		}
	default:
		return nil
	}
	return markerActionsIn(expression, declared)
}

func isAuditEntryType(expression ast.Expr) bool {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name == "AuditEntry"
	case *ast.SelectorExpr:
		qualifier, ok := value.X.(*ast.Ident)
		return ok && qualifier.Name == "store" && value.Sel.Name == "AuditEntry"
	default:
		return false
	}
}

func functionBuildsAuditMarker(function *ast.FuncDecl) bool {
	found := false
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch expression := node.(type) {
		case *ast.AssignStmt:
			for i, left := range expression.Lhs {
				selector, ok := left.(*ast.SelectorExpr)
				if ok && selector.Sel.Name == "Detail" && isAuditMarkerExpression(markerAssignmentRHS(expression, i)) {
					found = true
				}
			}
		case *ast.CompositeLit:
			for _, element := range expression.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				name, named := pairKeyName(pair)
				if ok && named && name == "Detail" && isAuditMarkerExpression(pair.Value) {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

func pairKeyName(pair *ast.KeyValueExpr) (string, bool) {
	if pair == nil {
		return "", false
	}
	name, ok := pair.Key.(*ast.Ident)
	if !ok {
		return "", false
	}
	return name.Name, true
}

func calledFunctionName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return value.Sel.Name
	default:
		return ""
	}
}

func markerSitesInFunction(fileSet *token.FileSet, path string, function *ast.FuncDecl, declared map[string]AuditAction) []auditMarkerSite {
	actionAssignments := markerActionAssignmentsInFunction(function, declared)
	var sites []auditMarkerSite
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch expression := node.(type) {
		case *ast.CompositeLit:
			actions := make(map[AuditAction]bool)
			var details []ast.Expr
			for _, element := range expression.Elts {
				pair, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				name, ok := pair.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if name.Name == "Action" {
					for action := range markerActionsIn(pair.Value, declared) {
						actions[action] = true
					}
				}
				if name.Name == "Detail" && isAuditMarkerExpression(pair.Value) {
					details = append(details, pair.Value)
				}
			}
			for _, detail := range details {
				sites = append(sites, auditMarkerSite{
					file:     path,
					line:     fileSet.Position(detail.Pos()).Line,
					position: detail.Pos(),
					actions:  cloneMarkerActions(actions),
				})
			}
		}
		return true
	})

	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, left := range assignment.Lhs {
			selector, ok := left.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Detail" || !isAuditMarkerExpression(markerAssignmentRHS(assignment, i)) {
				continue
			}
			sites = append(sites, auditMarkerSite{
				file:     path,
				line:     fileSet.Position(left.Pos()).Line,
				position: left.Pos(),
				actions:  cloneMarkerActions(actionAssignments[markerExpressionKey(selector.X)]),
			})
		}
		return true
	})
	return sites
}

func markerActionAssignmentsInFunction(function *ast.FuncDecl, declared map[string]AuditAction) map[string]map[AuditAction]bool {
	actionAssignments := make(map[string]map[AuditAction]bool)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, left := range assignment.Lhs {
			selector, ok := left.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "Action" {
				continue
			}
			key := markerExpressionKey(selector.X)
			if key == "" {
				continue
			}
			for action := range literalMarkerActions(markerAssignmentRHS(assignment, i), declared) {
				if actionAssignments[key] == nil {
					actionAssignments[key] = make(map[AuditAction]bool)
				}
				actionAssignments[key][action] = true
			}
		}
		return true
	})
	return actionAssignments
}

func markerAssignmentRHS(assignment *ast.AssignStmt, index int) ast.Expr {
	if len(assignment.Rhs) == len(assignment.Lhs) && index < len(assignment.Rhs) {
		return assignment.Rhs[index]
	}
	if len(assignment.Rhs) == 1 && len(assignment.Lhs) == 1 {
		return assignment.Rhs[0]
	}
	return nil
}

func isAuditMarkerExpression(expression ast.Expr) bool {
	found := false
	ast.Inspect(expression, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			found = found || value.Name == "OperatorIdempotencyReplayPrefix" || value.Name == "OperatorTransportRejectionPrefix"
		case *ast.SelectorExpr:
			found = found || value.Sel.Name == "OperatorIdempotencyReplayPrefix" || value.Sel.Name == "OperatorTransportRejectionPrefix"
		}
		return !found
	})
	return found
}

func markerActionsIn(expression ast.Expr, declared map[string]AuditAction) map[AuditAction]bool {
	actions := make(map[AuditAction]bool)
	ast.Inspect(expression, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.Ident:
			if action, ok := declared[value.Name]; ok {
				actions[action] = true
			}
		case *ast.SelectorExpr:
			if action, ok := declared[value.Sel.Name]; ok {
				actions[action] = true
			}
		}
		return true
	})
	return actions
}

func markerExpressionKey(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		return markerExpressionKey(value.X) + "." + value.Sel.Name
	case *ast.IndexExpr:
		return markerExpressionKey(value.X) + "[]"
	default:
		return ""
	}
}

func cloneMarkerActions(source map[AuditAction]bool) map[AuditAction]bool {
	clone := make(map[AuditAction]bool, len(source))
	for action := range source {
		clone[action] = true
	}
	return clone
}
