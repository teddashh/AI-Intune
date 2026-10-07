package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/teddashh/AI-Intune/internal/artifact"
	"github.com/teddashh/AI-Intune/internal/store"
)

func cmdJob(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: clawctl-hub job create|list|show|evidence [flags]")
		os.Exit(2)
	}
	switch argv[0] {
	case "create":
		if err := runJobCreateCommand(context.Background(), argv[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Fatal(terminalSafe(err.Error()))
		}
	case "list", "show", "evidence":
		if err := runJobReadCommand(context.Background(), argv, os.Stdout, os.Stderr); err != nil && !errors.Is(err, flag.ErrHelp) {
			log.Fatal(terminalSafe(err.Error()))
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: job %s (available: create, list, show, evidence)\n", argv[0])
		os.Exit(2)
	}
}

type openClawJobMaterial = artifact.OpenClawMaterial

// prepareOpenClawJob 是 deployment preview/create 共用的 artifact → spec → digest 路徑。
// ⚠ 預覽與 create 若各自生成，預覽可能拿一份 engines、agent 卻收到另一份 spec。
func prepareOpenClawJob(artifactsDir, version, requestedSHA256 string) (openClawJobMaterial, error) {
	return artifact.ResolveOpenClawMaterial(artifactsDir, version, requestedSHA256)
}

// marshalCompactNoEscape 生成 spec 用：不做 HTML 轉義，engines 的 ">=22.19.0" 存進 DB
// 就是人看得懂的原文，也跟 writeJSON（同樣不轉義）回給 agent 的位元組一致。
func marshalCompactNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func selectArtifactForJob(dir, version, requestedSHA256 string) (artifactSidecar, error) {
	return artifact.SelectOpenClawArtifact(dir, strings.TrimSpace(version), strings.TrimSpace(requestedSHA256))
}

// resolveMachine 把人打的名字對到名冊上的一台。
// allowRetired=false 用在開單（退場的機器不開單），true 用在看歷史。
func resolveMachine(st *store.Store, name string, allowRetired bool) (store.Machine, error) {
	if name == "" {
		return store.Machine{}, errors.New("must specify --machine (display_name or machine_id)")
	}
	ms, err := st.ListMachines()
	if err != nil {
		return store.Machine{}, err
	}
	var hits []store.Machine
	for _, m := range ms {
		if m.MachineID == name || m.DisplayName == name {
			hits = append(hits, m)
		}
	}
	switch len(hits) {
	case 0:
		return store.Machine{}, fmt.Errorf("machine %q not found in registry", name)
	case 1:
		if hits[0].RetiredAt != nil && !allowRetired {
			return store.Machine{}, fmt.Errorf("%s was retired at %s, cannot create job", name, hits[0].RetiredAt.UTC().Format(time.RFC3339))
		}
		return hits[0], nil
	default:
		// ⚠ display_name 不保證唯一（重灌後再報到就會有兩台同名）。
		// 猜一台的話，單會開到另一台 —— 這裡要人用 machine_id 講清楚。
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.MachineID)
		}
		return store.Machine{}, fmt.Errorf("%q matched %d machines: %s - specify machine_id", name, len(hits), strings.Join(ids, ", "))
	}
}
