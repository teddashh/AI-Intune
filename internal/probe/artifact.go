package probe

import (
	"os"
	"time"

	"github.com/teddashh/AI-Intune/internal/model"
)

// CheckArtifacts 對每一條期望去看那個檔案，回報**事實**：在不在、多舊。
//
// ⚠⚠ 這裡刻意不判「過或不過」。門檻（MaxAgeSeconds）雖然就在手上，
// 判決仍然是 Hub 的事 —— model 開頭第 1 條規則：Agent 只回報事實。
// 讓 agent 判會有一個很實際的後果：改門檻要重新部署四台 agent，
// 而且一台舊 agent 會用舊門檻，於是同一個畫面上兩台機器的標準不一樣。
//
// ⚠ 只 stat，不開檔、不讀內容。讀內容就會走回「掃關鍵字」那條路，
// 而且產出物可能很大或含密鑰。我們要的只是「它有沒有被重新寫過」。
func CheckArtifacts(exps []model.Expectation) []model.ArtifactCheck {
	out := make([]model.ArtifactCheck, 0, len(exps))
	for _, e := range exps {
		out = append(out, statArtifact(e))
	}
	return out
}

func statArtifact(e model.Expectation) model.ArtifactCheck {
	c := model.ArtifactCheck{Unit: e.Unit, Artifact: e.Artifact}

	// ⚠ 用 Stat 不是 Lstat：宣告的人想知道的是「那份產出物新不新」，
	// 而一條指向新檔案的 symlink 是滿足這個期望的。
	// 但 symlink 斷掉要能分辨 —— 那會走到下面 IsNotExist 之外的錯誤分支。
	fi, err := os.Stat(e.Artifact)
	switch {
	case err == nil:
		mt := fi.ModTime().UTC()
		c.Exists, c.ModTime, c.Size = true, &mt, fi.Size()
	case os.IsNotExist(err):
		// ⚠ 「不存在」是一個**答案**，不是錯誤。Err 留空。
		// 一個從來沒被產出過的檔案，跟一個我們沒權限看的檔案，
		// 對修的人來說是完全不同的兩件事。
		c.Exists = false
	default:
		c.Err = err.Error()
	}
	return c
}

// ⚠ 這個檔案沒有時間來源。「多舊」由 Hub 用它自己的時鐘算 ——
// model 開頭第 2 條規則：生死判斷一律用 Hub 的時間。
// sampleagent4 的時鐘快 79 秒，讓機器自己算年齡會讓它的產出物看起來比實際新。
var _ = time.Time{}
