package operator

import "testing"

// 這四支文案函式都是只讀輸入欄位的純函式；下表是手寫、刻意挑過的分支交叉組合。
// 專案沒有「所有合法計數組合」的宣告清單，新增分支時請自己回來加一列，不要為了守衛去 production 加清單。

func TestTheToolHeadlineSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name string
		tool SoftwareTool
		want string
	}{
		{
			name: "有裝且版號一致也沒人沉默",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 4, AbsentOn: 0, UnreportedOn: 0, Spread: 1, Newest: "2026.7.1"},
			want: "openclaw：4/4 台上有，都是 2026.7.1。",
		},
		// 版號散開時必須明講幾種版號不一致，不能改口說「都是」最新版。
		{
			name: "有裝但版號散開",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 5, Spread: 3, Newest: "2026.7.1"},
			want: "openclaw：5/5 台上有，3 種版號不一致。",
		},
		// 沉默在 InstalledOn > 0 的那一行也不可以被吞掉；對照 production :601-604 的 ⚠⚠ 註解與只走 InstalledOn == 0 的既有測試。
		{
			name: "有裝且版號一致但有人沉默",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 3, AbsentOn: 0, UnreportedOn: 2, Spread: 1, Newest: "2026.7.1"},
			want: "openclaw：3/5 台上有，都是 2026.7.1；另有 2 台沒回報過。",
		},
		{
			name: "有裝且版號散開又有人沉默",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 4, AbsentOn: 1, UnreportedOn: 2, Spread: 2, Newest: "2026.7.1"},
			want: "openclaw：4/7 台上有，2 種版號不一致；另有 2 台沒回報過。",
		},
		{
			name: "有裝但沒有可比較的最新版",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 2, AbsentOn: 1, UnreportedOn: 0, Spread: 1, Newest: ""},
			want: "openclaw：2/3 台上有。",
		},
		// AbsentOn 在 InstalledOn > 0 的句子裡只進分母，不會被單獨講出來。
		{
			name: "有裝也有人沒裝",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 3, AbsentOn: 2, UnreportedOn: 0, Spread: 1, Newest: "2026.7.1"},
			want: "openclaw：3/5 台上有，都是 2026.7.1。",
		},
		{
			name: "版號散開但比不出最新版",
			tool: SoftwareTool{Name: "openclaw", InstalledOn: 3, Spread: 2, Newest: ""},
			want: "openclaw：3/3 台上有，2 種版號不一致。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SoftwareToolHeadline(tt.tool)
			if got != tt.want {
				t.Errorf("輸入 InstalledOn=%d、AbsentOn=%d、UnreportedOn=%d、Spread=%d、Newest=%q，拿到 %q，期望 %q", tt.tool.InstalledOn, tt.tool.AbsentOn, tt.tool.UnreportedOn, tt.tool.Spread, tt.tool.Newest, got, tt.want)
			}
		})
	}
}

func TestTheToolNextStepSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name string
		tool SoftwareTool
		want string
	}{
		{
			name: "版號散開且有最新版",
			tool: SoftwareTool{Spread: 2, Newest: "2026.7.1"},
			want: "要拉齊就把落後的那幾台升到 2026.7.1。",
		},
		// 這是目前的行為：比不出最新版時，版號不一致在下一步裡不見了，只留下沉默那句。
		{
			name: "版號散開但沒有最新版且有人沉默",
			tool: SoftwareTool{Spread: 2, Newest: "", UnreportedOn: 1},
			want: "沒回報過的那幾台先去看 agent 有沒有在回報。",
		},
		// 這是目前的行為：比不出最新版又沒人沉默時，drift 完全沒有下一步。
		{
			name: "版號散開但沒有最新版也沒人沉默",
			tool: SoftwareTool{Spread: 2, Newest: "", UnreportedOn: 0},
			want: "",
		},
		{
			name: "版號沒有散開但有人沉默",
			tool: SoftwareTool{Spread: 1, UnreportedOn: 2},
			want: "沒回報過的那幾台先去看 agent 有沒有在回報。",
		},
		{
			name: "沒事可做",
			tool: SoftwareTool{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := softwareToolNextStep(tt.tool)
			if got != tt.want {
				t.Errorf("輸入 Spread=%d、UnreportedOn=%d，拿到 %q，期望 %q", tt.tool.Spread, tt.tool.UnreportedOn, got, tt.want)
			}
		})
	}
}

// 其餘分支已有守衛，這裡只補兩條還沒有人讀過的早退。
func TestTheSoftwareReportHeadlineSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name   string
		report SoftwareReport
		want   string
	}{
		{"名冊沒有機器", SoftwareReport{Machines: 0}, "名冊上沒有機器，沒有東西可以清查。"},
		{"沒有任何工具", SoftwareReport{Machines: 5, Tools: []SoftwareTool{}}, "分母 5 台，還沒有任何一台回報過工具。"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SoftwareReportHeadline(tt.report)
			if got != tt.want {
				t.Errorf("輸入 Machines=%d、Tools=%d，拿到 %q，期望 %q", tt.report.Machines, len(tt.report.Tools), got, tt.want)
			}
		})
	}
}

// 另外三臂與它們的優先順序已由 TestTheNextStepAsksWhichFileBeforeItAsksWhoIsBehind 釘住，這裡只補沒人走過的那一臂。
func TestTheSoftwareReportNextStepStaysSilentWhenThereIsNothingToDo(t *testing.T) {
	got := softwareReportNextStep(SoftwareReport{Machines: 4, Reporting: 4})
	if got != "" {
		t.Errorf("拿到 %q，期望空字串", got)
	}
}
