package state

// 這支測試用 AST 掃描 production 原始碼中型別為 Judgement 的 composite
// literal，只認 State 欄位直接寫出的常數識別字，並把它們和 AllStates 宣告
// 裡直接列出的識別字比對；它不追蹤變數、呼叫或 selector，也不做呼叫鏈分析，
// 因此並不完備。它保證的是「凡是原始碼裡直接寫出來的 Judgement state，
// 一個都不准不在 AllStates 裡」。兩側另設數量下限，避免掃描規則失效後
// 因為集合意外變空而安靜地全綠。
//
// ⚠ 隔離盤（./...、-count=1）量到：新增 Quarantined，並在 Derive 開頭
// 插入執行期完全無效果的 if false 回傳，但不加入 AllStates 時，本測試紅，
// 全樹其他測試零紅（others=[]）。把 IdentityConflict 從 AllStates 移除時，
// 本測試紅，另有 24 支紅；移除方向本來就重兵把守，本測試並非唯一看守者。
// 對照組把 Quarantined 同時加入 AllStates 時，本測試綠；另有 23 支因為
// 「剛好五格」的 envelope 斷言而紅，那是別的事。
//
// ⚠ 第一個突變執行期毫無效果，任何行為測試都看不見，只有讀原始碼的測試
// 看得見。state.go 的 AllStates 上方本來就寫著「⚠⚠ 加新狀態一定要同時
// 加到這裡」；這支測試就是在執行那句話。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type judgementStateSite struct {
	name     string
	position token.Position
}

func TestDeriveReturnsOnlyStatesInAllStates(t *testing.T) {
	t.Parallel()

	fileSet := token.NewFileSet()
	files := productionStateFiles(t)
	judgementStates := make(map[string][]token.Position)
	allStates := make(map[string]bool)
	allStateElements := 0
	allStatesFound := false

	for _, path := range files {
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}

		ast.Inspect(parsed, func(node ast.Node) bool {
			literal, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typeName, ok := literal.Type.(*ast.Ident)
			if !ok || typeName.Name != "Judgement" {
				return true
			}
			for _, element := range literal.Elts {
				field, ok := element.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := field.Key.(*ast.Ident)
				if !ok || key.Name != "State" {
					continue
				}
				value, ok := field.Value.(*ast.Ident)
				if !ok {
					position := fileSet.Position(field.Value.Pos())
					t.Fatalf("%s:%d 的 Judgement.State 不是單純識別字；AST 掃描器已經看不懂這份原始碼了", position.Filename, position.Line)
				}
				judgementStates[value.Name] = append(judgementStates[value.Name], fileSet.Position(value.Pos()))
			}
			return true
		})

		for _, declaration := range parsed.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok || general.Tok != token.VAR {
				continue
			}
			for _, specification := range general.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, name := range value.Names {
					if name.Name != "AllStates" {
						continue
					}
					if allStatesFound {
						t.Fatalf("找到重複的 AllStates 宣告：%s:%d", path, fileSet.Position(name.Pos()).Line)
					}
					allStatesFound = true
					if index >= len(value.Values) {
						t.Fatalf("%s:%d 的 AllStates 沒有初始值；AST 掃描器已經看不懂這份原始碼了", path, fileSet.Position(name.Pos()).Line)
					}
					literal, ok := value.Values[index].(*ast.CompositeLit)
					if !ok || !isStateSliceType(literal.Type) {
						t.Fatalf("%s:%d 的 AllStates 不是 []State composite literal；AST 掃描器已經看不懂這份原始碼了", path, fileSet.Position(value.Values[index].Pos()).Line)
					}
					for _, element := range literal.Elts {
						allStateElements++
						stateName, ok := element.(*ast.Ident)
						if !ok {
							position := fileSet.Position(element.Pos())
							t.Fatalf("%s:%d 的 AllStates 元素不是單純識別字；AST 掃描器已經看不懂這份原始碼了", position.Filename, position.Line)
						}
						allStates[stateName.Name] = true
					}
				}
			}
		}
	}

	if len(judgementStates) < 5 {
		t.Fatalf("Judgement.State 不同識別字數量 got=%d, expected>=5；AST 掃描規則可能已失效", len(judgementStates))
	}
	if allStateElements < 5 {
		t.Fatalf("AllStates 元素數量 got=%d, expected>=5；AST 掃描規則可能已失效", allStateElements)
	}

	var sites []judgementStateSite
	for name, positions := range judgementStates {
		if allStates[name] {
			continue
		}
		for _, position := range positions {
			sites = append(sites, judgementStateSite{name: name, position: position})
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i].position.Filename != sites[j].position.Filename {
			return sites[i].position.Filename < sites[j].position.Filename
		}
		return sites[i].position.Line < sites[j].position.Line
	})
	for _, site := range sites {
		t.Errorf("Judgement state %s 在 %s:%d 被回傳，但它在 AllStates 之外，所以窮舉那一份清單會安靜地少一格；Prometheus 上『從來沒出現』跟『值是 0』長得一樣", site.name, site.position.Filename, site.position.Line)
	}
}

func productionStateFiles(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read internal/state: %v", err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		files = append(files, filepath.Clean(entry.Name()))
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatal("internal/state 沒有找到任何 production .go 檔；AST 掃描規則可能已失效")
	}
	return files
}

func isStateSliceType(expression ast.Expr) bool {
	array, ok := expression.(*ast.ArrayType)
	if !ok || array.Len != nil {
		return false
	}
	element, ok := array.Elt.(*ast.Ident)
	return ok && element.Name == "State"
}
