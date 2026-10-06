# Intune UI reference contract

This document turns the supplied localized Microsoft Intune reference into a
product contract for AI-Intune. It is a design input, not a claim that
AI-Intune implements Microsoft Intune or Microsoft Graph.

## Source and audit

- Source: `\\SampleHub_Ridge_01\Live_Data\Intune.PDf`
- Mounted source: `/mnt/nas/Live_Data/intune.pdf` (read-only)
- PDF title: `intune | Microsoft Learn`
- Created: 2026-09-09
- Pages: 6,220
- SHA-256: `df22679d0ad6f0d6c7c5f1e09d3ece1de3a83c1b20f6f6c863efc5a382a10839`
- Full text audit: 6,220 page boundaries, 5,356,927 extracted characters
- Visual audit: all 817 pages containing a large raster interface image or
  architecture diagram were rendered into a page-numbered visual index;
  interface-critical pages were checked again at high resolution.

The PDF contains 836 article endings and screenshots from more than one Intune
generation. Page 34 shows the newer Intune home experience; pages 35–46 show
the durable navigation, dashboard, resource blade, detail-tab, support and
portal-setting patterns. Pages 4,819–4,973 cover reports. Pages 4,991–5,030
cover Copilot and natural-language exploration. Pages 5,039–5,099 cover
Security Copilot agents, recommendations, activities, setup and evidence.

## Information architecture

Use the localized Intune workload names when an AI-Intune capability has the
same operator meaning:

| Workload | AI-Intune destination | Delivered capability |
| --- | --- | --- |
| 儀表板 | `/` | Fleet status, findings, deployment summary |
| 裝置 | `/machines` | Roster, enrollment, lifecycle, monitor, updates |
| 應用程式 | `/apps` | Catalog, packages, profiles, assignments, deployments |
| 端點安全性 | `/settings/tailnet` | Verified operator/network boundary evidence |
| 代理程式 | `/jobs` | Agent work, lease, event and verification activity |
| 報告 | `/reports/changes` | Changes, ticket usage and audit records |
| 租用戶管理 | `/tenant/maintenance` | Retention, restore drills and service maintenance |
| 疑難排解 + 支援 | `/machines/diagnostics` | Device diagnostic preview and execution |

Do not advertise a workload merely to match Intune. Users, groups, compliance
policy and other Intune areas enter navigation only when their complete
AI-Intune workflows exist.

## Interaction contract

The shared shell uses a dark product bar, a persistent workload rail, and a
second vertical navigation rail for the selected workload. A resource page
uses a concise title, a working command bar and task-specific tabs.

Overview pages use clickable status tiles. A tile states the current count or
state and links to the filtered evidence behind it. Tables provide working
search/filter controls, explicit result counts, bounded pagination and row
links to resource details.

Report pages state their evaluated window and freshness, accept filters, and
use `產生報表` or `再次產生` for reads that require a fresh evaluation. Exported
columns retain the same meaning as the screen and the stable API.

Create and update flows use a stepped review. A destructive or broad action
must disclose scope before submission. Actions that cross the approval policy
remain pending until another authorized operator approves them.

Right-side panes are appropriate for bounded setup, filters, recommendation
details and confirmation. A full resource page remains the canonical place for
durable state and evidence.

The product bar keeps a discoverable language preference at the upper right.
It switches the shared admin-center chrome, workload rail, resource rail,
breadcrumb and page title between Traditional Chinese and English, persists
only in the browser, and returns to the exact local page. Evidence-rich page
content remains explicitly marked `zh-Hant` until its safety-critical status,
impact and next-step copy has been translated as a complete contract; a shell
preference must not mislabel untranslated evidence as English.

## Localized product terms

| Internal/current term | Product label |
| --- | --- |
| machine | 裝置 |
| machine roster | 裝置清單 |
| enrollment | 註冊裝置 |
| job queue | 代理程式活動 |
| job | 工作單 |
| deployment | 部署 |
| assignment | 指派 |
| profile | 設定檔 |
| artifact | 安裝套件 |
| fetch | 套件擷取 |
| operation | 作業 |
| changes | 變更 |
| ticket ledger | 票證使用量 |
| audit log | 稽核記錄 |
| troubleshooting | 疑難排解 |
| overview | 概觀 |
| properties | 屬性 |
| monitor | 監視 |
| settings | 設定 |
| recommendations | 建議 |
| activity | 活動 |
| knowledge source | 知識來源 |
| readiness check | 整備檢查 |

Protocol names, resource identifiers, channels and evidence fields remain
unchanged where translation would make operations ambiguous. Examples include
`machine_id`, `job_id`, OpenClaw, Tailnet, Canary, Stable and SHA-256.

## AI agent continuity

The Intune layout does not replace AI-Intune's agent model. It presents that
model through the localized `代理程式` workload:

- `概觀`: current availability and execution state.
- `建議`: a recommendation and its supporting factors; no recommendation is
  represented as verified fact.
- `活動`: job state, lease, event chronology and completion state.
- `驗證證據`: executor evidence, authority, provenance and evidence issues.
- `設定` or `知識來源`: only when a real persisted configuration or evidence
  source exists.
- `啟動代理程式`: only when the action is authorized and executable.

Enrollment, check-in, desired state, work leasing, event ingestion,
verification, approval, idempotency, audit, Web/CLI/API parity and persistence
remain product requirements. UI restructuring cannot bypass these boundaries.

## Evidence and safety differences from Intune

AI-Intune retains its own evidence semantics. `ok` or a completed job is not
renamed healthy or successful unless the required verifier says so. Agent
suggestions disclose their evidence and do not directly mutate a device.
Irreversible or fleet-wide actions retain preview, impact, approval,
idempotency and audit controls. Free-text agent evidence remains bounded and
safe for HTML, JSON, terminal and CSV consumers.
