package operator

import (
	"testing"

	"github.com/teddashh/AI-Intune/internal/compliance"
)

func TestComplianceGraceLabelNamesEveryOperatorFacingStep(t *testing.T) {
	if compliance.MaxGraceSeconds != 86400 {
		t.Fatalf("寬限期上限變成 %d 秒；上限變了，上面那張表的『天』那一格要重新確認", compliance.MaxGraceSeconds)
	}

	tests := []struct {
		seconds int
		want    string
	}{
		{seconds: 0, want: "判定不符合就立即生效"},
		{seconds: 1, want: "連續不符合 1 秒後生效"},
		{seconds: 59, want: "連續不符合 59 秒後生效"},
		{seconds: 60, want: "連續不符合 1 分鐘後生效"},
		{seconds: 3599, want: "連續不符合 3599 秒後生效"},
		{seconds: 3600, want: "連續不符合 1 小時後生效"},
		{seconds: 5400, want: "連續不符合 90 分鐘後生效"},
		{seconds: 82800, want: "連續不符合 23 小時後生效"},
		{seconds: 86400, want: "連續不符合 1 天後生效"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			got := complianceGraceLabel(tc.seconds)
			if got != tc.want {
				t.Errorf("seconds=%d：實際輸出 %q，期望輸出 %q", tc.seconds, got, tc.want)
			}
		})
	}
}
