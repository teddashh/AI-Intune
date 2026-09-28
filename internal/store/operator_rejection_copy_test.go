package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

type operatorRejectionSentence struct {
	family   string
	code     string
	sentence string
}

// RejectionDetail 表回傳的句子，就是 operator 在 Web 與 JSON 實際讀到的
// 拒絕理由；historical* 與 stored* 僅供重放使用，不在此列。家族清單與
// code 均由 AST 即時掃描而非手抄；新增家族卻未登錄，或 case 改用裸字串，
// 都會使該句悄悄無人守護，因此這兩件事本身也是斷言。這裡釘的是形狀而非
// 逐字內容：句子可以改寫，但不能空、不能讓兩個不同的 code 使用同一句、
// 該說「重新預覽」時不能不說、不能把「請重新讀取」用在非 precondition
// 的拒絕，也不能使用重放專屬的「原 request」。
func TestOperatorRejectionCopyKeepsItsShape(t *testing.T) {
	registered := map[string]func(string) (string, bool){
		"canonicalOperatorArtifactFetchRejectionDetail":          canonicalOperatorArtifactFetchRejectionDetail,
		"canonicalOperatorCatalogManifestRejectionDetail":        canonicalOperatorCatalogManifestRejectionDetail,
		"canonicalOperatorDeploymentRejectionDetail":             canonicalOperatorDeploymentRejectionDetail,
		"canonicalOperatorDeviceSyncRejectionDetail":             canonicalOperatorDeviceSyncRejectionDetail,
		"canonicalOperatorDiagnosticNoopRejectionDetail":         canonicalOperatorDiagnosticNoopRejectionDetail,
		"canonicalOperatorEnrollTokenRejectionDetail":            canonicalOperatorEnrollTokenRejectionDetail,
		"canonicalOperatorEnrollTokenRevocationRejectionDetail":  canonicalOperatorEnrollTokenRevocationRejectionDetail,
		"canonicalOperatorMachineLifecycleRejectionDetail":       canonicalOperatorMachineLifecycleRejectionDetail,
		"canonicalOperatorMachineProfileRejectionDetail":         canonicalOperatorMachineProfileRejectionDetail,
		"canonicalOperatorProfileAssignmentRejectionDetail":      canonicalOperatorProfileAssignmentRejectionDetail,
		"canonicalOperatorRetentionRejectionDetail":              canonicalOperatorRetentionRejectionDetail,
		"canonicalOperatorVerificationAssignmentRejectionDetail": canonicalOperatorVerificationAssignmentRejectionDetail,
		"canonicalOperatorVerifierRejectionDetail":               canonicalOperatorVerifierRejectionDetail,
		"canonicalOperatorVerifierRevocationRejectionDetail":     canonicalOperatorVerifierRevocationRejectionDetail,
		"restoreDrillRejectionDetail":                            restoreDrillRejectionDetail,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	codeByName := make(map[string]string)
	found := make(map[string]bool)
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if ok && gen.Tok == token.CONST {
				for _, spec := range gen.Specs {
					values := spec.(*ast.ValueSpec)
					for i, ident := range values.Names {
						if !strings.HasPrefix(ident.Name, "OperatorCode") {
							continue
						}
						if i >= len(values.Values) {
							t.Fatalf("constant=%q: expected an explicit string value; the code universe cannot be enumerated without it",
								ident.Name)
						}
						literal, ok := values.Values[i].(*ast.BasicLit)
						if !ok || literal.Kind != token.STRING {
							t.Fatalf("constant=%q: expected a string literal; the code universe cannot be enumerated without it",
								ident.Name)
						}
						value, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						codeByName[ident.Name] = value
					}
				}
			}

			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !operatorRejectionDetailSignature(fn) ||
				strings.HasPrefix(fn.Name.Name, "historical") ||
				strings.HasPrefix(fn.Name.Name, "stored") {
				continue
			}
			found[fn.Name.Name] = true
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch node := node.(type) {
				case *ast.CaseClause:
					for _, expression := range node.List {
						operatorRejectionCodeIdentifier(t, fn.Name.Name, expression)
					}
				case *ast.CompositeLit:
					if _, ok := node.Type.(*ast.MapType); !ok {
						return true
					}
					for _, element := range node.Elts {
						if pair, ok := element.(*ast.KeyValueExpr); ok {
							operatorRejectionCodeIdentifier(t, fn.Name.Name, pair.Key)
						}
					}
				}
				return true
			})
		}
	}

	missing, extra := operatorRejectionRegistrationDifference(found, registered)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("missing=%q extra=%q: expected AST tables and registered tables to match exactly; an unregistered rejection family leaves every sentence in that family completely unguarded and freely changeable while tests remain green",
			missing, extra)
	}
	if len(registered) != 15 {
		t.Errorf("expected exactly 15 registered tables, got %d; losing the fixed table count can silently remove an entire operator-facing copy contract",
			len(registered))
	}

	codes := make(map[string]bool)
	for _, code := range codeByName {
		codes[code] = true
	}
	codeValues := make([]string, 0, len(codes))
	for code := range codes {
		codeValues = append(codeValues, code)
	}
	sort.Strings(codeValues)

	var sentences []operatorRejectionSentence
	families := make([]string, 0, len(registered))
	for family := range registered {
		families = append(families, family)
	}
	sort.Strings(families)
	for _, family := range families {
		count := 0
		for _, code := range codeValues {
			sentence, ok := registered[family](code)
			if !ok {
				continue
			}
			count++
			sentences = append(sentences, operatorRejectionSentence{family: family, code: code, sentence: sentence})
		}
		if count == 0 {
			t.Fatalf("family=%q: expected at least one discovered sentence; zero results mean the exhaustive probe is broken, not that the table is empty",
				family)
		}
	}
	sort.Slice(sentences, func(i, j int) bool {
		if sentences[i].family != sentences[j].family {
			return sentences[i].family < sentences[j].family
		}
		if sentences[i].code != sentences[j].code {
			return sentences[i].code < sentences[j].code
		}
		return sentences[i].sentence < sentences[j].sentence
	})

	// 每一句都必須有內容，且邊界不得有空白。
	for _, item := range sentences {
		if item.sentence == "" || item.sentence != strings.TrimSpace(item.sentence) {
			t.Errorf("R1 family=%q code=%q sentence=%q: expected a non-empty sentence with no boundary whitespace; otherwise the operator receives an empty rejection reason and cannot tell what blocked the request",
				item.family, item.code, item.sentence)
		}
	}

	// 不同 code 不得共用同一句；相同 code 跨家族共用則允許。
	firstBySentence := make(map[string]operatorRejectionSentence)
	for _, item := range sentences {
		first, exists := firstBySentence[item.sentence]
		if exists && first.code != item.code {
			t.Errorf("R2 family=%q code=%q sentence=%q: expected different codes to have different sentences; it duplicates family=%q code=%q, so the operator cannot distinguish which rule fired or know what to change",
				item.family, item.code, item.sentence, first.family, first.code)
			continue
		}
		if !exists {
			firstBySentence[item.sentence] = item
		}
	}

	// 預覽過期或缺少預覽時，句子必須指示重新預覽。
	for _, item := range sentences {
		if (strings.HasSuffix(item.code, "PREVIEW_STALE") ||
			strings.HasSuffix(item.code, "PREVIEW_REQUIRED")) &&
			!strings.Contains(item.sentence, "重新預覽") {
			t.Errorf("R3 family=%q code=%q sentence=%q: expected PREVIEW_STALE and PREVIEW_REQUIRED copy to contain %q; re-previewing is the only correct next action, and without that instruction the operator only knows the request was rejected",
				item.family, item.code, item.sentence, "重新預覽")
		}
	}

	// 指示重新讀取的句子只能用於 precondition 類 code。
	for _, item := range sentences {
		if strings.Contains(item.sentence, "請重新讀取") &&
			!strings.Contains(item.code, "PRECONDITION") {
			t.Errorf("R4 family=%q code=%q sentence=%q: expected copy containing %q to use a PRECONDITION code; otherwise the operator is told to refresh a stale revision when doing so cannot change the decision",
				item.family, item.code, item.sentence, "請重新讀取")
		}
	}

	// 首次拒絕文案不得使用重放專屬的「原 request」時間座標。
	for _, item := range sentences {
		if strings.Contains(item.sentence, "原 request") {
			t.Errorf("R5 family=%q code=%q sentence=%q: expected first-rejection copy not to contain %q; that replay-only time reference sends the operator looking for a prior request that was never submitted",
				item.family, item.code, item.sentence, "原 request")
		}
	}
}

func operatorRejectionDetailSignature(fn *ast.FuncDecl) bool {
	if !strings.HasSuffix(fn.Name.Name, "RejectionDetail") || fn.Recv != nil ||
		fn.Type.Params == nil || len(fn.Type.Params.List) != 1 ||
		fn.Type.Results == nil || len(fn.Type.Results.List) != 2 {
		return false
	}
	parameter, parameterOK := fn.Type.Params.List[0].Type.(*ast.Ident)
	firstResult, firstOK := fn.Type.Results.List[0].Type.(*ast.Ident)
	secondResult, secondOK := fn.Type.Results.List[1].Type.(*ast.Ident)
	return parameterOK && parameter.Name == "string" &&
		len(fn.Type.Params.List[0].Names) == 1 &&
		firstOK && firstResult.Name == "string" &&
		secondOK && secondResult.Name == "bool"
}

func operatorRejectionCodeIdentifier(t *testing.T, family string, expression ast.Expr) {
	t.Helper()
	ident, ok := expression.(*ast.Ident)
	if ok && strings.HasPrefix(ident.Name, "OperatorCode") {
		return
	}
	t.Errorf("family=%q expression=%q: expected every case expression or map key to be an OperatorCode identifier; a raw string makes exhaustive probing miss that code and leaves one sentence in this table unguarded",
		family, operatorRejectionExpression(expression))
}

func operatorRejectionExpression(expression ast.Expr) string {
	switch expression := expression.(type) {
	case *ast.BasicLit:
		return expression.Value
	case *ast.Ident:
		return expression.Name
	default:
		return "<non-identifier expression>"
	}
}

func operatorRejectionRegistrationDifference(
	found map[string]bool,
	registered map[string]func(string) (string, bool),
) ([]string, []string) {
	var missing, extra []string
	for name := range found {
		if _, ok := registered[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range registered {
		if !found[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return missing, extra
}
