package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// TestEveryJudgementPathLoadsExpectations 讀自己的原始碼，不是跑自己的程式。
//
// ⚠⚠ 這一支守的是 2026-09-04 實測到的一個 bug：`SetExpectations` 原本
// 只寫在 serve() 裡，於是 `clawctl-hub report` 與 `machines` 在**一條期望
// 都沒有**的世界裡重算判決。實測（`--since 1h`，只差一個環境變數）：
//
//	沒期望：+2 more in hub
//	有期望：+3 more in hub   ← 多的那一行就是 sampleagent2 的事件流判決
//
// cmdReport 的價值全在於「它印出來的，就是早上八點會送出去的那一則」，
// 所以一個會說謊的預覽比沒有預覽更糟。
//
// 為什麼用讀原始碼而不是跑起來測：判決少了期望之後**還是會產生一個
// 完全合理的答案**（別的 finding 遞補上來當主因）。那種輸出用行為測試
// 很難釘 —— 你要斷言的是「這裡少了一段」，而畫面上什麼都不缺。
// 直接問「這個函式有沒有走過那一段」才問得準。
func TestEveryJudgementPathLoadsExpectations(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range []string{"main.go", "machinescmd.go", "dailyreportcmd.go"} {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("讀不到 %s：%v", name, err)
		}
		files = append(files, f)
	}

	// 這些函式會產生或呈現判決（State / Reason / Findings）。
	// ⚠ 新增會產生判決的子指令時，要把它加進來 —— 加了才會被守。
	need := map[string]string{
		"serve":                "Hub 本體",
		"runDailyReportDirect": "停服 break-glass 早報預覽（必須跟真的送出去的那則一致）",
		"runMachinesDirect":    "停服 break-glass 產生每一台的 State 與 structured findings",
	}

	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			why, watched := need[fn.Name.Name]
			if !watched {
				continue
			}
			var loads bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident: // loadExpectations(st)
					if fun.Name == "loadExpectations" {
						loads = true
					}
				case *ast.SelectorExpr: // st.SetExpectations(...)
					if fun.Sel.Name == "SetExpectations" {
						loads = true
					}
				}
				return true
			})
			if !loads {
				t.Errorf("%s（%s）沒有載入期望就在產生判決 —— "+
					"少了期望它不會壞掉，它會給出一個看起來很合理的**別的**答案", fn.Name.Name, why)
			}
			delete(need, fn.Name.Name)
		}
	}
	for name := range need {
		t.Errorf("判決路徑原始碼裡找不到 %s —— 它改名或搬走了，這支守衛跟著失效了", name)
	}
}

// ⚠ 上面那支測試靠一份手寫名單。名單會過期，所以再問一次反面：
// 有沒有哪個 cmdXxx 呼叫了會產生判決的 store 方法、卻不在名單上。
func TestNoUnwatchedSubcommandProducesJudgements(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("讀不到 main.go：%v", err)
	}
	// 這些 store 方法回傳的東西裡帶著 State / Reason / Findings。
	judging := map[string]bool{"Overview": true, "Detail": true, "MachineState": true}
	watched := map[string]bool{"serve": true, "cmdReport": true, "cmdMachines": true}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "cmd") {
			continue
		}
		if watched[fn.Name.Name] {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if ok && judging[sel.Sel.Name] {
				t.Errorf("%s 呼叫了 %s（會產生判決），但不在守衛名單上 —— "+
					"把它加進 TestEveryJudgementPathLoadsExpectations",
					fn.Name.Name, sel.Sel.Name)
			}
			return true
		})
	}
}
