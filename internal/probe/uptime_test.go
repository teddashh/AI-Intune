package probe

import "testing"

func TestUptimeThatCannotBeReadIsNotAFreshBoot(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *int64
	}{
		{name: "measured", text: "123456.78 98765.43", want: func() *int64 { v := int64(123456); return &v }()},
		{name: "fresh boot", text: "0.42 0.10", want: func() *int64 { v := int64(0); return &v }()},
		{name: "empty", text: "", want: nil},
		{name: "invalid", text: "abc 1", want: nil},
		{name: "negative", text: "-1 2", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseUptimeSeconds(tc.text)
			if tc.want == nil && got != nil {
				t.Fatalf("parseUptimeSeconds(%q) = %d，預期 nil；否則會把沒量到印成 0s，operator 會以為那台 Mac 每顆心跳都剛開機／一直在重開", tc.text, *got)
			}
			if tc.want != nil && (got == nil || *got != *tc.want) {
				t.Fatalf("parseUptimeSeconds(%q) = %v，預期 %d；否則會混淆沒量到與真的剛開機，operator 會誤判那台 Mac 一直在重開", tc.text, got, *tc.want)
			}
		})
	}
}
