package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/operator"
	"github.com/teddashh/AI-Intune/internal/store"
)

var forbiddenInterfaceCopy = []string{
	"temporary guard",
	"temporary explanation",
	"暫時設定",
	"臨時入口",
	"尚未實作",
	"成果判定未接通",
	"答不出來",
	"請自己",
	"要人上去",
	"不提供假的控制項",
	"不顯示表單",
	"沒有此動作所需 capability",
	"目前 Tailscale 身分沒有",
	"不是免責",
	"暫時",
	"不代表",
	"不能證明",
	"無法證明",
	"沒有獨立驗證",
	"independent verifier",
	"不掃關鍵字",
	"請親自",
	"請聯絡管理者",
	"沒有退回 direct DB",
	"不宣稱",
	"這不等於",
	"不要把它當",
	"fail closed",
	"fallback audit",
	"仍會繼續",
	"注意：",
	"尚未初始化",
	"尚未設定",
	"免責聲明",
	"請自行判斷",
	"不保證",
	"可能缺少",
	"仍在更新",
	"這一頁存在的理由",
	"request guard",
	"legacy ledger",
	"再升級",
}

func TestTemplatesExcludeDefensiveInterfaceCopy(t *testing.T) {
	paths, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		raw, err := templateFS.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		assertInterfaceCopy(t, path, string(raw))
	}
}

func TestProductionGoStringsExcludeDefensiveInterfaceCopy(t *testing.T) {
	roots := []string{"../../cmd", ".."}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(node ast.Node) bool {
				literal, ok := node.(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(literal.Value)
				if err == nil {
					assertInterfaceCopy(t, path, value)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProductionShellExcludesDefensiveInterfaceCopy(t *testing.T) {
	err := filepath.WalkDir("../../ops", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".sh") || strings.HasPrefix(entry.Name(), "test-") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assertInterfaceCopy(t, path, string(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentInterfacesExcludeResponsibilityCopy(t *testing.T) {
	err := filepath.WalkDir("../../ops", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		extension := strings.ToLower(filepath.Ext(path))
		if entry.IsDir() || strings.HasPrefix(entry.Name(), "test-") ||
			(extension != ".sh" && extension != ".yml" && extension != ".yaml" && extension != ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		visible := make([]string, 0)
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "#") {
				visible = append(visible, line)
			}
		}
		lower := strings.ToLower(strings.Join(visible, "\n"))
		for _, phrase := range []string{
			"這不是驗收", "真正的驗收", "推的人自己說", "這句話是 prometheus 說的",
			"不是 ansible", "你自己", "你不會", "不會有人", "上去看", "請人工", "先人工", "需人工",
			"可能不存在", "請先比較",
		} {
			if strings.Contains(lower, phrase) {
				t.Errorf("%s contains responsibility copy %q", path, phrase)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func assertInterfaceCopy(t *testing.T, path, value string) {
	t.Helper()
	lower := strings.ToLower(value)
	for _, phrase := range forbiddenInterfaceCopy {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			t.Errorf("%s contains forbidden interface copy %q", path, phrase)
		}
	}
}

// jobIndependentConclusionCopy are the words that would turn a second
// producer's report into the Hub's own verdict about a job. The independent
// section says who wrote what and when the Hub received it; it never promotes
// that into "this job is verified" or "this machine is healthy".
var jobIndependentConclusionCopy = []string{
	"已驗證", "驗證通過", "驗證成功", "已確認", "確認無誤",
	"healthy", "健康", "正常運作", "is current", "up to date",
}

// TestIndependentEvidenceCopyReportsEvidenceNotAConclusion guards the one copy
// mistake this slice makes easy: a second producer saying "passed" is a report
// from that producer, not the Hub concluding the job is verified. The word
// "verified" is allowed only as the name of the timestamp the verifier itself
// reported.
func TestIndependentEvidenceCopyReportsEvidenceNotAConclusion(t *testing.T) {
	surfaces := map[string]string{}
	// Every surface that renders independent evidence is held to the same rule.
	// The job page reports one job's rows; the deployment page reports the same
	// verdicts one per target. Neither may turn them into the Hub's own answer.
	for _, block := range []struct{ name, file, open, close string }{
		{"templates/job.html independent section", "templates/job.html", "{{with $v.Evidence.Independent}}", ""},
		{"templates/deployment.html independent section", "templates/deployment.html",
			`<h2 id="deployment-targets"`, "<h3>排除</h3>"},
	} {
		raw, err := templateFS.ReadFile(block.file)
		if err != nil {
			t.Fatal(err)
		}
		surfaces[block.name] = independentTemplateSection(t, string(raw), block.open, block.close)
	}
	for _, verdict := range []string{
		string(store.IndependentAbsent), string(store.IndependentProducerRevoked),
		string(store.IndependentDigestMismatch), string(store.IndependentReleaseMismatch),
		string(store.IndependentStale), string(store.IndependentFailed),
		string(store.IndependentReleaseUnreported), string(store.IndependentPassed),
	} {
		label := independentVerdictLabel(verdict)
		if label == verdict {
			t.Errorf("verdict %q has no sentence of its own", verdict)
		}
		surfaces["independentVerdictLabel("+verdict+")"] = label
	}
	if independentVerdictLabel("not-a-verdict") != "not-a-verdict" {
		t.Error("an unknown verdict must not be given one of the six sentences")
	}
	for _, state := range []string{
		operator.JobAssignmentReported, operator.JobAssignmentProducerRevoked,
		operator.JobAssignmentWaitingForJob, operator.JobAssignmentAwaitingReport,
	} {
		label := assignmentStateLabel(state)
		if label == state {
			t.Errorf("assignment state %q has no sentence of its own", state)
		}
		surfaces["assignmentStateLabel("+state+")"] = label
	}
	if assignmentStateLabel("not-a-state") != "not-a-state" {
		t.Error("an unknown assignment state must not be given one of the four sentences")
	}

	for name, text := range surfaces {
		lowered := strings.ToLower(text)
		for _, banned := range jobIndependentConclusionCopy {
			if strings.Contains(lowered, strings.ToLower(banned)) {
				t.Errorf("%s states a conclusion with %q", name, banned)
			}
		}
		for _, word := range verifiedWordOccurrences(lowered) {
			if word != "verified_at" {
				t.Errorf("%s uses %q outside the verifier's own timestamp field", name, word)
			}
		}
	}
}

// independentTemplateSection returns the copy of one independent block: the
// template text between open and close (close "" means to the end of the page),
// with every {{action}} removed. Actions are Go expressions, not words an
// operator reads, and a field named ReportedVerifiedAt is not the page calling
// a job verified.
func independentTemplateSection(t *testing.T, page, open, close string) string {
	t.Helper()
	start := strings.Index(page, open)
	if start < 0 {
		t.Fatalf("template has no independent evidence section starting at %q", open)
	}
	section := page[start:]
	if close != "" {
		end := strings.Index(section, close)
		if end < 0 {
			t.Fatalf("independent evidence section has no end marker %q", close)
		}
		section = section[:end]
	}
	var copyOnly strings.Builder
	for rest := section; rest != ""; {
		at := strings.Index(rest, "{{")
		if at < 0 {
			copyOnly.WriteString(rest)
			break
		}
		copyOnly.WriteString(rest[:at])
		end := strings.Index(rest[at:], "}}")
		if end < 0 {
			t.Fatalf("independent evidence section has an unterminated action")
		}
		rest = rest[at+end+len("}}"):]
	}
	return copyOnly.String()
}

// verifiedWordOccurrences returns each occurrence of "verified" together with
// the identifier characters that follow it, so a bare "verified" can be told
// apart from the field name "verified_at".
func verifiedWordOccurrences(lowered string) []string {
	var found []string
	for i := 0; ; {
		at := strings.Index(lowered[i:], "verified")
		if at < 0 {
			return found
		}
		at += i
		end := at + len("verified")
		for end < len(lowered) && (lowered[end] == '_' ||
			(lowered[end] >= 'a' && lowered[end] <= 'z')) {
			end++
		}
		found = append(found, lowered[at:end])
		i = end
	}
}

// passed 這個 verdict 也涵蓋「所有列都沒回報 digest」的情況——那是沒有比過，
// 不是比過而且一樣。2026-09-12 第一次真機獨立驗證就是這個形狀：三條規則全通過、
// observed_digest 全空，而畫面上那一句當時宣稱 digest 相同。
// ⚠ 上面那張黑名單擋的是字面寫法，擋不住整類。實測把那一句改成
// 「每一列的 digest 都與這張工作單相同」——只在 digest 和「與」中間插一個
// 「都」字——四個樣式全部躲過去，全樹全綠。所以迴圈後面還要一段結構性斷言：
// 那一句必須講「沒有一列相衝突」這個**否定**主張。一個沒有回報 digest 的列，
// 既不衝突也不相同；只有否定的那個講得出口。
//
// 黑名單量的是「有沒有人寫出我想過的那句錯話」，
// 結構性斷言量的是「這句話還是不是原本那一類主張」。兩個都要。
//
// 下刀後四臂重量：
//   - 插「都」字那一發（原本全綠）→ 只有這一支紅。
//   - 換一種說法「digest 完全吻合」→ 也只有這一支紅。結構性斷言擋得住
//     我沒想過的寫法，這正是它存在的理由。
//   - 黑名單原本就擋得住的字面寫法 → 一樣只有這一支紅。
//   - ⚠「全部通過」那一句是**順帶覆蓋**：把它拿掉、衝突那半留著的時候，
//     除了這一支還有 TestDeploymentPageStatesCrossDomainEvidencePerTarget
//     與 TestJobPageStatesTheCrossDomainVerdictAsATypedState 會紅。
//     留著它是讓這一支自己說得完整，不是因為那個方向有缺口。
func TestThePassedSentenceNeverClaimsADigestComparisonThatDidNotHappen(t *testing.T) {
	terminalAt := time.Date(2026, 9, 12, 7, 25, 0, 0, time.UTC)
	verdict := store.EvaluateIndependentVerdict(
		"sha256:"+strings.Repeat("a", 64), "", &terminalAt,
		[]store.IndependentVerdictInput{{Passed: true, ReceivedAt: terminalAt.Add(time.Minute)}})
	if verdict != store.IndependentPassed {
		t.Fatalf("沒回報 digest 的通過列算出 %q，這支測試盯的是 passed 那一句", verdict)
	}
	label := independentVerdictLabel(string(verdict))
	for _, claim := range []string{"digest 與這張工作單相同", "digest 相同", "digest 一致", "digest 相符"} {
		if strings.Contains(label, claim) {
			t.Fatalf("passed 的句子說「%s」，但落到 passed 的列可以一個 digest 都沒回報。\n實際句子：%s",
				claim, label)
		}
	}
	if !strings.Contains(label, "相衝突") {
		t.Fatalf("passed 的句子能講的只有「沒有一列衝突」這個否定主張；一個沒有回報 digest 的列，既不衝突也不相同。句子裡沒有「相衝突」，就代表它已經改成在主張某種一致性。\n實際句子：%s", label)
	}
	if !strings.Contains(label, "全部通過") {
		t.Fatalf("passed 的句子要說清楚它在講的是規則通過，不是 digest 比對的結果。\n實際句子：%s", label)
	}
}
