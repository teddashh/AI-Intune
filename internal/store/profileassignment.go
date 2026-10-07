package store

// 指派狀態 —— 「這台機器**現在**身上是哪一份 profile」。
//
// ⚠⚠ 一台機器可以被指派很多次，帳本把每一次都留著（supersedes_assignment_id
// 串成一條鏈）。「現在身上那一份」是 assignment_revision 最大的那一筆 —— 這條
// 規則不是這裡發明的，是 operator_profile_assignment.go 的 preview 在用的同一條。
// 兩邊不准各有一套：盤面說的「現在誰有」如果跟確認頁說的「現在是什麼」不一樣，
// 那兩個畫面就會叫同一個人做兩件相反的事。
//
// ⚠ 這裡回的是**所有機器**現在的指派，不是「某一份 profile 指派給了誰」。
// 一份發佈了卻一台都沒指派的 profile，正是最該被看見的東西，而它在「先挑
// profile 再查它的指派」的形狀裡永遠查不出來 —— 那個查詢回 0 列，而 0 列跟
// 「這份 profile 不存在」在呼叫端長得一模一樣。正式庫 2026-09-12 就是這個狀態：
// machine_profiles 1 列、machine_profile_assignments 0 列。

import (
	"database/sql"
	"fmt"
	"time"
)

// ProfileAssignmentPackage 是一份指派裡的一個套件，連同它在帳本上留下的那兩筆。
//
// ⚠ DesiredID 與 JobID 一起帶出來，是因為指派這件事在這個帳本裡不是一個旗標：
// 它會寫一列安裝意圖，再開一張工作單。少了這兩個，畫面就只能說「指派了」，
// 說不出「指派變成了什麼」，而那正是每機安裝狀態那一頁在讀的東西。
type ProfileAssignmentPackage struct {
	Position  int    `json:"position"`
	PackageID string `json:"package_id"`
	Version   string `json:"version"`
	DesiredID string `json:"desired_id"`
	JobID     string `json:"job_id"`
	// Direct 是「這個套件是 profile 自己點名的」，false 表示它是被依賴帶進來的。
	Direct bool `json:"direct"`
}

// ProfileAssignment 是一台機器現在身上那一份 profile。
//
// ⚠ 這裡刻意不帶機器名字，也不帶「那張工作單走到哪裡了」。名字要從名冊來，
// 工作單的狀態在 jobs 上；把它們塞進同一個查詢，就等於讓「有指派的」變成分母，
// 而這份盤面真正要看見的是**沒有指派的那幾台**。
type ProfileAssignment struct {
	MachineID          string    `json:"machine_id"`
	AssignmentID       string    `json:"assignment_id"`
	AssignmentRevision int64     `json:"assignment_revision"`
	ProfileID          string    `json:"profile_id"`
	ProfileRevision    int64     `json:"profile_revision"`
	ProfileDigest      string    `json:"profile_digest"`
	AssignedAt         time.Time `json:"assigned_at"`
	AssignedBy         string    `json:"assigned_by"`
	// Packages 照 position 排。可以是空的：一份什麼都沒帶的指派是一個要照實
	// 顯示的狀態，不是一列可以在讀的時候丟掉的資料。
	Packages []ProfileAssignmentPackage `json:"packages"`
}

// FleetProfileAssignments 回每一台機器現在身上那一份 profile，照 machine_id 排。
//
// 沒有被指派過的機器不會出現在結果裡 —— 它們要從名冊那一側補進來，因為名冊才是
// 分母。讓這個查詢決定有哪些機器，等於讓「有指派的」變成分母。
func (s *Store) FleetProfileAssignments() ([]ProfileAssignment, error) {
	rows, err := s.rdb.Query(`
SELECT a.machine_id, a.assignment_id, a.assignment_revision, a.profile_id, a.profile_revision,
       a.profile_digest, a.assigned_at, a.assigned_by,
       p.position, p.package_id, p.package_version, p.desired_id, p.job_id, p.direct
  FROM machine_profile_assignments a
  LEFT JOIN machine_profile_assignment_packages p ON p.assignment_id = a.assignment_id
 WHERE a.assignment_revision = (
       SELECT MAX(newest.assignment_revision)
         FROM machine_profile_assignments newest
        WHERE newest.machine_id = a.machine_id)
 ORDER BY a.machine_id, p.position`)
	if err != nil {
		return nil, fmt.Errorf("store: read fleet profile assignments: %w", err)
	}
	defer rows.Close()

	var out []ProfileAssignment
	// ⚠ LEFT JOIN，所以一份指派會攤成好幾列；同一個 assignment_id 的後續列只是
	// 它的第 2、3… 個套件。收攏的鍵是 assignment_id，因為套件本來就是掛在指派上
	// 的。上面那個「只要現行那一筆」的條件讓一台機器只會有一份指派，所以此刻用
	// machine_id 收攏會得到一樣的結果——量過了，那個變異殺不掉，兩種寫法在現在
	// 這個查詢下是等價的。
	for rows.Next() {
		var row ProfileAssignment
		var assignedAt string
		var position sql.NullInt64
		var packageID, version, desiredID, jobID sql.NullString
		var direct sql.NullBool
		if err := rows.Scan(&row.MachineID, &row.AssignmentID, &row.AssignmentRevision,
			&row.ProfileID, &row.ProfileRevision, &row.ProfileDigest, &assignedAt, &row.AssignedBy,
			&position, &packageID, &version, &desiredID, &jobID, &direct); err != nil {
			return nil, fmt.Errorf("store: scan fleet profile assignments: %w", err)
		}
		row.AssignedAt = parseTime(assignedAt)
		if len(out) == 0 || out[len(out)-1].AssignmentID != row.AssignmentID {
			row.Packages = nil
			out = append(out, row)
		}
		if !position.Valid {
			continue // LEFT JOIN 的空側：這份指派一個套件都沒有
		}
		current := &out[len(out)-1]
		current.Packages = append(current.Packages, ProfileAssignmentPackage{
			Position:  int(position.Int64),
			PackageID: packageID.String,
			Version:   version.String,
			DesiredID: desiredID.String,
			JobID:     jobID.String,
			Direct:    direct.Bool,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read fleet profile assignments: %w", err)
	}
	return out, nil
}
