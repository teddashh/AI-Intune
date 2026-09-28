package operatorclient

import (
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func dataClientDisclosure(t *testing.T, now time.Time) operator.DataDisclosure {
	t.Helper()
	disclosure, err := operator.DataDisclosureFor(store.DefaultRetention(), now)
	if err != nil {
		t.Fatal(err)
	}
	return disclosure
}

func dataClientMachine(t *testing.T, now time.Time) operator.MachineDataResult {
	t.Helper()
	disclosure := dataClientDisclosure(t, now)
	result := operator.MachineDataResult{
		SchemaVersion: operator.DataDisclosureSchemaVersion, EvaluatedAt: now,
		MachineID: "m1", DisplayName: "samplehub1",
	}
	for index, category := range disclosure.Categories {
		measured := operator.MachineDataCategory{Category: category, Rows: int64(index)}
		if measured.Rows > 0 {
			oldest := now.Add(-time.Duration(index) * time.Hour)
			newest := now.Add(-time.Minute)
			measured.Oldest, measured.Newest = &oldest, &newest
		}
		if category.Retention.Kind == operator.DataRetentionTimed {
			cutoff := now.Add(-time.Duration(category.Retention.Days) * 24 * time.Hour)
			measured.CutoffAt = &cutoff
		}
		result.Categories = append(result.Categories, measured)
		result.Rows += measured.Rows
	}
	return result
}

func TestTheDataClientAcceptsADisclosureThatAgreesWithItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	disclosure, err := complianceClientServer(t, dataClientDisclosure(t, now)).DataDisclosure(t.Context())
	if err != nil {
		t.Fatalf("一致的揭露面被拒絕：%v", err)
	}
	if len(disclosure.Categories) != len(operator.DataCategoryKeys()) {
		t.Fatalf("disclosure=%+v", disclosure)
	}
}

// 揭露面的價值全在於它講的保留期就是 Hub 真的會遵守的那一個。一份自相矛盾的
// 揭露面——說不按時間清卻給了天數、兩類講同一張表、交代換了一種說法——不是拿來
// 顯示的東西，是拿來拒收的。
func TestTheDataClientRefusesADisclosureThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.DataDisclosure){
		"類別數對不起來": func(d *operator.DataDisclosure) { d.Categories = d.Categories[1:] },
		"順序被換過": func(d *operator.DataDisclosure) {
			d.Categories[0], d.Categories[1] = d.Categories[1], d.Categories[0]
		},
		"表數對不起來":      func(d *operator.DataDisclosure) { d.Tables++ },
		"會被清的類別數對不起來": func(d *operator.DataDisclosure) { d.Timed++ },
		"同一張表被講兩次": func(d *operator.DataDisclosure) {
			d.Categories[1].Tables = append(d.Categories[1].Tables, d.Categories[0].Tables[0])
			d.Tables++
		},
		"不按時間清卻給了天數": func(d *operator.DataDisclosure) {
			for index := range d.Categories {
				if d.Categories[index].Retention.Kind == operator.DataRetentionKept {
					d.Categories[index].Retention.Days = 30
					return
				}
			}
		},
		"會被清卻沒講多久": func(d *operator.DataDisclosure) {
			for index := range d.Categories {
				if d.Categories[index].Retention.Kind == operator.DataRetentionTimed {
					d.Categories[index].Retention.Days = 0
					d.Categories[index].RetentionSentence =
						operator.DataRetentionSentence(d.Categories[index].Retention)
					return
				}
			}
		},
		"固定筆數上限只講了一半": func(d *operator.DataDisclosure) {
			for index := range d.Categories {
				if d.Categories[index].Retention.RingRows > 0 {
					d.Categories[index].Retention.RingSubject = ""
					d.Categories[index].RetentionSentence =
						operator.DataRetentionSentence(d.Categories[index].Retention)
					return
				}
			}
		},
		"不認得的來源": func(d *operator.DataDisclosure) {
			d.Categories[0].Source = "somewhere"
			d.Categories[0].SourceSentence = operator.DataSourceSentence(d.Categories[0].Source)
		},
		"要求比 view 更高的權限": func(d *operator.DataDisclosure) { d.Categories[0].Capability = "admin" },
		"指向一個不是路徑的地方":    func(d *operator.DataDisclosure) { d.Categories[0].Path = "machines" },
		"保留期換了一種說法": func(d *operator.DataDisclosure) {
			d.Categories[0].RetentionSentence += "（大約）"
		},
		"來源換了一種說法": func(d *operator.DataDisclosure) {
			d.Categories[0].SourceSentence = "不知道是誰寫的。"
		},
		"退役之後換了一種說法": func(d *operator.DataDisclosure) {
			d.Categories[0].RetirementSentence = "都會被刪掉。"
		},
		"自由文字的交代對不上": func(d *operator.DataDisclosure) {
			d.Categories[0].FreeText += "；還有別的"
		},
		"標題裡有終端機跳脫序列": func(d *operator.DataDisclosure) {
			d.Categories[0].Title = "名冊\x1b[2J"
		},
	} {
		disclosure := dataClientDisclosure(t, now)
		edit(&disclosure)
		if _, err := complianceClientServer(t, disclosure).DataDisclosure(t.Context()); err == nil {
			t.Errorf("%s：自相矛盾的揭露面被接受了", name)
		}
	}
}

func TestTheMachineDataClientAsksTheCanonicalPath(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	client, asked := recordingReportServer(t, dataClientMachine(t, now))
	if _, err := client.MachineData(t.Context(), "m1"); err != nil {
		t.Fatalf("一致的單機揭露面被拒絕：%v", err)
	}
	if *asked != "/v1/operator/machines/m1/data" {
		t.Fatalf("用戶端問的是 %q", *asked)
	}
}

func TestTheMachineDataClientRefusesAResultThatContradictsItself(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	for name, edit := range map[string]func(*operator.MachineDataResult){
		"總列數對不起來": func(r *operator.MachineDataResult) { r.Rows++ },
		"沒有時刻的列數對不起來": func(r *operator.MachineDataResult) {
			r.Undated = r.Rows + 1
		},
		"一類的沒有時刻比總列數還多": func(r *operator.MachineDataResult) {
			r.Categories[1].Undated = r.Categories[1].Rows + 1
			r.Undated += r.Categories[1].Undated
		},
		"少了一類": func(r *operator.MachineDataResult) { r.Categories = r.Categories[1:] },
		"順序被換過": func(r *operator.MachineDataResult) {
			r.Categories[0], r.Categories[1] = r.Categories[1], r.Categories[0]
		},
		"一列都沒有卻給了時刻": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].Rows == 0 {
					r.Categories[index].Oldest = &now
					r.Categories[index].Newest = &now
					return
				}
			}
		},
		"最舊比最新還晚": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].Oldest != nil {
					later := r.Categories[index].Newest.Add(time.Hour)
					r.Categories[index].Oldest = &later
					return
				}
			}
		},
		"最新在這次讀取之後": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].Newest != nil {
					later := r.EvaluatedAt.Add(time.Hour)
					r.Categories[index].Newest = &later
					return
				}
			}
		},
		"只給了一半的時間範圍": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].Oldest != nil {
					r.Categories[index].Oldest = nil
					return
				}
			}
		},
		"不按時間清卻給了清除界線": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].CutoffAt == nil {
					r.Categories[index].CutoffAt = &now
					return
				}
			}
		},
		"清除界線跟保留期算出來的不一樣": func(r *operator.MachineDataResult) {
			for index := range r.Categories {
				if r.Categories[index].CutoffAt != nil {
					shifted := r.Categories[index].CutoffAt.Add(-24 * time.Hour)
					r.Categories[index].CutoffAt = &shifted
					return
				}
			}
		},
		"說退役了卻沒有退役時刻": func(r *operator.MachineDataResult) { r.Retired = true },
		"機器換了一台":      func(r *operator.MachineDataResult) { r.MachineID = "m2" },
		"名稱裡有終端機跳脫序列": func(r *operator.MachineDataResult) { r.DisplayName = "samplehub1\x1b[2J" },
	} {
		result := dataClientMachine(t, now)
		edit(&result)
		client, _ := recordingReportServer(t, result)
		if _, err := client.MachineData(t.Context(), "m1"); err == nil {
			t.Errorf("%s：自相矛盾的單機揭露面被接受了", name)
		}
	}
}
