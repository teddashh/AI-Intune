package operatorclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
)

// DataDisclosure reads what this Hub keeps about a machine: what each kind
// holds, who produced it, how long it stays, and what survives retirement.
//
// 用戶端自己再驗一次這份揭露面的自洽性。每一句交代都是由同一組函式產生的，所以
// 一份說了別的句子的回應不是拿來顯示的東西，是拿來拒收的：那表示對面的 Hub 對
// 「這一類留多久」有第二種說法，而揭露面講錯保留期的後果是一個守不住的承諾。
func (c *Client) DataDisclosure(ctx context.Context) (operator.DataDisclosure, error) {
	var out operator.DataDisclosure
	req, err := c.newOperatorRequest(ctx, http.MethodGet, "/v1/operator/data-disclosure", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: data disclosure returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "data disclosure", &out); err != nil {
		return out, err
	}
	if err := validateDataDisclosure(out); err != nil {
		return out, err
	}
	return out, nil
}

// MachineData reads how much of each kind this Hub currently holds about one
// machine.
func (c *Client) MachineData(ctx context.Context, machineID string) (operator.MachineDataResult, error) {
	var out operator.MachineDataResult
	req, err := c.newMachineOperatorRequest(ctx, http.MethodGet, machineID, "/data", nil)
	if err != nil {
		return out, err
	}
	response, err := c.doRaw(req)
	if err != nil {
		return out, err
	}
	if response.status != http.StatusOK {
		return out, fmt.Errorf("operator client: machine data returned HTTP %d", response.status)
	}
	if err := validateSettingReadHeaders(response.header); err != nil {
		return out, err
	}
	if err := decodeStrictJSONDocument(response.body, "machine data", &out); err != nil {
		return out, err
	}
	if err := validateMachineData(out, machineID); err != nil {
		return out, err
	}
	return out, nil
}

func validateDataDisclosure(disclosure operator.DataDisclosure) error {
	if disclosure.SchemaVersion != operator.DataDisclosureSchemaVersion ||
		disclosure.EvaluatedAt.IsZero() || disclosure.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: data disclosure identity 不一致")
	}
	keys := operator.DataCategoryKeys()
	if len(disclosure.Categories) != len(keys) {
		return fmt.Errorf("operator client: data disclosure 有 %d 類，這個版本認得 %d 類",
			len(disclosure.Categories), len(keys))
	}
	tables, timed := 0, 0
	seen := map[string]operator.DataCategoryKey{}
	for index, category := range disclosure.Categories {
		if category.Key != keys[index] {
			return fmt.Errorf("operator client: 第 %d 類是 %q，canonical 順序是 %q",
				index, category.Key, keys[index])
		}
		if err := validateDataCategory(category); err != nil {
			return err
		}
		for _, table := range category.Tables {
			if previous, repeated := seen[table]; repeated {
				return fmt.Errorf("operator client: %q 同時被 %q 與 %q 講了",
					table, previous, category.Key)
			}
			seen[table] = category.Key
		}
		tables += len(category.Tables)
		if category.Retention.Kind == operator.DataRetentionTimed {
			timed++
		}
	}
	if disclosure.Tables != tables || disclosure.Timed != timed {
		return fmt.Errorf("operator client: data disclosure 說涵蓋 %d 張表、%d 類會被清，逐類加起來是 %d／%d",
			disclosure.Tables, disclosure.Timed, tables, timed)
	}
	return nil
}

func validateDataCategory(category operator.DataCategory) error {
	// 這些字串會原樣印到終端機。控制字元在那裡是跳脫序列，不是文字。自由文字那一
	// 格可以是空的——那是「這一類沒有自由文字」的表示法，不是漏填。
	for name, value := range map[string]string{
		"data category title": category.Title, "data category holds": category.Holds,
		"data category path": category.Path,
	} {
		if err := validateMachineClientText(name, value, 512); err != nil {
			return err
		}
	}
	if category.FreeText != "" {
		if err := validateMachineClientText("data category free text", category.FreeText, 512); err != nil {
			return err
		}
	}
	if strings.TrimSpace(category.Title) == "" || strings.TrimSpace(category.Holds) == "" ||
		len(category.Tables) == 0 || !strings.HasPrefix(category.Path, "/") {
		return fmt.Errorf("operator client: data category %q 沒有交代自己是什麼", category.Key)
	}
	if category.Capability != "view" {
		return fmt.Errorf("operator client: data category %q 說要 %q 權限，讀取面只要 view",
			category.Key, category.Capability)
	}
	switch category.Source {
	case operator.DataSourceMachine, operator.DataSourceHub, operator.DataSourceOperator:
	default:
		return fmt.Errorf("operator client: data category %q 的來源是 %q", category.Key, category.Source)
	}
	if err := validateDataRetention(category.Key, category.Retention); err != nil {
		return err
	}
	// 這四句是由同一組函式產生的。對面換了一種說法，就是對同一件事有第二種解釋。
	if category.SourceSentence != operator.DataSourceSentence(category.Source) ||
		category.RetentionSentence != operator.DataRetentionSentence(category.Retention) ||
		category.RetirementSentence != operator.DataRetirementSentence(category.Retention) ||
		category.FreeTextSentence != operator.DataFreeTextSentence(category.FreeText) {
		return fmt.Errorf("operator client: data category %q 的交代跟它自己的欄位對不起來", category.Key)
	}
	return nil
}

func validateDataRetention(key operator.DataCategoryKey, retention operator.DataRetention) error {
	switch retention.Kind {
	case operator.DataRetentionTimed:
		if retention.Days < 1 || retention.Class == "" {
			return fmt.Errorf("operator client: data category %q 說會被時間清，卻沒有講清多久（%+v）",
				key, retention)
		}
	case operator.DataRetentionKept:
		if retention.Days != 0 || retention.Class != "" {
			return fmt.Errorf("operator client: data category %q 說不按時間清，卻給了一個保留期（%+v）",
				key, retention)
		}
	default:
		return fmt.Errorf("operator client: data category %q 的保留期種類是 %q", key, retention.Kind)
	}
	if (retention.RingRows > 0) != (retention.RingSubject != "") {
		return fmt.Errorf("operator client: data category %q 的固定筆數上限只講了一半（%+v）",
			key, retention)
	}
	return nil
}

func validateMachineData(result operator.MachineDataResult, machineID string) error {
	if result.SchemaVersion != operator.DataDisclosureSchemaVersion || result.MachineID != machineID ||
		strings.TrimSpace(result.DisplayName) == "" ||
		result.EvaluatedAt.IsZero() || result.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: machine data identity 不一致")
	}
	if err := validateMachineClientText("machine data display_name", result.DisplayName, 256); err != nil {
		return err
	}
	if result.Retired != (result.RetiredAt != nil) {
		return errors.New("operator client: machine data 說退役了卻沒有退役時刻")
	}
	keys := operator.DataCategoryKeys()
	if len(result.Categories) != len(keys) {
		return fmt.Errorf("operator client: machine data 有 %d 類，這個版本認得 %d 類",
			len(result.Categories), len(keys))
	}
	var rows, undated int64
	for index, measured := range result.Categories {
		if measured.Category.Key != keys[index] {
			return fmt.Errorf("operator client: 第 %d 類是 %q，canonical 順序是 %q",
				index, measured.Category.Key, keys[index])
		}
		if err := validateDataCategory(measured.Category); err != nil {
			return err
		}
		if err := validateMachineDataCategory(measured, result.EvaluatedAt); err != nil {
			return err
		}
		rows += measured.Rows
		undated += measured.Undated
	}
	if result.Rows != rows || result.Undated != undated {
		return fmt.Errorf("operator client: machine data 說有 %d 列（%d 列沒有時刻），逐類加起來是 %d／%d",
			result.Rows, result.Undated, rows, undated)
	}
	return nil
}

func validateMachineDataCategory(measured operator.MachineDataCategory, evaluatedAt time.Time) error {
	key := measured.Category.Key
	if measured.Rows < 0 || measured.Undated < 0 || measured.Undated > measured.Rows {
		return fmt.Errorf("operator client: data category %q 的列數 %d／%d 站不住",
			key, measured.Undated, measured.Rows)
	}
	if measured.Rows == 0 && (measured.Oldest != nil || measured.Newest != nil) {
		return fmt.Errorf("operator client: data category %q 一列都沒有，卻給了時刻", key)
	}
	if (measured.Oldest == nil) != (measured.Newest == nil) {
		return fmt.Errorf("operator client: data category %q 只給了一半的時間範圍", key)
	}
	if measured.Oldest != nil {
		if measured.Oldest.After(*measured.Newest) {
			return fmt.Errorf("operator client: data category %q 的最舊比最新還晚", key)
		}
		if measured.Newest.After(evaluatedAt) {
			return fmt.Errorf("operator client: data category %q 的最新在這次讀取之後", key)
		}
	}
	timed := measured.Category.Retention.Kind == operator.DataRetentionTimed
	if timed != (measured.CutoffAt != nil) {
		return fmt.Errorf("operator client: data category %q 的清除界線跟它的保留期對不起來", key)
	}
	if timed {
		want := evaluatedAt.Add(-time.Duration(measured.Category.Retention.Days) * 24 * time.Hour)
		if !measured.CutoffAt.Equal(want) {
			return fmt.Errorf("operator client: data category %q 的清除界線是 %v，保留期算出來是 %v",
				key, measured.CutoffAt, want)
		}
	}
	return nil
}
