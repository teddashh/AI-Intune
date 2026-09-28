package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

func decodeDisclosure(t *testing.T, body []byte) operator.DataDisclosure {
	t.Helper()
	var disclosure operator.DataDisclosure
	if err := json.Unmarshal(body, &disclosure); err != nil {
		t.Fatal(err)
	}
	return disclosure
}

func TestOperatorDataDisclosureAPINamesEveryCategory(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/data-disclosure")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", rec.Code,
			rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	disclosure := decodeDisclosure(t, rec.Body.Bytes())
	if disclosure.SchemaVersion != operator.DataDisclosureSchemaVersion ||
		len(disclosure.Categories) != len(operator.DataCategoryKeys()) {
		t.Fatalf("disclosure=%+v", disclosure)
	}
	if disclosure.Tables != len(store.MachineDataTables()) {
		t.Errorf("說涵蓋 %d 張表，實際量的是 %d 張", disclosure.Tables, len(store.MachineDataTables()))
	}
	for _, category := range disclosure.Categories {
		if category.Title == "" || category.Holds == "" || category.RetentionSentence == "" ||
			category.RetirementSentence == "" || category.FreeTextSentence == "" {
			t.Errorf("%s 沒有交代完自己：%+v", category.Key, category)
		}
	}
}

// 保留期調短，揭露面必須跟著短。印預設值的揭露面會繼續承諾一段 Hub 已經刪掉的歷史。
func TestOperatorDataDisclosureAPIFollowsAShortenedRetention(t *testing.T) {
	f := newJobsFixture(t, "cnode-data-retention")
	policy := store.DefaultRetention()
	policy.Occupancy = 12 * 24 * time.Hour
	(&hub{store: f.store, artifactsDir: f.artifactsDir, retention: policy}).operatorRoutes(f.mux)
	rec := reportGet(t, f, "/v1/operator/data-disclosure")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, category := range decodeDisclosure(t, rec.Body.Bytes()).Categories {
		if category.Key != operator.DataCategoryTickets {
			continue
		}
		if category.Retention.Days != 12 || !strings.Contains(category.RetentionSentence, "12") {
			t.Fatalf("保留期縮短之後的票證=%+v %q", category.Retention, category.RetentionSentence)
		}
		return
	}
	t.Fatal("揭露面裡沒有票證使用量")
}

func TestOperatorMachineDataAPICountsWhatIsThere(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/machines/"+f.machine.id+"/data")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q body=%s", rec.Code,
			rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	var result operator.MachineDataResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.MachineID != f.machine.id || result.Rows == 0 || result.Retired {
		t.Fatalf("result=%+v", result)
	}
	var summed int64
	for _, measured := range result.Categories {
		summed += measured.Rows
	}
	if summed != result.Rows {
		t.Fatalf("總列數 %d，逐類加起來是 %d", result.Rows, summed)
	}
}

func TestOperatorDataAPIsRefuseWhatTheyCannotAnswer(t *testing.T) {
	f, _ := reportFixture(t)
	for path, want := range map[string]int{
		"/v1/operator/data-disclosure?days=7":                    http.StatusBadRequest,
		"/v1/operator/machines/" + f.machine.id + "/data?days=7": http.StatusBadRequest,
		"/v1/operator/machines/不在名冊上/data":                       http.StatusNotFound,
	} {
		if rec := reportGet(t, f, path); rec.Code != want {
			t.Errorf("%s status=%d, want %d（body=%s）", path, rec.Code, want, rec.Body.String())
		}
	}
}

// 揭露面對每一類說「去這一頁看，需要 view 權限」。那句話必須跟那條 route 真正
// 要求的權限一致——否則操作員照著它走過去，會在門口被擋下來。
func TestEveryDisclosedPageRequiresTheCapabilityTheDisclosureNames(t *testing.T) {
	f, _ := reportFixture(t)
	rec := reportGet(t, f, "/v1/operator/data-disclosure")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, category := range decodeDisclosure(t, rec.Body.Bytes()).Categories {
		policy, known := operatorRoutePolicies["GET "+category.Path]
		if !known {
			t.Errorf("%s 指向 %q，那不是一條 operator route", category.Key, category.Path)
			continue
		}
		if policy.Permission.String() != category.Capability {
			t.Errorf("%s 說 %q 需要 %q，那條 route 要的是 %q",
				category.Key, category.Path, category.Capability, policy.Permission)
		}
	}
}
