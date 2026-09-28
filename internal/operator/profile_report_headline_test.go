package operator

import "testing"

// 這三支文案函式都是只讀輸入欄位的純函式；下表是手寫、刻意挑過的分支交叉組合。
// 專案沒有「所有合法計數組合」的宣告清單，新增分支時請自己回來加一列，不要為了守衛去 production 加清單。

func TestTheProfileRowHeadlineSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name string
		row  ProfileRow
		want string
	}{
		// 這一臂在這張表出現之前，全 repo 沒有任何測試造出過這種列。
		{
			name: "在籍與已退役同時有而且沒有套件",
			row:  ProfileRow{ProfileID: "openclaw-standard", Revision: 1, AssignedOn: 2, RetiredOn: 3, Packages: []ProfilePackage{}},
			want: "openclaw-standard rev 1：機隊上 2 台穿著它，另有 3 台已退役的還是它；它一個套件都沒點名。",
		},
		{
			name: "只有在籍",
			row: ProfileRow{ProfileID: "openclaw-standard", Revision: 2, AssignedOn: 4, RetiredOn: 0, Packages: []ProfilePackage{
				{State: ProfilePackageAssignedAndSeen},
				{State: ProfilePackageAssignedNotSeen},
			}},
			want: "openclaw-standard rev 2：機隊上 4 台穿著它；它點名 2 個套件版本。",
		},
		{
			name: "只有已退役",
			row: ProfileRow{ProfileID: "openclaw-standard", Revision: 3, AssignedOn: 0, RetiredOn: 2, Packages: []ProfilePackage{
				{State: ProfilePackageNeither},
				{State: ProfilePackageAssignedAndSeen},
				{State: ProfilePackageSeenNotAssigned},
			}},
			want: "openclaw-standard rev 3：機隊上沒有機器穿著它，2 台已退役的還是它；它點名 3 個套件版本，其中 1 個這個 Hub 沒有指派過也沒有看到過。",
		},
		{
			name: "兩邊都沒有且量錯檔案",
			row: ProfileRow{ProfileID: "openclaw-standard", Revision: 4, AssignedOn: 0, RetiredOn: 0, Packages: []ProfilePackage{
				{State: ProfilePackageAssignedAndSeen, SeenMisattributedOn: 1},
				{State: ProfilePackageAssignedNotSeen},
			}},
			want: "openclaw-standard rev 4：沒有機器穿著它；它點名 2 個套件版本；另有 1 個看得到的版號量的是沒在跑的那一份。",
		},
		{
			name: "只有在籍且未知與量錯檔案都有",
			row: ProfileRow{ProfileID: "openclaw-standard", Revision: 5, AssignedOn: 1, Packages: []ProfilePackage{
				{State: ProfilePackageNeither, SeenMisattributedOn: 2},
				{State: ProfilePackageNeither},
				{State: ProfilePackageAssignedAndSeen},
				{State: ProfilePackageSeenNotAssigned},
			}},
			want: "openclaw-standard rev 5：機隊上 1 台穿著它；它點名 4 個套件版本，其中 2 個這個 Hub 沒有指派過也沒有看到過；另有 1 個看得到的版號量的是沒在跑的那一份。",
		},
		{
			name: "兩邊都沒有而且沒有套件",
			row:  ProfileRow{ProfileID: "openclaw-standard", Revision: 6, AssignedOn: 0, RetiredOn: 0, Packages: []ProfilePackage{}},
			want: "openclaw-standard rev 6：沒有機器穿著它；它一個套件都沒點名。",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := profileRowHeadline(tt.row)
			if got != tt.want {
				t.Errorf("輸入 ProfileID=%q、Revision=%d、AssignedOn=%d、RetiredOn=%d、Packages=%d，拿到 %q，期望 %q", tt.row.ProfileID, tt.row.Revision, tt.row.AssignedOn, tt.row.RetiredOn, len(tt.row.Packages), got, tt.want)
			}
		})
	}
}

func TestTheProfileReportHeadlineSaysTheseExactWords(t *testing.T) {
	tests := []struct {
		name   string
		report ProfileReport
		want   string
	}{
		{"還沒發佈", ProfileReport{Published: 0}, "還沒有發佈任何 profile。"},
		{"全部都穿著", ProfileReport{Published: 2, Machines: 5, Wearing: 5, Bare: 0, Unassigned: 0, SeenMisattributed: 0}, "發佈了 2 版 profile，分母 5 台：5 台身上有 profile。"},
		// 「幾台身上沒有 profile」在 CLI 人讀輸出裡只有這一句講得出來。
		{"有人身上沒有", ProfileReport{Published: 3, Machines: 5, Wearing: 3, Bare: 2, Unassigned: 0, SeenMisattributed: 0}, "發佈了 3 版 profile，分母 5 台：3 台身上有 profile，2 台身上沒有。"},
		{"樣樣都有", ProfileReport{Published: 4, Machines: 6, Wearing: 4, Bare: 2, Unassigned: 1, SeenMisattributed: 3}, "發佈了 4 版 profile，分母 6 台：4 台身上有 profile，2 台身上沒有；1 份是最新的一版卻一台都沒指派；另有 3 格看得到的版號量的是沒在跑的那一份。"},
		// 既有的守衛只走過「同時還有 Bare 與 Unassigned」的那一格。
		{"只有量錯檔案", ProfileReport{Published: 2, Machines: 4, Wearing: 4, Bare: 0, Unassigned: 0, SeenMisattributed: 3}, "發佈了 2 版 profile，分母 4 台：4 台身上有 profile；另有 3 格看得到的版號量的是沒在跑的那一份。"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ProfileReportHeadline(tt.report)
			if got != tt.want {
				t.Errorf("輸入 Published=%d、Machines=%d、Wearing=%d、Bare=%d、Unassigned=%d、SeenMisattributed=%d，拿到 %q，期望 %q", tt.report.Published, tt.report.Machines, tt.report.Wearing, tt.report.Bare, tt.report.Unassigned, tt.report.SeenMisattributed, got, tt.want)
			}
		})
	}
}

// 另外三臂與它們的優先順序已由 TestTheProfileNextStepAsksTheHarderQuestionsFirst 釘住，這裡只補沒人寫過的那一臂。
func TestTheProfileReportNextStepTellsAnEmptyHubToPublishFirst(t *testing.T) {
	got := profileReportNextStep(ProfileReport{Published: 0})
	want := "要讓機器裝東西，先發佈一份 profile。"
	if got != want {
		t.Errorf("拿到 %q，期望 %q", got, want)
	}
}
