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
		return errors.New("operator client: data disclosure identity is inconsistent")
	}
	keys := operator.DataCategoryKeys()
	if len(disclosure.Categories) != len(keys) {
		return fmt.Errorf("operator client: data disclosure has %d categories, this version recognizes %d categories",
			len(disclosure.Categories), len(keys))
	}
	tables, timed := 0, 0
	seen := map[string]operator.DataCategoryKey{}
	for index, category := range disclosure.Categories {
		if category.Key != keys[index] {
			return fmt.Errorf("operator client: category %d is %q, canonical order is %q",
				index, category.Key, keys[index])
		}
		if err := validateDataCategory(category); err != nil {
			return err
		}
		for _, table := range category.Tables {
			if previous, repeated := seen[table]; repeated {
				return fmt.Errorf("operator client: %q is covered by both %q and %q",
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
		return fmt.Errorf("operator client: data disclosure reports %d tables, %d categories timed, category totals sum to %d/%d",
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
		return fmt.Errorf("operator client: data category %q lacks description", category.Key)
	}
	if category.Capability != "view" {
		return fmt.Errorf("operator client: data category %q requires %q capability, but read surface requires view",
			category.Key, category.Capability)
	}
	switch category.Source {
	case operator.DataSourceMachine, operator.DataSourceHub, operator.DataSourceOperator:
	default:
		return fmt.Errorf("operator client: data category %q source is %q", category.Key, category.Source)
	}
	if err := validateDataRetention(category.Key, category.Retention); err != nil {
		return err
	}
	// 這四句是由同一組函式產生的。對面換了一種說法，就是對同一件事有第二種解釋。
	if category.SourceSentence != operator.DataSourceSentence(category.Source) ||
		category.RetentionSentence != operator.DataRetentionSentence(category.Retention) ||
		category.RetirementSentence != operator.DataRetirementSentence(category.Retention) ||
		category.FreeTextSentence != operator.DataFreeTextSentence(category.FreeText) {
		return fmt.Errorf("operator client: data category %q description does not match its fields", category.Key)
	}
	return nil
}

func validateDataRetention(key operator.DataCategoryKey, retention operator.DataRetention) error {
	switch retention.Kind {
	case operator.DataRetentionTimed:
		if retention.Days < 1 || retention.Class == "" {
			return fmt.Errorf("operator client: data category %q indicates time-based retention, but specifies no retention duration (%+v)",
				key, retention)
		}
	case operator.DataRetentionKept:
		if retention.Days != 0 || retention.Class != "" {
			return fmt.Errorf("operator client: data category %q indicates non-timed retention, but specifies a retention duration (%+v)",
				key, retention)
		}
	default:
		return fmt.Errorf("operator client: data category %q retention kind is %q", key, retention.Kind)
	}
	if (retention.RingRows > 0) != (retention.RingSubject != "") {
		return fmt.Errorf("operator client: data category %q fixed row limit is incomplete (%+v)",
			key, retention)
	}
	return nil
}

func validateMachineData(result operator.MachineDataResult, machineID string) error {
	if result.SchemaVersion != operator.DataDisclosureSchemaVersion || result.MachineID != machineID ||
		strings.TrimSpace(result.DisplayName) == "" ||
		result.EvaluatedAt.IsZero() || result.EvaluatedAt.Location() != time.UTC {
		return errors.New("operator client: machine data identity is inconsistent")
	}
	if err := validateMachineClientText("machine data display_name", result.DisplayName, 256); err != nil {
		return err
	}
	if result.Retired != (result.RetiredAt != nil) {
		return errors.New("operator client: machine data indicates retired but lacks retired_at timestamp")
	}
	keys := operator.DataCategoryKeys()
	if len(result.Categories) != len(keys) {
		return fmt.Errorf("operator client: machine data has %d categories, this version recognizes %d categories",
			len(result.Categories), len(keys))
	}
	var rows, undated int64
	for index, measured := range result.Categories {
		if measured.Category.Key != keys[index] {
			return fmt.Errorf("operator client: category %d is %q, canonical order is %q",
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
		return fmt.Errorf("operator client: machine data reports %d rows (%d rows without timestamp), category totals sum to %d/%d",
			result.Rows, result.Undated, rows, undated)
	}
	return nil
}

func validateMachineDataCategory(measured operator.MachineDataCategory, evaluatedAt time.Time) error {
	key := measured.Category.Key
	if measured.Rows < 0 || measured.Undated < 0 || measured.Undated > measured.Rows {
		return fmt.Errorf("operator client: data category %q row counts %d/%d are invalid",
			key, measured.Undated, measured.Rows)
	}
	if measured.Rows == 0 && (measured.Oldest != nil || measured.Newest != nil) {
		return fmt.Errorf("operator client: data category %q has no rows, but provides timestamps", key)
	}
	if (measured.Oldest == nil) != (measured.Newest == nil) {
		return fmt.Errorf("operator client: data category %q provides only partial time range", key)
	}
	if measured.Oldest != nil {
		if measured.Oldest.After(*measured.Newest) {
			return fmt.Errorf("operator client: data category %q oldest timestamp is after newest", key)
		}
		if measured.Newest.After(evaluatedAt) {
			return fmt.Errorf("operator client: data category %q newest timestamp is after read time", key)
		}
	}
	timed := measured.Category.Retention.Kind == operator.DataRetentionTimed
	if timed != (measured.CutoffAt != nil) {
		return fmt.Errorf("operator client: data category %q prune boundary does not match retention period", key)
	}
	if timed {
		want := evaluatedAt.Add(-time.Duration(measured.Category.Retention.Days) * 24 * time.Hour)
		if !measured.CutoffAt.Equal(want) {
			return fmt.Errorf("operator client: data category %q prune boundary is %v, retention period calculates to %v",
				key, measured.CutoffAt, want)
		}
	}
	return nil
}
