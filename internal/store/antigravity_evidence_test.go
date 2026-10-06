package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/teddashh/AI-Intune/internal/deploy"
)

func TestAntigravityMeasuredEvidenceRequiresBothRulesFromOneRelease(t *testing.T) {
	const version = "1.2.14"
	hash := strings.Repeat("e", 64)
	spec := antigravityEvidenceSpec("linux", "amd64", version, hash)
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, "agy-linux")
	jobID := insertMeasuredEvidenceJob(t, s, "agy-linux", "antigravity", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-linux")
	if err := s.RecordVerification(jobID, "agy-linux", token, "self-report",
		"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
		t.Fatalf("self-attested Antigravity error=%v", err)
	}
	release := "/home/operator/.local/share/clawctl/antigravity/releases/" + version
	recordAntigravityEvidence(t, s, jobID, "agy-linux", token, "antigravity-activate",
		release, "/bin/agy", hash, version+"\n")
	if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
		t.Fatalf("measured state=%q err=%v", state, err)
	}

	t.Run("wrong version text", func(t *testing.T) {
		assertAntigravityEvidenceNotReady(t, "agy-version", spec, func(release, binary string) (string, string) {
			return "sha256:" + hash + "\n", "agy " + version + "\n"
		})
	})
	t.Run("wrong marker", func(t *testing.T) {
		assertAntigravityEvidenceNotReady(t, "agy-marker", spec, func(release, binary string) (string, string) {
			return "sha256:" + strings.Repeat("f", 64) + "\n", version + "\n"
		})
	})
	t.Run("same release with both rules is ready", func(t *testing.T) {
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "agy-control")
		jobID := insertMeasuredEvidenceJob(t, s, "agy-control", "antigravity", spec)
		token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-control")
		recordAntigravityEvidence(t, s, jobID, "agy-control", token, "antigravity-activate",
			"/home/operator/.local/share/clawctl/antigravity/releases/"+version, "/bin/agy", hash, version+"\n")
		if required, ready, err := s.measuredEvidenceStatus(jobID); err != nil || !required || !ready {
			t.Fatalf("required=%t ready=%t err=%v", required, ready, err)
		}
	})
	t.Run("invalid spec version still requires evidence", func(t *testing.T) {
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "agy-bad-version")
		jobID := insertMeasuredEvidenceJob(t, s, "agy-bad-version", "antigravity",
			antigravityEvidenceSpec("linux", "amd64", "1.2", hash))
		token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-bad-version")
		if err := s.RecordVerification(jobID, "agy-bad-version", token, "self-report",
			"echo installed", 0, "ok", "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
		if required, ready, err := s.measuredEvidenceStatus(jobID); err != nil || !required || ready {
			t.Fatalf("required=%t ready=%t err=%v", required, ready, err)
		}
		if _, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); !errors.Is(err, ErrNoVerification) {
			t.Fatalf("invalid version succeeded on self-report: %v", err)
		}
	})
	t.Run("wrong suffix", func(t *testing.T) {
		assertAntigravityEvidenceNotReady(t, "agy-suffix", spec, func(release, binary string) (string, string) {
			return "", ""
		})
	})
	t.Run("different release paths", func(t *testing.T) {
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "agy-split")
		jobID := insertMeasuredEvidenceJob(t, s, "agy-split", "antigravity", spec)
		token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-split")
		other := "/home/other/.local/share/clawctl/antigravity/releases/" + version
		recordAntigravityPair(t, s, jobID, "agy-split", token, "antigravity-current",
			"cat "+release+"/.clawctl-artifact-sha256", "sha256:"+hash+"\n",
			other+"/bin/agy --version", version+"\n")
		if _, ready, err := s.measuredEvidenceStatus(jobID); err != nil || ready {
			t.Fatalf("ready=%t err=%v", ready, err)
		}
	})
	t.Run("windows exe", func(t *testing.T) {
		windows := antigravityEvidenceSpec("windows", "amd64", version, hash)
		s := newDeployTestStore(t)
		registerDeployMachine(t, s, "agy-windows")
		jobID := insertMeasuredEvidenceJob(t, s, "agy-windows", "antigravity", windows)
		token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-windows")
		winRelease := "C:/Users/operator/.local/share/clawctl/antigravity/releases/" + version
		recordAntigravityEvidence(t, s, jobID, "agy-windows", token, "antigravity-current",
			winRelease, "/bin/agy.exe", hash, "\n"+version+"\n")
		if state, err := s.MarkSucceededIfVerified(jobID, deployTestNow.Add(time.Second)); err != nil || state != deploy.Succeeded {
			t.Fatalf("windows state=%q err=%v", state, err)
		}
	})
	for _, tc := range []struct {
		name, column, value string
	}{
		{name: "producer", column: "producer_kind", value: "hub"},
		{name: "role", column: "evidence_role", value: "observer"},
		{name: "authority", column: "authority", value: "operator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDeployTestStore(t)
			registerDeployMachine(t, s, "agy-"+tc.name)
			jobID := insertMeasuredEvidenceJob(t, s, "agy-"+tc.name, "antigravity", spec)
			token := advanceMeasuredEvidenceJob(t, s, jobID, "agy-"+tc.name)
			recordAntigravityEvidence(t, s, jobID, "agy-"+tc.name, token, "antigravity-activate",
				release, "/bin/agy", hash, version+"\n")
			if _, err := s.DB().Exec(`UPDATE verification_results SET `+tc.column+`=? WHERE job_id=?`, tc.value, jobID); err != nil {
				t.Fatal(err)
			}
			if _, ready, err := s.measuredEvidenceStatus(jobID); err != nil || ready {
				t.Fatalf("ready=%t err=%v", ready, err)
			}
		})
	}
}

func assertAntigravityEvidenceNotReady(t *testing.T, machine, spec string, stdouts func(release, binary string) (string, string)) {
	t.Helper()
	s := newDeployTestStore(t)
	registerDeployMachine(t, s, machine)
	jobID := insertMeasuredEvidenceJob(t, s, machine, "antigravity", spec)
	token := advanceMeasuredEvidenceJob(t, s, jobID, machine)
	release := "/home/operator/.local/share/clawctl/antigravity/releases/1.2.14"
	binary := release + "/bin/agy"
	artifactStdout, versionStdout := stdouts(release, binary)
	if artifactStdout == "" {
		recordAntigravityPair(t, s, jobID, machine, token, "antigravity-activate",
			"cat /tmp/agy-marker", "sha256:"+strings.Repeat("e", 64)+"\n",
			"/tmp/agy --version", "1.2.14\n")
	} else {
		recordAntigravityPair(t, s, jobID, machine, token, "antigravity-activate",
			"cat "+release+"/.clawctl-artifact-sha256", artifactStdout,
			binary+" --version", versionStdout)
	}
	if _, ready, err := s.measuredEvidenceStatus(jobID); err != nil || ready {
		t.Fatalf("ready=%t err=%v", ready, err)
	}
}

func antigravityEvidenceSpec(targetOS, targetArch, version, hash string) string {
	return `{"kind":"antigravity","version":"` + version + `","target_os":"` + targetOS + `","target_arch":"` + targetArch +
		`","bundle_layout":"antigravity-bundle:v1","artifact":{"sha256":"` + hash + `","size":1,"url":"/v1/artifacts/` + hash + `"}}`
}

func recordAntigravityEvidence(t *testing.T, s *Store, jobID, machine, token, prefix, release, binary, hash, versionStdout string) {
	t.Helper()
	recordAntigravityPair(t, s, jobID, machine, token, prefix,
		"cat "+release+"/.clawctl-artifact-sha256", "sha256:"+hash+"\n",
		release+binary+" --version", versionStdout)
}

func recordAntigravityPair(t *testing.T, s *Store, jobID, machine, token, prefix, artifactCommand, artifactStdout, versionCommand, versionStdout string) {
	t.Helper()
	for _, evidence := range []struct{ rule, command, stdout string }{
		{prefix + "-artifact", artifactCommand, artifactStdout},
		{prefix + "-version", versionCommand, versionStdout},
	} {
		if err := s.RecordVerification(jobID, machine, token, evidence.rule,
			evidence.command, 0, evidence.stdout, "", true, deployTestNow); err != nil {
			t.Fatal(err)
		}
	}
}
