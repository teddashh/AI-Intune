package store

// 安裝意圖 —— 「Hub 最後一次叫這個範圍裝什麼」。
//
// ⚠⚠ 意圖在這個帳本裡有兩個 scope，而且它們共用同一條號碼帶。
// createDesiredStateTx 的計數器 key 是資源、**不是** scope（deploy.go），
// 就是為了讓 machine scope 與 channel scope 永遠比得出先後。所以「這台機器
// 最後被叫去裝什麼」的答案，是它自己那個 scope 與它 channel 那個 scope 裡
// **revision 最大**的那一筆 —— 不是「machine 蓋過 channel」，也不是反過來。
// 這裡只把每個 scope 的最後一筆各撈出來，誰贏由 revision 決定；那條比較規則
// 就是 agent MaxSeen 擋降版用的同一條，兩邊不准各有一套。
//
// ⚠ noop 不是安裝意圖。validateDeploymentMaterial 已經把 kind=="noop" 判成
// 不可部署的素材：診斷用的 noop 排在安裝之後，只代表後來又測過一次工作單通道，
// 不代表那台機器被叫去解除安裝。所以這裡跳過 noop，回的是每個 scope 最後一筆
// **真的會裝東西的**。正式庫 2026-09-12 的 22 列 machine scope 裡有 12 列是
// 診斷 noop，不跳過的話四台機器的安裝意圖會全部變成「noop」。

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// InstallIntent 是一個 scope 上、一個資源的最後一筆安裝意圖。
//
// ⚠ 這裡刻意不帶機器名字，也不帶「裝到了沒有」。名字要從名冊來，執行的那一半
// 在 jobs 上；把它們塞進同一個查詢，就等於讓「有意圖的」變成分母，而這份報告
// 真正要看見的是**沒有意圖的那幾台**。
type InstallIntent struct {
	ScopeType    string `json:"scope_type"` // "machine" 或 "channel"
	ScopeID      string `json:"scope_id"`
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	DesiredID    string `json:"desired_id"`
	Revision     int64  `json:"revision"`
	// Kind 是 spec 的 kind，永遠不會是 "noop"。
	Kind string `json:"kind"`
	// Version 是 spec 自己講的版號；沒講就是空字串，那是一個要照實顯示的狀態，
	// 不是可以拿別的欄位補上去的空格。
	Version   string    `json:"version,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by"`
}

// FleetInstallIntents 回每一個 scope × 每一個資源的最後一筆安裝意圖。
//
// ⚠ 回的是所有 scope，不是某一台的。呼叫端拿名冊去挑哪幾個 scope 對哪一台
// 適用 —— 因為一個掛著機器卻一列意圖都沒有的 channel，本身就是要被看見的發現，
// 而那個發現在「先挑機器再查」的形狀裡永遠查不出來。
func (s *Store) FleetInstallIntents() ([]InstallIntent, error) {
	rows, err := s.rdb.Query(`
SELECT scope_type, scope_id, resource_kind, resource_id, desired_id, revision, spec, created_at, created_by
  FROM desired_state
 ORDER BY revision DESC, rowid DESC`)
	if err != nil {
		return nil, fmt.Errorf("store: read fleet install intents: %w", err)
	}
	defer rows.Close()

	var out []InstallIntent
	seen := map[string]bool{}
	for rows.Next() {
		var it InstallIntent
		var spec, created string
		if err := rows.Scan(&it.ScopeType, &it.ScopeID, &it.ResourceKind, &it.ResourceID,
			&it.DesiredID, &it.Revision, &spec, &created, &it.CreatedBy); err != nil {
			return nil, fmt.Errorf("store: scan fleet install intents: %w", err)
		}
		key := it.ScopeType + "\x00" + it.ScopeID + "\x00" + it.ResourceKind + "\x00" + it.ResourceID
		if seen[key] {
			continue // revision DESC，同一個 scope×資源第一次看到的就是最後一筆
		}
		kind, _, err := model.ParseJobSpec([]byte(spec))
		if err != nil || kind == "" {
			// 解不開的 spec 不能當成安裝意圖，但它也**不是**「這個 scope 沒有
			// 意圖」—— 正確做法是讓那一格自己有一種狀態。現在整個帳本寫入端
			// （CreateDesiredState 與 validateDeploymentMaterial）都擋著這種
			// 列，所以照 FleetTools 的既有做法先跳過；等真的出現第一筆再處理，
			// 現在補一個猜的分支，會是一個永遠沒有人驗證過的分支。
			continue
		}
		if kind == "noop" {
			continue // 診斷用的空工作單，不是安裝意圖；但它把這個 scope 的號碼帶往前推了
		}
		seen[key] = true
		it.Kind = kind
		it.Version = specVersion(spec)
		it.CreatedAt = parseTime(created)
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read fleet install intents: %w", err)
	}
	return out, nil
}

// specVersion 讀 spec 自己講的版號。
//
// ⚠ kind 一律走 model.ParseJobSpec，因為那是 admission gate 判 noop 的同一段；
// 版號另外讀，是因為那個 parser 只回 kind 與 artifact。讀不到就是空字串 ——
// 不准回退去猜 artifact 的檔名或 resource_id。
func specVersion(raw string) string {
	var spec model.OpenClawSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return ""
	}
	return spec.Version
}

// InstallIntentVersion 是這個帳本上**出現過**的一個（資源, 版本）。
//
// ⚠⚠ 它跟 FleetInstallIntents 問的是兩個不同的問題。那一個問「現在」——每個
// scope 每個資源的最後一筆；這一個問「有沒有過」——整條 desired_state 的歷史。
// 兩者不能互相代替：一份 profile 點名 openclaw 2026.9.2 而現行意圖裡沒有這一版，
// 只代表「現在沒有人被叫去裝它」，不代表「從來沒有人被叫去裝它」。畫面上要講
// 「這個 Hub 沒有指派過這一版」的話，只能讀這裡——拿現行意圖去講那句話，是在
// 用一個現在式的事實回答一個過去式的問題。
type InstallIntentVersion struct {
	ResourceKind string `json:"resource_kind"`
	ResourceID   string `json:"resource_id"`
	// Version 是 spec 自己講的版號。沒講版號的意圖不會出現在這裡：一個「指派過
	// 但沒說哪一版」的事實回答不了「有沒有指派過這一版」。
	Version string `json:"version"`
	// Intents 是有幾列安裝意圖點名過它。
	Intents int `json:"intents"`
	// FirstAt / LastAt 是第一次與最後一次被點名的時刻（Hub 自己的鐘）。
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
}

// FleetIntentVersions 回這個帳本上出現過的每一個（資源, 版本），照資源再照版號排。
//
// ⚠ noop 照樣跳過，理由跟 FleetInstallIntents 一樣：診斷用的空工作單不是安裝意圖。
// 這裡不做 scope 的收攏——一版被 machine scope 與 channel scope 各指派過一次，
// 算兩列意圖，因為「幾列意圖點名過它」問的就是這個。
func (s *Store) FleetIntentVersions() ([]InstallIntentVersion, error) {
	rows, err := s.rdb.Query(`
SELECT resource_kind, resource_id, spec, created_at
  FROM desired_state
 ORDER BY rowid`)
	if err != nil {
		return nil, fmt.Errorf("store: read assigned versions: %w", err)
	}
	defer rows.Close()

	type versionKey struct{ kind, id, version string }
	index := map[versionKey]int{}
	var out []InstallIntentVersion
	for rows.Next() {
		var kindColumn, idColumn, spec, created string
		if err := rows.Scan(&kindColumn, &idColumn, &spec, &created); err != nil {
			return nil, fmt.Errorf("store: scan assigned versions: %w", err)
		}
		specKind, _, err := model.ParseJobSpec([]byte(spec))
		if err != nil || specKind == "" || specKind == "noop" {
			continue
		}
		version := specVersion(spec)
		if version == "" {
			continue
		}
		key := versionKey{kind: kindColumn, id: idColumn, version: version}
		at := parseTime(created)
		position, seen := index[key]
		if !seen {
			index[key] = len(out)
			out = append(out, InstallIntentVersion{
				ResourceKind: kindColumn, ResourceID: idColumn, Version: version,
				Intents: 1, FirstAt: at, LastAt: at,
			})
			continue
		}
		row := &out[position]
		row.Intents++
		// ⚠ 不假設 rowid 的順序就是時間順序。一次 restore 或一次 migration 之後
		// 插入順序跟 created_at 可以不一致，而「第一次被點名是什麼時候」被講錯的
		// 時候，畫面上看不出來。
		if !at.IsZero() && (row.FirstAt.IsZero() || at.Before(row.FirstAt)) {
			row.FirstAt = at
		}
		if at.After(row.LastAt) {
			row.LastAt = at
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read assigned versions: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ResourceKind != out[j].ResourceKind {
			return out[i].ResourceKind < out[j].ResourceKind
		}
		if out[i].ResourceID != out[j].ResourceID {
			return out[i].ResourceID < out[j].ResourceID
		}
		return out[i].Version < out[j].Version
	})
	return out, nil
}
