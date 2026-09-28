package probe

import "testing"

func TestLoadAverageThatCannotBeReadIsNotAZeroLoad(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *float64
	}{
		{name: "正常負載", text: "0.52 0.41 0.38 1/234 5678", want: func() *float64 { v := 0.52; return &v }()},
		{name: "真的零負載", text: "0.00 0.00 0.00", want: func() *float64 { v := 0.0; return &v }()},
		{name: "空字串", text: "", want: nil},
		{name: "第一欄無法解析", text: "abc 1 2", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseLoad1m(tt.text)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("無法讀取的負載應為 nil，實際為 %v；把沒量到畫成 0.00，operator 會以為那台 Mac 很閒", *got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("負載解析結果 = %v，想要 %v；量到的 0 不能變成 nil，否則無法區分真的閒置與沒量到", got, *tt.want)
			}
		})
	}
}
