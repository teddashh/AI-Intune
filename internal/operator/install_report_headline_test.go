package operator

import "testing"

// 這四支文案函式都是只讀輸入欄位的純函式；下表是手寫、刻意挑過的分支交叉組合。
// 專案沒有「所有合法計數組合」的宣告清單，新增分支時請自己回來加一列，不要為守衛去 production 加清單。

func TestTheResourceHeadlineSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name     string
		resource InstallResource
		want     string
	}{
		{
			name:     "完全沒指派",
			resource: InstallResource{Name: "openclaw", AssignedOn: 0, UnassignedOn: 3},
			want:     "openclaw：3 台都沒有被指派過。",
		},
		{
			name:     "全部對上",
			resource: InstallResource{Name: "openclaw", AssignedOn: 4, MatchingOn: 4, DifferingOn: 0},
			want:     "openclaw：4/4 台被指派過，指派的跟看到的都一樣。",
		},
		// 沉默不是一個對得上。
		{
			name:     "全部沉默",
			resource: InstallResource{Name: "openclaw", AssignedOn: 5, MatchingOn: 0, DifferingOn: 0},
			want:     "openclaw：5/5 台被指派過；有 5 台對不起來。",
		},
		// 沉默不是一個對得上。
		{
			name:     "部分沉默",
			resource: InstallResource{Name: "openclaw", AssignedOn: 5, MatchingOn: 1, DifferingOn: 0},
			want:     "openclaw：5/5 台被指派過；有 4 台對不起來。",
		},
		{
			name:     "有不一樣",
			resource: InstallResource{Name: "openclaw", AssignedOn: 4, UnassignedOn: 1, MatchingOn: 1, DifferingOn: 2},
			want:     "openclaw：4/5 台被指派過，其中 2 台指派的跟看到的不一樣；有 1 台對不起來；另有 1 台沒有被指派過。",
		},
		{
			name:     "全對上但量錯檔案",
			resource: InstallResource{Name: "openclaw", AssignedOn: 4, MatchingOn: 4, MisattributedOn: 2},
			want:     "openclaw：4/4 台被指派過，指派的跟看到的都一樣；其中 2 台看到的版號量的是沒在跑的那一份。",
		},
		{
			name:     "四種都有",
			resource: InstallResource{Name: "openclaw", AssignedOn: 6, UnassignedOn: 2, MatchingOn: 1, DifferingOn: 2, MisattributedOn: 1},
			want:     "openclaw：6/8 台被指派過，其中 2 台指派的跟看到的不一樣；其中 1 台看到的版號量的是沒在跑的那一份；有 3 台對不起來；另有 2 台沒有被指派過。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InstallResourceHeadline(tt.resource)
			if got != tt.want {
				t.Errorf("輸入 AssignedOn=%d、UnassignedOn=%d、MatchingOn=%d、DifferingOn=%d、MisattributedOn=%d，拿到 %q，期望 %q", tt.resource.AssignedOn, tt.resource.UnassignedOn, tt.resource.MatchingOn, tt.resource.DifferingOn, tt.resource.MisattributedOn, got, tt.want)
			}
		})
	}
}

func TestTheResourceNextStepSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name     string
		resource InstallResource
		want     string
	}{
		{"量錯檔案排在最前面", InstallResource{MisattributedOn: 1, DifferingOn: 2, UnassignedOn: 3}, "先看那幾台看到的版號量的是沒在跑的那一份，再談哪一邊是對的。"},
		{"有不一樣也有未指派", InstallResource{DifferingOn: 2, UnassignedOn: 3}, "先看指派的跟看到的不一樣的那幾台，決定哪一邊是對的。"},
		{"只有未指派", InstallResource{UnassignedOn: 3}, "沒有被指派過的那幾台，要讓它們裝就指派一次。"},
		{"沒事可做", InstallResource{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := installResourceNextStep(tt.resource)
			if got != tt.want {
				t.Errorf("輸入 MisattributedOn=%d、DifferingOn=%d、UnassignedOn=%d，拿到 %q，期望 %q", tt.resource.MisattributedOn, tt.resource.DifferingOn, tt.resource.UnassignedOn, got, tt.want)
			}
		})
	}
}

func TestTheInstallReportHeadlineSaysTheseExactWords(t *testing.T) {
	// 這裡只有資源個數會進到句子裡。
	tests := []struct {
		name   string
		report InstallReport
		want   string
	}{
		{"名冊沒有機器", InstallReport{Machines: 0}, "名冊上沒有機器，沒有東西可以對。"},
		{"沒有任何資源", InstallReport{Machines: 5, Resources: []InstallResource{}}, "分母 5 台，還沒有任何一台被指派過任何東西。"},
		{"一個資源且有人未指派", InstallReport{Machines: 5, Resources: []InstallResource{{}}, Assigned: 4}, "分母 5 台、1 個資源：4 台被指派過；另有 1 台一個資源都沒有被指派過。"},
		{"兩個資源且有不一樣和量錯檔案", InstallReport{Machines: 5, Resources: []InstallResource{{}, {}}, Assigned: 5, Differing: 1, Misattributed: 2}, "分母 5 台、2 個資源：5 台被指派過，1 個資源上指派的跟看到的不一樣；另有 2 格看到的版號量的是沒在跑的那一份。"},
		{"全部乾淨", InstallReport{Machines: 3, Resources: []InstallResource{{}}, Assigned: 3}, "分母 3 台、1 個資源：3 台被指派過。"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InstallReportHeadline(tt.report)
			if got != tt.want {
				t.Errorf("輸入 Machines=%d、Resources=%d、Assigned=%d、Differing=%d、Misattributed=%d，拿到 %q，期望 %q", tt.report.Machines, len(tt.report.Resources), tt.report.Assigned, tt.report.Differing, tt.report.Misattributed, got, tt.want)
			}
		})
	}
}

func TestTheInstallReportNextStepSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name   string
		report InstallReport
		want   string
	}{
		{"量錯檔案排在最前面", InstallReport{Machines: 5, Assigned: 3, Differing: 1, Misattributed: 2}, "先看那幾格看到的版號量的是沒在跑的那一份，再談哪一個資源對不起來。"},
		{"有不一樣也有未指派", InstallReport{Machines: 5, Assigned: 3, Differing: 1}, "先看指派的跟看到的不一樣的那幾個資源。"},
		{"只有未指派", InstallReport{Machines: 5, Assigned: 3}, "一個資源都沒有被指派過的那幾台，要讓它們裝東西就指派一次。"},
		{"沒事可做", InstallReport{Machines: 5, Assigned: 5}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := installReportNextStep(tt.report)
			if got != tt.want {
				t.Errorf("輸入 Machines=%d、Assigned=%d、Differing=%d、Misattributed=%d，拿到 %q，期望 %q", tt.report.Machines, tt.report.Assigned, tt.report.Differing, tt.report.Misattributed, got, tt.want)
			}
		})
	}
}
