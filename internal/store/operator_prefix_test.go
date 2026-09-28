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

// TestEveryPersistedOperatorPrefixIsUnambiguous 檢查兩組持久化名稱。
// audit_log.detail 分類標記由讀取端用 HasPrefix 辨識；相同值或互為前綴時，
// 較長的標記也會符合較短的標記。idempotency operation 命名空間寫進
// operator_idempotency.operation，生產端以等號辨識；但命名空間常數會接上 id，
// 所以較短值接上適當的 id 仍能組出較長值，兩者也不得相同或互為前綴。
// 兩組位於不同欄位，值相同不會誤判，因此不做跨組檢查，以免鎖死合法命名。
// 掃描依賴名稱以 Prefix、Pfx 或 Operation 結尾的慣例；
// operatorVerifierRevokeOperationPfx 正是原本只掃 Prefix 時漏掉的常數。
// 每組 12 個的下限只防止 AST 掃描空轉，不是清點，新增常數不該讓測試失敗。
// 這支仍守不到在寫入點當場串出的 operation 字串，例如
// operator_deployment.go:260 的 "deployment-" + string(action) + ":v1:" +
// deploymentID、operator.go:340 的 "machine-channel:" + req.MachineID，以及
// operator_machine_rename.go:113、operator_machine_notes.go:103、
// setting_policy.go:100 與 :262、compliance_policy.go:93 與 :259、
// tailnet_operator.go:125。那些要先抽成常數才守得到，屬於 production 決定。
func TestEveryPersistedOperatorPrefixIsUnambiguous(t *testing.T) {
	const (
		auditDetailGroup          = "audit_log.detail classification markers"
		idempotencyOperationGroup = "idempotency operation namespaces"
	)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	valuesByGroup := map[string]map[string]string{
		auditDetailGroup:          {},
		idempotencyOperationGroup: {},
	}
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
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values := spec.(*ast.ValueSpec)
				for i, ident := range values.Names {
					isCandidate := strings.HasSuffix(ident.Name, "Prefix") ||
						strings.HasSuffix(ident.Name, "Pfx") ||
						strings.HasSuffix(ident.Name, "Operation")
					if !isCandidate || i >= len(values.Values) {
						continue
					}
					literal, ok := values.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					group := auditDetailGroup
					if strings.Contains(ident.Name, "Operation") {
						group = idempotencyOperationGroup
					}
					valuesByGroup[group][ident.Name] = value
				}
			}
		}
	}

	for group, valuesByName := range valuesByGroup {
		if len(valuesByName) < 12 {
			t.Fatalf("group %q discovered %d constants, expected at least 12; this lower bound prevents the AST scan from silently doing no work",
				group, len(valuesByName))
		}

		names := make([]string, 0, len(valuesByName))
		for name := range valuesByName {
			names = append(names, name)
		}
		sort.Strings(names)

		for _, name := range names {
			value := valuesByName[name]
			if value == "" {
				if group == auditDetailGroup {
					t.Errorf("group %q constant %q has an empty value; an empty audit marker would make every detail match a HasPrefix reader", group, name)
				} else {
					t.Errorf("group %q constant %q has an empty value; an empty namespace would become any operation once an id is appended", group, name)
				}
			}
		}

		for i, firstName := range names {
			firstValue := valuesByName[firstName]
			for _, secondName := range names[i+1:] {
				secondValue := valuesByName[secondName]
				if firstValue == secondValue {
					if group == auditDetailGroup {
						t.Errorf("group %q constants %q and %q have values %q and %q; HasPrefix readers would classify both audit details as the same marker",
							group, firstName, secondName, firstValue, secondValue)
					} else {
						t.Errorf("group %q constants %q and %q have values %q and %q; namespaces receive an appended id, so equal bases can produce the same operation",
							group, firstName, secondName, firstValue, secondValue)
					}
					continue
				}
				if strings.HasPrefix(firstValue, secondValue) ||
					strings.HasPrefix(secondValue, firstValue) {
					if group == auditDetailGroup {
						t.Errorf("group %q constants %q and %q have values %q and %q; HasPrefix readers would make the longer audit detail also match the shorter marker",
							group, firstName, secondName, firstValue, secondValue)
					} else {
						t.Errorf("group %q constants %q and %q have values %q and %q; namespaces receive an appended id, so the shorter value can be extended to produce the longer operation",
							group, firstName, secondName, firstValue, secondValue)
					}
				}
			}
		}
	}
}
